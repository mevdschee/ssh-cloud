package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/mevdschee/ssh-cloud/internal/remote"
	"github.com/mevdschee/ssh-cloud/internal/transferlog"
)

// send streams a local file into a new remote file, hashing what it sends.
func (e *Engine) send(ctx context.Context, localPath, remotePath string) (hashInfo, error) {
	src, err := os.Open(localPath)
	if err != nil {
		return hashInfo{}, err
	}
	defer src.Close()
	dst, err := e.c.RawCreate(remotePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return hashInfo{}, fmt.Errorf("create %q: %w", remotePath, err)
	}
	h := sha256.New()
	n, err := dst.ReadFromWithConcurrency(io.TeeReader(&countingReader{ctx: ctx, r: src, fn: progressOf(ctx)}, h), 0)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return hashInfo{}, fmt.Errorf("upload %s: %w", localPath, err)
	}
	return hashInfo{Hash: hex.EncodeToString(h.Sum(nil)), Size: n}, nil
}

// Download copies a remote file to a local path. The local file is written
// to a temp file next to it and renamed into place when complete, replacing
// an existing local file. The hash of the bytes actually read is recorded in
// the transfer list; a hash file on the server that disagrees is repaired.
func (e *Engine) Download(ctx context.Context, remotePath, localPath string) (Result, error) {
	p := remote.Clean(remotePath)
	res, err := e.download(ctx, p, localPath)
	entry := transferlog.Entry{Direction: transferlog.Download, RemotePath: p, LocalPath: localPath, Size: res.Size, Hash: res.Hash, Result: transferlog.Done}
	if err != nil {
		entry.Result, entry.Error, entry.Hash = transferlog.Failed, err.Error(), ""
		slog.Error("download failed", "remote", p, "local", localPath, "err", err)
	} else {
		slog.Info("download done", "remote", p, "local", localPath, "size", res.Size, "sha256", res.Hash)
	}
	if lerr := e.log.Add(entry); lerr != nil && err == nil {
		err = fmt.Errorf("record transfer: %w", lerr)
	}
	return res, err
}

func (e *Engine) download(ctx context.Context, p, localPath string) (Result, error) {
	if p == "" || remote.IsInternal(remote.Base(p)) {
		return Result{}, fmt.Errorf("download %q: %w", p, remote.ErrInvalidPath)
	}
	// Read the hash file before the content: if an upload replaces the file
	// in between, the hash we compare with is older, never newer, than what
	// we read, so a mismatch is detected rather than missed.
	before, beforeOK, err := e.readHash(p)
	if err != nil {
		return Result{}, err
	}

	// Wait out a claim by an uploader (the file is briefly missing then).
	if _, err := e.waitStat(ctx, p); err != nil {
		return Result{}, fmt.Errorf("download %q: %w", p, err)
	}
	src, err := e.c.RawOpen(p)
	if err != nil {
		return Result{}, fmt.Errorf("download %q: %w", p, err)
	}
	defer src.Close()
	if fi, err := src.Stat(); err == nil && fi.IsDir() {
		return Result{}, fmt.Errorf("download %q: is a folder", p)
	}

	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return Result{}, err
	}
	part := localPath + ".sshcloud-part"
	dst, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return Result{}, err
	}
	h := sha256.New()
	n, err := src.WriteTo(&countingWriter{ctx: ctx, w: io.MultiWriter(dst, h), fn: progressOf(ctx)})
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(part)
		return Result{}, fmt.Errorf("download %q: %w", p, err)
	}
	if err := os.Rename(part, localPath); err != nil {
		os.Remove(part)
		return Result{}, err
	}
	read := hashInfo{Hash: hex.EncodeToString(h.Sum(nil)), Size: n}

	if !beforeOK || before != read {
		slog.Info("hash file out of sync, repairing", "path", p, "hashFileValid", beforeOK, "hashFile", before.Hash, "read", read.Hash)
		e.repairHash(p, before, beforeOK, read)
	}
	return Result{Path: p, Size: read.Size, Hash: read.Hash, Outcome: transferlog.Done}, nil
}

// repairHash fixes an out-of-sync hash file after a download. It claims the
// file and only writes the hash of what was read if the hash file is still
// the same stale one and the size still matches; a hash file that changed
// meanwhile was written by an uploader and is fresh. Repair is best effort:
// when the file is busy it is skipped (the next upload then prompts, which is
// the safe side).
func (e *Engine) repairHash(p string, stale hashInfo, staleOK bool, read hashInfo) {
	claim := remote.Join(remote.Dir(p), remote.ClaimName(remote.Base(p)))
	if err := e.c.RawClaim(p, claim); err != nil {
		slog.Debug("hash repair skipped, file busy", "path", p, "err", err)
		return
	}
	defer e.restore(claim, p)

	fi, err := e.c.RawLstat(claim)
	if err != nil || fi.Size() != read.Size {
		return
	}
	now, nowOK, err := e.readHash(p)
	if err != nil || nowOK != staleOK || now != stale {
		return
	}
	err = e.writeHash(p, read)
	slog.Info("hash file repaired", "path", p, "err", err)
}
