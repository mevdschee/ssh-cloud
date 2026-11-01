// Package transfer moves file content between the local disk and the cloud,
// with hash based collision detection.
//
// Protocol summary (see PLAN.md):
//   - Every file may have a hidden hash file ".<name>.sha256" holding the
//     SHA-256 and size of its current content. Missing or invalid means
//     "unknown version".
//   - An upload goes to a hidden temp file first. Replacing an existing file
//     claims it by renaming it to ".<name>.claim-<random>"; only one client
//     can win that rename. Under the claim the hash file is compared with the
//     hash from this client's transfer list, then the temp file is renamed
//     into place with a rename that never overwrites.
//   - Anything that does not match is a conflict, reported as a
//     *ConflictError so the user can choose Replace, KeepBoth or Skip.
package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"sync"
	"time"

	"github.com/mevdschee/ssh-cloud/internal/remote"
	"github.com/mevdschee/ssh-cloud/internal/transferlog"
)

// Mode selects what an upload does with an existing, different version.
type Mode int

const (
	// Check uploads only if the server still has the version of this
	// client's last transfer; otherwise it returns a *ConflictError.
	Check Mode = iota
	// Replace overwrites whatever version is on the server (user's choice).
	Replace
	// KeepBoth uploads next to the original as "name (2).ext".
	KeepBoth
)

// Reason explains a conflict.
type Reason string

const (
	ChangedOnServer  Reason = "changed on the server since your last transfer"
	NeverTransferred Reason = "not transferred by you before"
	UnknownVersion   Reason = "version unknown (no hash file)"
	ChangedOutside   Reason = "changed outside SSH-Cloud"
	CreatedMeanwhile Reason = "created by someone else during your upload"
)

// ErrConflict matches every *ConflictError with errors.Is.
var ErrConflict = errors.New("conflict")

// ErrBusy is returned when a file stays claimed by another client too long.
var ErrBusy = errors.New("file is busy (claimed by another client)")

// ErrLocalChanged is returned when the local file changed during an upload.
var ErrLocalChanged = errors.New("local file changed during upload")

// ConflictError reports that an upload needs the user's decision.
type ConflictError struct {
	Path       string
	Reason     Reason
	ServerSize int64
	Last       *transferlog.Entry // this client's last transfer, if any
}

func (e *ConflictError) Error() string        { return fmt.Sprintf("%s: %s", e.Path, e.Reason) }
func (e *ConflictError) Is(target error) bool { return target == ErrConflict }

// Result describes a finished transfer.
type Result struct {
	Path    string // remote path actually written or read
	Size    int64
	Hash    string
	Outcome transferlog.Result
}

// Engine performs transfers for one connection and records them in the
// transfer list. It is safe for concurrent use.
type Engine struct {
	c   *remote.Client
	log *transferlog.Log

	// ClaimWait is how long to wait for a file claimed by another client
	// before treating the claim as left behind by a crash. Default 5s.
	ClaimWait time.Duration

	mu      sync.Mutex
	pending map[string]pendingTemp // remote path -> uploaded temp awaiting a decision

	beforeCommit func() // test hook: runs after the content is sent
}

type pendingTemp struct {
	tmp  string
	hash hashInfo
}

// New creates an engine.
func New(c *remote.Client, log *transferlog.Log) *Engine {
	return &Engine{c: c, log: log, pending: map[string]pendingTemp{}}
}

func (e *Engine) claimWait() time.Duration {
	if e.ClaimWait <= 0 {
		return 5 * time.Second
	}
	return e.ClaimWait
}

func validTarget(p string) error {
	if p == "" || remote.IsInternal(remote.Base(p)) {
		return fmt.Errorf("upload to %q: %w", p, remote.ErrInvalidPath)
	}
	return nil
}

// Upload uploads a local file to a remote path.
//
// In Check mode an unchanged file (the server already has exactly this
// content) is skipped without transfer, and a version that differs from this
// client's last transfer returns a *ConflictError. In that case nothing on the
// server changed; call Upload again with Replace or KeepBoth, or Skip. If the
// conflict was only discovered after the content was sent, the uploaded temp
// file is kept and reused by that next call, so nothing is sent twice.
func (e *Engine) Upload(ctx context.Context, localPath, remotePath string, mode Mode) (Result, error) {
	p := remote.Clean(remotePath)
	if err := validTarget(p); err != nil {
		return Result{}, err
	}
	local, err := hashLocal(localPath)
	if err != nil {
		return Result{}, err
	}
	entry := transferlog.Entry{Direction: transferlog.Upload, RemotePath: p, LocalPath: localPath, Size: local.Size, Hash: local.Hash}

	res, err := e.upload(ctx, localPath, p, mode, local)
	switch {
	case err == nil:
		entry.RemotePath, entry.Result = res.Path, res.Outcome
	case errors.Is(err, ErrConflict):
		entry.Result = transferlog.Conflict
	default:
		entry.Result, entry.Error = transferlog.Failed, err.Error()
	}
	if lerr := e.log.Add(entry); lerr != nil && err == nil {
		err = fmt.Errorf("record transfer: %w", lerr)
	}
	return res, err
}

func (e *Engine) upload(ctx context.Context, localPath, p string, mode Mode, local hashInfo) (Result, error) {
	if mode == Check {
		unchanged, err := e.precheck(ctx, p, local)
		if err != nil {
			return Result{}, err
		}
		if unchanged {
			e.dropPending(p)
			return Result{Path: p, Size: local.Size, Hash: local.Hash, Outcome: transferlog.Unchanged}, nil
		}
	}

	tmp, err := e.stage(ctx, localPath, p, local)
	if err != nil {
		return Result{}, err
	}
	if e.beforeCommit != nil {
		e.beforeCommit()
	}
	res, err := e.commit(ctx, p, tmp, mode, local)
	if errors.Is(err, ErrConflict) {
		return res, err // keep the temp for the user's decision
	}
	e.dropPending(p)
	if err != nil {
		_ = e.c.RawRemove(tmp)
		e.c.Prune(remote.Dir(p))
	}
	return res, err
}

// Skip discards an upload that ended in a conflict.
func (e *Engine) Skip(localPath, remotePath string) error {
	p := remote.Clean(remotePath)
	e.dropPending(p)
	e.c.Prune(remote.Dir(p))
	return e.log.Add(transferlog.Entry{Direction: transferlog.Upload, RemotePath: p, LocalPath: localPath, Result: transferlog.Skipped})
}

func (e *Engine) dropPending(p string) {
	e.mu.Lock()
	pt, ok := e.pending[p]
	delete(e.pending, p)
	e.mu.Unlock()
	if ok {
		_ = e.c.RawRemove(pt.tmp)
	}
}

// compare checks the server version of p against this client's last
// transfer. It returns "" when they match.
func (e *Engine) compare(p string, size int64) (Reason, *transferlog.Entry, error) {
	h, ok, err := e.readHash(p)
	if err != nil {
		return "", nil, err
	}
	last, known := e.log.Latest(p)
	var lastp *transferlog.Entry
	if known {
		lastp = &last
	}
	switch {
	case !ok:
		return UnknownVersion, lastp, nil
	case h.Size != size:
		return ChangedOutside, lastp, nil
	case !known:
		return NeverTransferred, nil, nil
	case h.Hash != last.Hash:
		return ChangedOnServer, lastp, nil
	}
	return "", lastp, nil
}

// precheck is the cheap check before any content is sent. It only reads
// metadata and the hash file.
func (e *Engine) precheck(ctx context.Context, p string, local hashInfo) (unchanged bool, err error) {
	fi, err := e.waitStat(ctx, p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil // new file
	}
	if err != nil {
		return false, err
	}
	if fi.IsDir() {
		return false, fmt.Errorf("upload to %q: is a folder: %w", p, remote.ErrExists)
	}
	if h, ok, err := e.readHash(p); err != nil {
		return false, err
	} else if ok && h.Hash == local.Hash && h.Size == fi.Size() {
		return true, nil
	}
	reason, last, err := e.compare(p, fi.Size())
	if err != nil {
		return false, err
	}
	if reason != "" {
		return false, &ConflictError{Path: p, Reason: reason, ServerSize: fi.Size(), Last: last}
	}
	return false, nil
}

// stage uploads the local file to a hidden temp file next to the target, or
// reuses one left by an earlier conflicting attempt with the same content.
func (e *Engine) stage(ctx context.Context, localPath, p string, local hashInfo) (string, error) {
	e.mu.Lock()
	pt, ok := e.pending[p]
	e.mu.Unlock()
	if ok {
		if pt.hash == local {
			if _, err := e.c.RawLstat(pt.tmp); err == nil {
				return pt.tmp, nil
			}
		}
		e.dropPending(p)
	}

	if err := e.c.MkdirAll(remote.Dir(p)); err != nil {
		return "", err
	}
	tmp := remote.Join(remote.Dir(p), remote.TempName(remote.Base(p)))
	sent, err := e.send(ctx, localPath, tmp)
	if err != nil {
		_ = e.c.RawRemove(tmp)
		return "", err
	}
	if sent != local {
		_ = e.c.RawRemove(tmp)
		return "", ErrLocalChanged
	}
	e.mu.Lock()
	e.pending[p] = pendingTemp{tmp: tmp, hash: local}
	e.mu.Unlock()
	return tmp, nil
}

// commit puts the staged temp file in place according to mode.
func (e *Engine) commit(ctx context.Context, p, tmp string, mode Mode, local hashInfo) (Result, error) {
	done := func(path string, outcome transferlog.Result) (Result, error) {
		e.forgetPending(p)
		if err := e.writeHash(path, local); err != nil {
			return Result{}, err
		}
		return Result{Path: path, Size: local.Size, Hash: local.Hash, Outcome: outcome}, nil
	}
	if mode == KeepBoth {
		path, err := e.keepBoth(tmp, p)
		if err != nil {
			return Result{}, err
		}
		return done(path, transferlog.KeptBoth)
	}

	deadline := time.Now().Add(e.claimWait())
	for {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if _, err := e.waitStat(ctx, p); errors.Is(err, fs.ErrNotExist) {
			// New file: a rename that never overwrites.
			err := e.renameNoReplace(tmp, p)
			if err == nil {
				return done(p, transferlog.Done)
			}
			if !errors.Is(err, remote.ErrExists) {
				return Result{}, err
			}
			if mode == Check {
				return Result{}, &ConflictError{Path: p, Reason: CreatedMeanwhile}
			}
			continue // Replace: claim the file that appeared
		} else if err != nil {
			return Result{}, err
		}

		claim := remote.Join(remote.Dir(p), remote.ClaimName(remote.Base(p)))
		err := e.c.RawClaim(p, claim)
		if errors.Is(err, remote.ErrNotFound) {
			// Another client claimed it first; wait for its new version.
			if time.Now().After(deadline) {
				return Result{}, ErrBusy
			}
			backoff()
			continue
		}
		if err != nil {
			return Result{}, err
		}
		return e.commitClaimed(p, tmp, claim, mode, local, done)
	}
}

// commitClaimed runs while this client holds the claim on p.
func (e *Engine) commitClaimed(p, tmp, claim string, mode Mode, local hashInfo, done func(string, transferlog.Result) (Result, error)) (Result, error) {
	outcome := transferlog.Done
	if mode == Check {
		fi, err := e.c.RawLstat(claim)
		if err != nil {
			return Result{}, err
		}
		reason, last, err := e.compare(p, fi.Size())
		if err != nil {
			e.restore(claim, p)
			return Result{}, err
		}
		if reason != "" {
			if reason == ChangedOutside {
				_ = e.removeHash(p) // stale: content unknown and not read by us
			}
			e.restore(claim, p)
			return Result{}, &ConflictError{Path: p, Reason: reason, ServerSize: fi.Size(), Last: last}
		}
	} else {
		outcome = transferlog.Replaced
	}

	// Delete the hash file before the new version goes in and write it after:
	// a crash in between leaves "unknown version", which always prompts.
	if err := e.removeHash(p); err != nil {
		e.restore(claim, p)
		return Result{}, err
	}
	if err := e.renameNoReplace(tmp, p); err != nil {
		if errors.Is(err, remote.ErrExists) {
			// Someone created p while we held the claim (only possible after
			// a crash recovery race). Keep the claimed version safe.
			e.preserve(claim, p)
			return Result{}, &ConflictError{Path: p, Reason: CreatedMeanwhile}
		}
		e.restore(claim, p)
		return Result{}, err
	}
	res, err := done(p, outcome)
	_ = e.c.RawRemove(claim)
	return res, err
}

// forgetPending drops the pending entry without deleting the temp file (it
// has been renamed into place).
func (e *Engine) forgetPending(p string) {
	e.mu.Lock()
	delete(e.pending, p)
	e.mu.Unlock()
}

// keepBoth renames the temp file to the first free "name (n).ext".
func (e *Engine) keepBoth(tmp, p string) (string, error) {
	for n := 2; n < 10000; n++ {
		cand := numbered(p, n)
		if _, err := e.c.RawLstat(cand); err == nil {
			continue
		}
		err := e.renameNoReplace(tmp, cand)
		if err == nil {
			return cand, nil
		}
		if !errors.Is(err, remote.ErrExists) {
			return "", err
		}
	}
	return "", fmt.Errorf("no free name for a copy of %q", p)
}

// restore renames a claim back to its file. If that name is taken meanwhile,
// the claimed version is preserved under a free name instead of being lost.
func (e *Engine) restore(claim, p string) {
	if err := e.renameNoReplace(claim, p); errors.Is(err, remote.ErrExists) {
		e.preserve(claim, p)
	}
}

func (e *Engine) preserve(claim, p string) {
	_, _ = e.keepBoth(claim, p)
}

// renameNoReplace renames without ever replacing the target. On servers whose
// plain rename replaces (not OpenSSH), an explicit check narrows the gap.
func (e *Engine) renameNoReplace(from, to string) error {
	replaces, err := e.c.RenameReplaces()
	if err != nil {
		return err
	}
	if replaces {
		if _, err := e.c.RawLstat(to); err == nil {
			return fmt.Errorf("rename to %q: %w", to, remote.ErrExists)
		}
	}
	return e.c.RawRename(from, to)
}

// waitStat stats p. While p is missing but claimed by another client it waits
// for the claim to end; a claim that outlives ClaimWait is treated as left by
// a crash and renamed back.
func (e *Engine) waitStat(ctx context.Context, p string) (os.FileInfo, error) {
	deadline := time.Now().Add(e.claimWait())
	for {
		fi, err := e.c.RawLstat(p)
		if !errors.Is(err, fs.ErrNotExist) {
			return fi, err
		}
		claims, cerr := e.claimsOf(p)
		if cerr != nil {
			return nil, cerr
		}
		if len(claims) == 0 {
			return nil, err
		}
		if time.Now().After(deadline) {
			e.restore(claims[0], p)
			deadline = time.Now().Add(e.claimWait())
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		backoff()
	}
}

// claimsOf lists the claim files of p.
func (e *Engine) claimsOf(p string) ([]string, error) {
	infos, err := e.c.RawReadDir(remote.Dir(p))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var claims []string
	for _, fi := range infos {
		if name, ok := remote.ClaimOf(fi.Name()); ok && name == remote.Base(p) {
			claims = append(claims, remote.Join(remote.Dir(p), fi.Name()))
		}
	}
	return claims, nil
}

// Recover renames claims in a namespace back to their file when the file is
// missing (a client crashed while holding the claim). It is always safe: a
// claimer that is still busy finds its own final rename refused and reports a
// conflict. It returns the number of restored files.
func (e *Engine) Recover(dir string) (int, error) {
	infos, err := e.c.RawReadDir(remote.Clean(dir))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, fi := range infos {
		name, ok := remote.ClaimOf(fi.Name())
		if !ok {
			continue
		}
		p := remote.Join(dir, name)
		if _, err := e.c.RawLstat(p); errors.Is(err, fs.ErrNotExist) {
			if e.renameNoReplace(remote.Join(dir, fi.Name()), p) == nil {
				n++
			}
		}
	}
	return n, nil
}

// Move moves a file or folder on the server and keeps the transfer list in
// step, so a later upload to the new path still matches.
func (e *Engine) Move(from, to string) error {
	from, to = remote.Clean(from), remote.Clean(to)
	if err := e.c.Move(from, to); err != nil {
		return err
	}
	return e.log.Moved(from, to)
}

func backoff() {
	time.Sleep(time.Duration(10+rand.IntN(40)) * time.Millisecond)
}

// ctxReader stops a copy when the context is cancelled.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (r ctxReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(b)
}

type ctxWriter struct {
	ctx context.Context
	w   io.Writer
}

func (w ctxWriter) Write(b []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.w.Write(b)
}
