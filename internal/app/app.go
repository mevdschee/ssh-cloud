// Package app is the window-independent part of the SSH-Cloud explorer: it
// owns the connection, lists namespaces (including virtual folders that only
// exist in the view until a file is uploaded into them) and runs uploads and
// downloads of whole selections, asking a callback when a collision needs the
// user's decision. The windigo UI is a thin layer on top.
package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/mevdschee/ssh-cloud/internal/dragdata"
	"github.com/mevdschee/ssh-cloud/internal/remote"
	"github.com/mevdschee/ssh-cloud/internal/transfer"
	"github.com/mevdschee/ssh-cloud/internal/transferlog"
)

// App is one open connection profile.
type App struct {
	cfg remote.Config
	log *transferlog.Log

	mu      sync.Mutex
	client  *remote.Client
	engine  *transfer.Engine
	virtual map[string]bool // virtual folders by cloud path
}

// Open connects and opens the transfer list.
func Open(ctx context.Context, cfg remote.Config, transferLogFile string) (*App, error) {
	log, err := transferlog.Open(transferLogFile)
	if err != nil {
		return nil, err
	}
	a := &App{cfg: cfg, log: log, virtual: map[string]bool{}}
	if _, _, err := a.conn(ctx); err != nil {
		log.Close()
		return nil, err
	}
	return a, nil
}

// Close closes the connection and the transfer list.
func (a *App) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client != nil {
		a.client.Close()
	}
	a.log.Close()
}

// Log returns the transfer list.
func (a *App) Log() *transferlog.Log { return a.log }

// Connected reports whether the connection is currently up.
func (a *App) Connected() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.client != nil && a.client.Err() == nil
}

// conn returns a live connection, reconnecting if it was lost.
func (a *App) conn(ctx context.Context) (*remote.Client, *transfer.Engine, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client != nil && a.client.Err() == nil {
		return a.client, a.engine, nil
	}
	if a.client != nil {
		slog.Info("reconnecting", "reason", a.client.Err())
	}
	c, err := remote.Dial(ctx, a.cfg)
	if err != nil {
		return nil, nil, err
	}
	a.client, a.engine = c, transfer.New(c, a.log)
	return c, a.engine, nil
}

// List lists a namespace. Virtual folders are merged in; a virtual folder
// that does not exist on the server lists as empty.
func (a *App) List(ctx context.Context, dir string) ([]remote.Entry, error) {
	dir = remote.Clean(dir)
	c, _, err := a.conn(ctx)
	if err != nil {
		return nil, err
	}
	entries, err := c.List(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && a.isVirtual(dir) {
			entries = nil
		} else {
			return nil, err
		}
	}

	a.mu.Lock()
	present := map[string]bool{}
	for _, e := range entries {
		present[e.Path] = true
		delete(a.virtual, e.Path) // it is real now
	}
	for p := range a.virtual {
		if remote.Dir(p) == dir && !present[p] {
			entries = append(entries, remote.Entry{Path: p, Name: remote.Base(p), IsDir: true})
		}
	}
	a.mu.Unlock()

	sort.SliceStable(entries, func(i, j int) bool {
		x, y := entries[i], entries[j]
		if x.IsDir != y.IsDir {
			return x.IsDir
		}
		return strings.ToLower(x.Name) < strings.ToLower(y.Name)
	})
	return entries, nil
}

// IsVirtual reports whether a folder exists only in the view.
func (a *App) IsVirtual(p string) bool { return a.isVirtual(remote.Clean(p)) }

func (a *App) isVirtual(p string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for v := range a.virtual {
		if v == p || strings.HasPrefix(v, p+"/") {
			return true
		}
	}
	return false
}

// NewFolder creates a virtual folder in dir. It becomes real when the first
// file is uploaded into it. It returns the folder's cloud path.
func (a *App) NewFolder(ctx context.Context, dir, name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || remote.IsInternal(name) {
		return "", fmt.Errorf("folder name %q: %w", name, remote.ErrInvalidPath)
	}
	p := remote.Join(dir, name)
	entries, err := a.List(ctx, dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if strings.EqualFold(e.Name, name) {
			return "", fmt.Errorf("%q: %w", p, remote.ErrExists)
		}
	}
	a.mu.Lock()
	a.virtual[p] = true
	a.mu.Unlock()
	return p, nil
}

// Rename renames a file or folder within its namespace.
func (a *App) Rename(ctx context.Context, p, newName string) (string, error) {
	if newName == "" || strings.ContainsAny(newName, `/\`) {
		return "", fmt.Errorf("rename to %q: %w", newName, remote.ErrInvalidPath)
	}
	to := remote.Join(remote.Dir(p), newName)
	return to, a.Move(ctx, p, to)
}

// Move moves a file or folder. Virtual folders are moved in the view.
func (a *App) Move(ctx context.Context, from, to string) error {
	from, to = remote.Clean(from), remote.Clean(to)
	if a.moveVirtual(from, to) {
		return nil
	}
	_, e, err := a.conn(ctx)
	if err != nil {
		return err
	}
	return e.Move(from, to)
}

func (a *App) moveVirtual(from, to string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.virtual[from] {
		return false
	}
	for v := range a.virtual {
		if v == from || strings.HasPrefix(v, from+"/") {
			delete(a.virtual, v)
			a.virtual[to+v[len(from):]] = true
		}
	}
	return true
}

// Delete deletes files and folders (folders with everything in them).
func (a *App) Delete(ctx context.Context, paths []string, progress Progress) error {
	var errs []error
	for i, p := range paths {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		p = remote.Clean(p)
		if progress != nil {
			progress(ProgressInfo{Action: "Deleting", File: p, FileIndex: i, FileCount: len(paths)})
		}
		if a.deleteVirtual(p) {
			continue
		}
		c, _, err := a.conn(ctx)
		if err != nil {
			return err
		}
		if err := c.Delete(p); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (a *App) deleteVirtual(p string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	found := false
	for v := range a.virtual {
		if v == p || strings.HasPrefix(v, p+"/") {
			delete(a.virtual, v)
			found = true
		}
	}
	return found
}

// Choice is the user's answer to a collision.
type Choice int

const (
	Replace Choice = iota
	KeepBoth
	Skip
	Cancel // stop the whole operation
)

// Decider is asked for every collision. When applyAll is true, the choice is
// used for all remaining collisions of the same operation. It is called from
// the worker goroutine and may block while the user decides.
type Decider func(c *transfer.ConflictError, localPath string) (choice Choice, applyAll bool)

// OverwriteLocal is asked before a download replaces an existing local file.
type OverwriteLocal func(localPath string) (overwrite bool, applyAll bool)

// ProgressInfo describes the state of a running multi-file operation.
type ProgressInfo struct {
	Action     string // "Uploading", "Downloading", "Copying", "Deleting"
	File       string // current file (cloud path)
	FileIndex  int    // zero-based index of the current file
	FileCount  int
	Bytes      int64 // bytes done over all files
	TotalBytes int64
}

// Progress receives progress updates from the worker goroutine. It can be
// called often; the receiver should throttle.
type Progress func(ProgressInfo)

// Summary counts the outcome of a multi-file operation.
type Summary struct {
	Done, Unchanged, Replaced, KeptBoth, Skipped int
	Errors                                       []error
	Cancelled                                    bool
}

func (s Summary) String() string {
	var parts []string
	add := func(n int, what string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, what))
		}
	}
	add(s.Done, "done")
	add(s.Unchanged, "unchanged")
	add(s.Replaced, "replaced")
	add(s.KeptBoth, "kept both")
	add(s.Skipped, "skipped")
	add(len(s.Errors), "failed")
	if s.Cancelled {
		parts = append(parts, "cancelled")
	}
	if len(parts) == 0 {
		return "nothing to do"
	}
	return strings.Join(parts, ", ")
}

// tracker turns per-file byte progress into ProgressInfo for the whole job.
type tracker struct {
	fn    Progress
	info  ProgressInfo
	start int64 // bytes done before the current file
}

func newTracker(fn Progress, action string, count int, total int64) *tracker {
	return &tracker{fn: fn, info: ProgressInfo{Action: action, FileCount: count, TotalBytes: total}}
}

// file starts file i and returns a context that reports its byte progress.
func (t *tracker) file(ctx context.Context, i int, name string) context.Context {
	t.start = t.info.Bytes
	t.info.FileIndex, t.info.File = i, name
	if t.fn == nil {
		return ctx
	}
	t.fn(t.info)
	return transfer.WithProgress(ctx, func(done int64) {
		t.info.Bytes = t.start + done
		t.fn(t.info)
	})
}

// fileDone marks the current file as complete with its full size.
func (t *tracker) fileDone(size int64) {
	t.info.Bytes = t.start + size
	if t.fn != nil {
		t.fn(t.info)
	}
}

type upload struct {
	local, remote string
	size          int64
}

// Upload uploads local files and folders into a namespace. Folders are
// uploaded recursively; empty local folders are ignored (folders do not exist
// on their own).
func (a *App) Upload(ctx context.Context, localPaths []string, dir string, decide Decider, progress Progress) Summary {
	var s Summary
	var jobs []upload
	var total int64
	for _, lp := range localPaths {
		fi, err := os.Stat(lp)
		if err != nil {
			s.Errors = append(s.Errors, err)
			continue
		}
		if !fi.IsDir() {
			jobs = append(jobs, upload{lp, remote.Join(dir, filepath.Base(lp)), fi.Size()})
			total += fi.Size()
			continue
		}
		base := filepath.Dir(lp)
		err = filepath.WalkDir(lp, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type().IsRegular() {
				rel, _ := filepath.Rel(base, p)
				var size int64
				if info, err := d.Info(); err == nil {
					size = info.Size()
				}
				jobs = append(jobs, upload{p, remote.Join(dir, filepath.ToSlash(rel)), size})
				total += size
			}
			return nil
		})
		if err != nil {
			s.Errors = append(s.Errors, err)
		}
	}

	t := newTracker(progress, "Uploading", len(jobs), total)
	var all *Choice
	for i, j := range jobs {
		if ctx.Err() != nil {
			s.Cancelled = true
			break
		}
		fctx := t.file(ctx, i, j.remote)
		_, e, err := a.conn(ctx)
		if err != nil {
			s.Errors = append(s.Errors, err)
			break
		}
		res, err := e.Upload(fctx, j.local, j.remote, transfer.Check)
		var ce *transfer.ConflictError
		if errors.As(err, &ce) {
			choice := Skip
			if all != nil {
				choice = *all
			} else if decide != nil {
				var applyAll bool
				choice, applyAll = decide(ce, j.local)
				if applyAll {
					all = &choice
				}
			}
			switch choice {
			case Replace:
				res, err = e.Upload(fctx, j.local, j.remote, transfer.Replace)
			case KeepBoth:
				res, err = e.Upload(fctx, j.local, j.remote, transfer.KeepBoth)
			case Skip, Cancel:
				err = e.Skip(j.local, j.remote)
				if err == nil {
					s.Skipped++
				}
				if choice == Cancel {
					s.Cancelled = true
				}
			}
			if choice == Skip || choice == Cancel {
				t.fileDone(j.size)
				if err != nil {
					s.Errors = append(s.Errors, err)
				}
				if s.Cancelled {
					break
				}
				continue
			}
		}
		t.fileDone(j.size)
		if err != nil {
			if ctx.Err() != nil {
				s.Cancelled = true
				break
			}
			s.Errors = append(s.Errors, err)
			continue
		}
		switch res.Outcome {
		case transferlog.Unchanged:
			s.Unchanged++
		case transferlog.Replaced:
			s.Replaced++
		case transferlog.KeptBoth:
			s.KeptBoth++
		default:
			s.Done++
		}
	}
	return s
}

type download struct {
	remote, local string
	size          int64
}

// Download downloads files and folders into a local folder. Folders are
// downloaded recursively.
func (a *App) Download(ctx context.Context, remotePaths []string, localDir string, overwrite OverwriteLocal, progress Progress) Summary {
	var s Summary
	var jobs []download
	var total int64
	for _, rp := range remotePaths {
		rp = remote.Clean(rp)
		c, _, err := a.conn(ctx)
		if err != nil {
			s.Errors = append(s.Errors, err)
			return s
		}
		e, err := c.Stat(rp)
		if err != nil {
			s.Errors = append(s.Errors, err)
			continue
		}
		if !e.IsDir {
			jobs = append(jobs, download{rp, filepath.Join(localDir, e.Name), e.Size})
			total += e.Size
			continue
		}
		if err := a.walk(ctx, rp, func(f remote.Entry) {
			rel := strings.TrimPrefix(f.Path, remote.Dir(rp)+"/")
			if remote.Dir(rp) == "" {
				rel = f.Path
			}
			jobs = append(jobs, download{f.Path, filepath.Join(localDir, filepath.FromSlash(rel)), f.Size})
			total += f.Size
		}); err != nil {
			s.Errors = append(s.Errors, err)
		}
	}

	t := newTracker(progress, "Downloading", len(jobs), total)
	var all *bool
	for i, j := range jobs {
		if ctx.Err() != nil {
			s.Cancelled = true
			break
		}
		fctx := t.file(ctx, i, j.remote)
		if _, err := os.Stat(j.local); err == nil {
			ok := false
			if all != nil {
				ok = *all
			} else if overwrite != nil {
				var applyAll bool
				ok, applyAll = overwrite(j.local)
				if applyAll {
					all = &ok
				}
			}
			if !ok {
				s.Skipped++
				t.fileDone(j.size)
				continue
			}
		}
		_, e, err := a.conn(ctx)
		if err != nil {
			s.Errors = append(s.Errors, err)
			break
		}
		_, err = e.Download(fctx, j.remote, j.local)
		t.fileDone(j.size)
		if err != nil {
			if ctx.Err() != nil {
				s.Cancelled = true
				break
			}
			s.Errors = append(s.Errors, err)
			continue
		}
		s.Done++
	}
	return s
}

// CanCopy reports whether the server supports server-side copy.
func (a *App) CanCopy(ctx context.Context) bool {
	_, e, err := a.conn(ctx)
	return err == nil && e.CanCopy()
}

type copyJob struct {
	from, to string
	size     int64
}

// Copy copies files and folders into a namespace on the server, without
// moving content through the client. A name that is taken gets " - Copy"
// (then " - Copy (2)", ...), like Windows Explorer. Copying a folder into
// itself is refused.
func (a *App) Copy(ctx context.Context, entries []remote.Entry, dir string, progress Progress) Summary {
	var s Summary
	dir = remote.Clean(dir)
	_, e, err := a.conn(ctx)
	if err != nil {
		s.Errors = append(s.Errors, err)
		return s
	}
	if !e.CanCopy() {
		s.Errors = append(s.Errors, remote.ErrCopyUnsupported)
		return s
	}
	existing, err := a.List(ctx, dir)
	if err != nil {
		s.Errors = append(s.Errors, err)
		return s
	}
	taken := map[string]bool{}
	for _, x := range existing {
		taken[strings.ToLower(x.Name)] = true
	}

	var jobs []copyJob
	var total int64
	for _, src := range entries {
		if src.IsDir && (dir == src.Path || strings.HasPrefix(dir, src.Path+"/")) {
			s.Errors = append(s.Errors, fmt.Errorf("cannot copy %q into itself", src.Path))
			continue
		}
		name := freeName(src.Name, src.IsDir, taken)
		taken[strings.ToLower(name)] = true
		target := remote.Join(dir, name)
		if !src.IsDir {
			jobs = append(jobs, copyJob{src.Path, target, src.Size})
			total += src.Size
			continue
		}
		if err := a.walk(ctx, src.Path, func(f remote.Entry) {
			jobs = append(jobs, copyJob{f.Path, target + strings.TrimPrefix(f.Path, src.Path), f.Size})
			total += f.Size
		}); err != nil {
			s.Errors = append(s.Errors, err)
		}
	}

	t := newTracker(progress, "Copying", len(jobs), total)
	for i, j := range jobs {
		if ctx.Err() != nil {
			s.Cancelled = true
			break
		}
		fctx := t.file(ctx, i, j.from)
		err := e.Copy(fctx, j.from, j.to)
		t.fileDone(j.size)
		if err != nil {
			if ctx.Err() != nil {
				s.Cancelled = true
				break
			}
			s.Errors = append(s.Errors, err)
			continue
		}
		s.Done++
	}
	return s
}

// freeName returns name, or "stem - Copy.ext", "stem - Copy (2).ext", ...
// when taken (case-insensitive, like Windows).
func freeName(name string, isDir bool, taken map[string]bool) string {
	if !taken[strings.ToLower(name)] {
		return name
	}
	stem, ext := name, ""
	if !isDir {
		if e := path.Ext(name); e != name {
			stem, ext = strings.TrimSuffix(name, e), e
		}
	}
	for n := 1; ; n++ {
		cand := stem + " - Copy" + ext
		if n > 1 {
			cand = fmt.Sprintf("%s - Copy (%d)%s", stem, n, ext)
		}
		if !taken[strings.ToLower(cand)] {
			return cand
		}
	}
}

// walk calls fn for every file below a folder.
func (a *App) walk(ctx context.Context, dir string, fn func(f remote.Entry)) error {
	entries, err := a.List(ctx, dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir {
			if err := a.walk(ctx, e.Path, fn); err != nil {
				return err
			}
		} else {
			fn(e)
		}
	}
	return nil
}

// DragItems lists the files and folders of a selection for a drag to
// Windows Explorer, recursively, with names relative to the selection's
// folder. Folders come before their contents. Only metadata is read.
func (a *App) DragItems(ctx context.Context, entries []remote.Entry) ([]dragdata.Item, error) {
	var items []dragdata.Item
	var add func(e remote.Entry, rel string) error
	add = func(e remote.Entry, rel string) error {
		items = append(items, dragdata.Item{
			Name: dragdata.WindowsName(rel), Remote: e.Path, IsDir: e.IsDir, Size: e.Size, ModTime: e.ModTime,
		})
		if !e.IsDir {
			return nil
		}
		children, err := a.List(ctx, e.Path)
		if err != nil {
			return err
		}
		for _, c := range children {
			if err := add(c, rel+"/"+c.Name); err != nil {
				return err
			}
		}
		return nil
	}
	for _, e := range entries {
		if err := add(e, e.Name); err != nil {
			return nil, err
		}
	}
	return items, nil
}

// DownloadFile downloads one file to an exact local path (used when Windows
// Explorer asks for a dragged file's content).
func (a *App) DownloadFile(ctx context.Context, remotePath, localPath string) error {
	_, e, err := a.conn(ctx)
	if err != nil {
		return err
	}
	_, err = e.Download(ctx, remotePath, localPath)
	return err
}

// FileStatus is a file's state relative to this client's last transfer.
type FileStatus int

const (
	StatusUnknown  FileStatus = iota // never transferred by this client
	StatusUpToDate                   // same version as the last transfer
	StatusChanged                    // changed on the server since then
)

// Status checks files of a listing against the transfer list. Only files this
// client transferred are checked (one small hash file read each).
func (a *App) Status(ctx context.Context, entries []remote.Entry) map[string]FileStatus {
	out := map[string]FileStatus{}
	_, e, err := a.conn(ctx)
	if err != nil {
		return out
	}
	for _, f := range entries {
		if f.IsDir || ctx.Err() != nil {
			continue
		}
		changed, known, err := e.ServerChanged(f.Path, f.Size)
		switch {
		case err != nil || !known:
		case changed:
			out[f.Path] = StatusChanged
		default:
			out[f.Path] = StatusUpToDate
		}
	}
	return out
}
