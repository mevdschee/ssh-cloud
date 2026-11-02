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
	"time"

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
// an existing local file. The hash of the bytes actually read and the server
// version they belong to are recorded in the transfer list. A download never
// writes to the server.
func (e *Engine) Download(ctx context.Context, remotePath, localPath string) (Result, error) {
	p := remote.Clean(remotePath)
	res, err := e.download(ctx, p, localPath)
	entry := transferlog.Entry{Direction: transferlog.Download, RemotePath: p, LocalPath: localPath, Size: res.Size, Hash: res.Hash, Mtime: unix(res.ModTime),
		Result: transferlog.Done, DragOut: isDragOut(ctx)}
	if err != nil {
		entry.Result, entry.Error, entry.Hash, entry.Mtime = transferlog.Failed, err.Error(), "", 0
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
	// Wait out a claim by an uploader (the file is briefly missing then).
	if _, err := e.waitStat(ctx, p); err != nil {
		return Result{}, fmt.Errorf("download %q: %w", p, err)
	}
	src, err := e.c.RawOpen(p)
	if err != nil {
		return Result{}, fmt.Errorf("download %q: %w", p, err)
	}
	defer src.Close()
	// Stat the open handle, before and after reading: it keeps reading the
	// version it opened even when an upload replaces the file meanwhile.
	fi, err := src.Stat()
	if err != nil {
		return Result{}, fmt.Errorf("download %q: %w", p, err)
	}
	if fi.IsDir() {
		return Result{}, fmt.Errorf("download %q: is a folder", p)
	}
	before := versionOf(fi)

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
	res := Result{Path: p, Size: n, Hash: hex.EncodeToString(h.Sum(nil)), Outcome: transferlog.Done}

	// A file changed in place while it was read (by a tool outside the app)
	// has no single version: record it as unknown, so the next upload asks.
	after, err := src.Stat()
	if err == nil && versionOf(after) == before && before.Size == n {
		res.ModTime = time.Unix(before.Mtime, 0)
	} else {
		slog.Warn("file changed during download, version unknown", "path", p)
	}
	return res, nil
}
