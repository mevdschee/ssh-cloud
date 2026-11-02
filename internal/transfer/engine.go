// Package transfer moves file content between the local disk and the cloud,
// with version based collision detection.
//
// Protocol summary (see PLAN.md):
//   - A version of a file on the server is its size and the server's mtime.
//     The transfer list records the version of every download and upload.
//   - An upload goes to a hidden temp file first. Replacing an existing file
//     claims it by renaming it to ".<name>.claim-<random>"; only one client
//     can win that rename. Under the claim the claimed file's version is
//     compared with this client's transfer list, the temp file gets a later
//     mtime than the claimed one if needed, and it is renamed into place with
//     a rename that never overwrites.
//   - Anything that does not match is a conflict, reported as a
//     *ConflictError so the user can choose Replace, KeepBoth or Skip.
package transfer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
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
	UnknownVersion   Reason = "version unknown"
	CreatedMeanwhile Reason = "created by someone else during your upload"
	OtherLocalFile   Reason = "already on the server, last transferred with another local file"
	MissingMeanwhile Reason = "deleted, moved or being replaced by someone else during your upload"
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
	ModTime time.Time // server mtime of the version; zero when unknown
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

	// copyData copies file content on the server (remote.Client.RawCopyData;
	// replaced in tests, whose server has no copy-data extension).
	copyData func(ctx context.Context, from, to string) error
	canCopy  func() bool
}

type pendingTemp struct {
	tmp  string
	hash hashInfo
}

// New creates an engine.
func New(c *remote.Client, log *transferlog.Log) *Engine {
	return &Engine{c: c, log: log, pending: map[string]pendingTemp{},
		copyData: c.RawCopyData, canCopy: c.CanCopy}
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

	slog.Debug("upload", "local", localPath, "remote", p, "mode", mode, "size", local.Size, "sha256", local.Hash)
	res, err := e.upload(ctx, localPath, p, mode, local)
	switch {
	case err == nil:
		slog.Info("upload done", "remote", res.Path, "outcome", res.Outcome, "size", res.Size)
	case errors.Is(err, ErrConflict):
		slog.Info("upload conflict", "remote", p, "err", err)
	default:
		slog.Error("upload failed", "local", localPath, "remote", p, "err", err)
	}
	switch {
	case err == nil:
		entry.RemotePath, entry.Result, entry.Mtime = res.Path, res.Outcome, unix(res.ModTime)
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
	existed := false // the target existed when the upload started
	if mode == Check {
		unchanged, exists, err := e.precheck(ctx, p, local, localPath)
		if err != nil {
			return Result{}, err
		}
		existed = exists
		if unchanged != nil {
			e.dropPending(p)
			return Result{Path: p, Size: local.Size, Hash: local.Hash, ModTime: time.Unix(unchanged.Mtime, 0), Outcome: transferlog.Unchanged}, nil
		}
	}

	tmp, err := e.stage(ctx, localPath, p, local)
	if err != nil {
		return Result{}, err
	}
	if e.beforeCommit != nil {
		e.beforeCommit()
	}
	res, err := e.commit(ctx, p, tmp, mode, local, existed)
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
	slog.Info("upload skipped", "local", localPath, "remote", p)
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

// compare checks the server version v of p against this client's last
// transfer. It returns "" when they match.
func (e *Engine) compare(p string, v version) (Reason, *transferlog.Entry) {
	last, known := e.log.Latest(p)
	if !known {
		return NeverTransferred, nil
	}
	rec, ok := recorded(last)
	switch {
	case !ok:
		return UnknownVersion, &last
	case rec != v:
		return ChangedOnServer, &last
	}
	return "", &last
}

// precheck is the cheap check before any content is sent. It only reads
// metadata. unchanged is the server version when it already has exactly the
// local content (as this client's last transfer of it).
func (e *Engine) precheck(ctx context.Context, p string, local hashInfo, localPath string) (unchanged *version, exists bool, err error) {
	fi, err := e.waitStat(ctx, p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil // new file
	}
	if err != nil {
		return nil, false, err
	}
	if fi.IsDir() {
		return nil, true, fmt.Errorf("upload to %q: is a folder: %w", p, remote.ErrExists)
	}
	v := versionOf(fi)
	reason, last := e.compare(p, v)
	if reason != "" {
		return nil, true, &ConflictError{Path: p, Reason: reason, ServerSize: fi.Size(), Last: last}
	}
	if last.Hash == local.Hash {
		return &v, true, nil
	}
	// Nobody changed the file since this client's last transfer. Replace it
	// without asking only when the upload comes from that same local file
	// (the drag out, edit, drag back case); another local file with the same
	// name may well be a different document, so ask.
	if !SameLocalFile(*last, localPath) {
		return nil, true, &ConflictError{Path: p, Reason: OtherLocalFile, ServerSize: fi.Size(), Last: last}
	}
	return nil, true, nil
}

// SameLocalFile reports whether localPath is the local file of a transfer:
// the same path (case-insensitive, as on Windows), or for a drag out, whose
// recorded path is a temp file, a file with the same name.
func SameLocalFile(e transferlog.Entry, localPath string) bool {
	if e.LocalPath == "" || localPath == "" {
		return false
	}
	if strings.EqualFold(filepath.Clean(e.LocalPath), filepath.Clean(localPath)) {
		return true
	}
	return e.DragOut && strings.EqualFold(filepath.Base(e.LocalPath), filepath.Base(localPath))
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
// existed says the target existed when a Check upload started. Such an upload
// never takes the new-file path: a missing target then means it was deleted,
// moved, or is claimed by another client right now (a folder listing cannot
// reliably show the claim), so creating it anew could silently replace the
// version inside that claim.
func (e *Engine) commit(ctx context.Context, p, tmp string, mode Mode, local hashInfo, existed bool) (Result, error) {
	done := func(path string, v version, outcome transferlog.Result) (Result, error) {
		e.forgetPending(p)
		return Result{Path: path, Size: local.Size, Hash: local.Hash, ModTime: time.Unix(v.Mtime, 0), Outcome: outcome}, nil
	}
	if mode == KeepBoth {
		v, err := e.tempVersion(tmp)
		if err != nil {
			return Result{}, err
		}
		path, err := e.keepBoth(tmp, p)
		if err != nil {
			return Result{}, err
		}
		return done(path, v, transferlog.KeptBoth)
	}

	deadline := time.Now().Add(e.claimWait())
	for {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if _, err := e.waitStat(ctx, p); errors.Is(err, fs.ErrNotExist) {
			if mode == Check && existed {
				return Result{}, &ConflictError{Path: p, Reason: MissingMeanwhile}
			}
			// New file: a rename that never overwrites.
			v, err := e.tempVersion(tmp)
			if err != nil {
				return Result{}, err
			}
			err = e.renameNoReplace(tmp, p)
			if err == nil {
				return done(p, v, transferlog.Done)
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
		if mode == Check && !existed {
			// Started as a new file, but someone created it meanwhile. There is
			// no version of ours to compare with, so do not claim it: ask.
			return Result{}, &ConflictError{Path: p, Reason: CreatedMeanwhile}
		}

		claim := remote.Join(remote.Dir(p), remote.ClaimName(remote.Base(p)))
		err := e.c.RawClaim(p, claim)
		if errors.Is(err, remote.ErrNotFound) {
			// Another client claimed it first; wait for its new version.
			if time.Now().After(deadline) {
				slog.Warn("file busy, giving up", "path", p)
				return Result{}, ErrBusy
			}
			slog.Debug("claim lost to another client, waiting", "path", p)
			backoff()
			continue
		}
		if err != nil {
			return Result{}, err
		}
		return e.commitClaimed(p, tmp, claim, mode, done)
	}
}

// commitClaimed runs while this client holds the claim on p.
func (e *Engine) commitClaimed(p, tmp, claim string, mode Mode, done func(string, version, transferlog.Result) (Result, error)) (Result, error) {
	fi, err := e.c.RawLstat(claim)
	if err != nil {
		e.restore(claim, p)
		return Result{}, err
	}
	claimed := versionOf(fi)
	outcome := transferlog.Done
	if mode == Check {
		if reason, last := e.compare(p, claimed); reason != "" {
			e.restore(claim, p)
			return Result{}, &ConflictError{Path: p, Reason: reason, ServerSize: fi.Size(), Last: last}
		}
	} else {
		outcome = transferlog.Replaced
	}

	v, err := e.later(tmp, claimed)
	if err != nil {
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
	res, err := done(p, v, outcome)
	_ = e.c.RawRemove(claim)
	return res, err
}

// tempVersion returns the version of a staged temp file, which the final
// rename keeps.
func (e *Engine) tempVersion(tmp string) (version, error) {
	fi, err := e.c.RawLstat(tmp)
	if err != nil {
		return version{}, err
	}
	return versionOf(fi), nil
}

// unix returns t in Unix seconds, or 0 for the zero time.
func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
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
	err := e.renameNoReplace(claim, p)
	if errors.Is(err, remote.ErrExists) {
		e.preserve(claim, p)
		return
	}
	if err != nil {
		slog.Error("could not restore claimed file", "claim", claim, "path", p, "err", err)
	}
}

func (e *Engine) preserve(claim, p string) {
	saved, err := e.keepBoth(claim, p)
	slog.Warn("claimed version preserved under a new name", "path", p, "savedAs", saved, "err", err)
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
			slog.Warn("claim outlived the wait, treating it as left by a crash", "claim", claims[0], "path", p)
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

// claimsOf lists the claim files of p. A listing is not an atomic snapshot
// and can miss a claim made during it; callers must stay safe when that
// happens (see commit).
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
				slog.Info("recovered claimed file", "path", p, "claim", fi.Name())
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

// CanCopy reports whether server-side copy is available.
func (e *Engine) CanCopy() bool { return e.canCopy() }

// Copy copies a file on the server to a new path without moving the content
// through the client. The target must not exist (ErrExists). The copy goes to
// a temp file first and is renamed into place with a rename that never
// overwrites. The copy is a new file with a version of its own.
func (e *Engine) Copy(ctx context.Context, from, to string) error {
	from, to = remote.Clean(from), remote.Clean(to)
	if err := validTarget(to); err != nil {
		return err
	}
	if !e.canCopy() {
		return remote.ErrCopyUnsupported
	}
	if _, err := e.c.RawLstat(to); err == nil {
		return fmt.Errorf("copy to %q: %w", to, remote.ErrExists)
	}
	if err := e.c.MkdirAll(remote.Dir(to)); err != nil {
		return err
	}
	tmp := remote.Join(remote.Dir(to), remote.TempName(remote.Base(to)))
	if err := e.copyData(ctx, from, tmp); err != nil {
		_ = e.c.RawRemove(tmp)
		e.c.Prune(remote.Dir(to))
		return err
	}
	fi, err := e.c.RawLstat(tmp)
	if err != nil {
		_ = e.c.RawRemove(tmp)
		return err
	}
	if err := e.renameNoReplace(tmp, to); err != nil {
		_ = e.c.RawRemove(tmp)
		return err
	}
	slog.Info("copy done", "from", from, "to", to, "size", fi.Size())
	return nil
}

// SetCopyFunc replaces the server-side copy primitive. It is meant for tests
// whose server lacks the copy-data extension.
func (e *Engine) SetCopyFunc(fn func(ctx context.Context, from, to string) error) {
	e.copyData, e.canCopy = fn, func() bool { return true }
}

// ServerChanged compares a file on the server, by the size and mtime from a
// listing, with this client's last transfer of it. known is false when this
// client never transferred the file.
func (e *Engine) ServerChanged(p string, size int64, mtime time.Time) (changed, known bool) {
	p = remote.Clean(p)
	if _, ok := e.log.Latest(p); !ok {
		return false, false
	}
	reason, _ := e.compare(p, version{Size: size, Mtime: mtime.Unix()})
	return reason != "", true
}
