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
	"path/filepath"
	"sort"
	"strings"
	"sync"

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
func (a *App) Delete(ctx context.Context, paths []string) error {
	var errs []error
	for _, p := range paths {
		p = remote.Clean(p)
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

// Progress receives short status messages.
type Progress func(msg string)

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
	add(s.Done, "transferred")
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

type upload struct{ local, remote string }

// Upload uploads local files and folders into a namespace. Folders are
// uploaded recursively; empty local folders are ignored (folders do not exist
// on their own).
func (a *App) Upload(ctx context.Context, localPaths []string, dir string, decide Decider, progress Progress) Summary {
	var s Summary
	var jobs []upload
	for _, lp := range localPaths {
		fi, err := os.Stat(lp)
		if err != nil {
			s.Errors = append(s.Errors, err)
			continue
		}
		if !fi.IsDir() {
			jobs = append(jobs, upload{lp, remote.Join(dir, filepath.Base(lp))})
			continue
		}
		base := filepath.Dir(lp)
		err = filepath.WalkDir(lp, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type().IsRegular() {
				rel, _ := filepath.Rel(base, p)
				jobs = append(jobs, upload{p, remote.Join(dir, filepath.ToSlash(rel))})
			}
			return nil
		})
		if err != nil {
			s.Errors = append(s.Errors, err)
		}
	}

	var all *Choice
	for i, j := range jobs {
		if ctx.Err() != nil {
			s.Cancelled = true
			break
		}
		if progress != nil {
			progress(fmt.Sprintf("Uploading %s (%d of %d)", remote.Base(j.remote), i+1, len(jobs)))
		}
		_, e, err := a.conn(ctx)
		if err != nil {
			s.Errors = append(s.Errors, err)
			break
		}
		res, err := e.Upload(ctx, j.local, j.remote, transfer.Check)
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
				res, err = e.Upload(ctx, j.local, j.remote, transfer.Replace)
			case KeepBoth:
				res, err = e.Upload(ctx, j.local, j.remote, transfer.KeepBoth)
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
				if err != nil {
					s.Errors = append(s.Errors, err)
				}
				if s.Cancelled {
					break
				}
				continue
			}
		}
		if err != nil {
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

type download struct{ remote, local string }

// Download downloads files and folders into a local folder. Folders are
// downloaded recursively.
func (a *App) Download(ctx context.Context, remotePaths []string, localDir string, overwrite OverwriteLocal, progress Progress) Summary {
	var s Summary
	var jobs []download
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
			jobs = append(jobs, download{rp, filepath.Join(localDir, e.Name)})
			continue
		}
		if err := a.walk(ctx, rp, func(p string) {
			rel := strings.TrimPrefix(p, remote.Dir(rp)+"/")
			if remote.Dir(rp) == "" {
				rel = p
			}
			jobs = append(jobs, download{p, filepath.Join(localDir, filepath.FromSlash(rel))})
		}); err != nil {
			s.Errors = append(s.Errors, err)
		}
	}

	var all *bool
	for i, j := range jobs {
		if ctx.Err() != nil {
			s.Cancelled = true
			break
		}
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
				continue
			}
		}
		if progress != nil {
			progress(fmt.Sprintf("Downloading %s (%d of %d)", remote.Base(j.remote), i+1, len(jobs)))
		}
		_, e, err := a.conn(ctx)
		if err != nil {
			s.Errors = append(s.Errors, err)
			break
		}
		if _, err := e.Download(ctx, j.remote, j.local); err != nil {
			s.Errors = append(s.Errors, err)
			continue
		}
		s.Done++
	}
	return s
}

// walk calls fn for every file below a folder.
func (a *App) walk(ctx context.Context, dir string, fn func(p string)) error {
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
			fn(e.Path)
		}
	}
	return nil
}
