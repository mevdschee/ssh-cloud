package remote

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"
)

var (
	// ErrExists is returned when the target of a rename or move exists.
	ErrExists = fs.ErrExist
	// ErrNotFound is returned when the source of an operation is missing.
	ErrNotFound = fs.ErrNotExist
)

// Entry is one item of a listing.
type Entry struct {
	Path    string // cloud path, for example "docs/report.docx"
	Name    string
	IsDir   bool
	Size    int64
	ModTime time.Time
	Mode    fs.FileMode
}

func entryOf(p string, fi os.FileInfo) Entry {
	return Entry{
		Path:    p,
		Name:    fi.Name(),
		IsDir:   fi.IsDir(),
		Size:    fi.Size(),
		ModTime: fi.ModTime(),
		Mode:    fi.Mode(),
	}
}

// List returns the visible entries of a namespace: folders first, then files,
// each sorted case-insensitively by name. SSH-Cloud's internal files are
// hidden.
func (c *Client) List(dir string) ([]Entry, error) {
	dir = Clean(dir)
	infos, err := c.sftp.ReadDir(c.abs(dir))
	if err != nil {
		return nil, fmt.Errorf("list %q: %w", dir, err)
	}
	entries := make([]Entry, 0, len(infos))
	for _, fi := range infos {
		if IsInternal(fi.Name()) {
			continue
		}
		entries = append(entries, entryOf(Join(dir, fi.Name()), fi))
	}
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return entries, nil
}

// Stat returns the entry for a cloud path.
func (c *Client) Stat(p string) (Entry, error) {
	p = Clean(p)
	fi, err := c.sftp.Stat(c.abs(p))
	if err != nil {
		return Entry{}, fmt.Errorf("stat %q: %w", p, err)
	}
	return entryOf(p, fi), nil
}

// exists reports whether a cloud path exists (without following symlinks).
func (c *Client) exists(p string) (bool, error) {
	_, err := c.sftp.Lstat(c.abs(p))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// MkdirAll creates a namespace and its parents. Namespaces normally come into
// existence implicitly when a file is uploaded into them.
func (c *Client) MkdirAll(dir string) error {
	dir = Clean(dir)
	if dir == "" {
		return nil
	}
	if err := c.sftp.MkdirAll(c.abs(dir)); err != nil {
		return fmt.Errorf("create %q: %w", dir, err)
	}
	return nil
}

// Rename gives a file or folder a new name within the same namespace.
func (c *Client) Rename(p, newName string) error {
	if newName == "" || strings.ContainsAny(newName, `/\`) {
		return fmt.Errorf("rename to %q: %w", newName, ErrInvalidPath)
	}
	return c.Move(p, Join(Dir(p), newName))
}

// Move moves (or renames) a file or folder to a new cloud path with a single
// SFTP rename. It never overwrites: if the target exists it fails with
// ErrExists. Missing parent namespaces of the target are created, and parent
// namespaces of the source that became empty are removed. A file keeps its
// mtime, so it keeps its version.
func (c *Client) Move(from, to string) error {
	from, to = Clean(from), Clean(to)
	if err := validName(from); err != nil {
		return fmt.Errorf("move %q: %w", from, err)
	}
	if err := validName(to); err != nil {
		return fmt.Errorf("move to %q: %w", to, err)
	}
	if from == to {
		return nil
	}
	if isWithin(to, from) {
		return fmt.Errorf("move %q into itself: %w", from, ErrInvalidPath)
	}

	src, err := c.sftp.Lstat(c.abs(from))
	if err != nil {
		return fmt.Errorf("move %q: %w", from, err)
	}
	// Explicit check, because rename(2) on the server would replace an empty
	// directory, and not every SFTP server refuses to overwrite files.
	switch ok, err := c.exists(to); {
	case err != nil:
		return fmt.Errorf("move to %q: %w", to, err)
	case ok:
		return fmt.Errorf("move to %q: %w", to, ErrExists)
	}

	if err := c.MkdirAll(Dir(to)); err != nil {
		return err
	}
	if err := c.sftp.Rename(c.abs(from), c.abs(to)); err != nil {
		c.prune(Dir(to))
		return fmt.Errorf("move %q to %q: %w", from, to, err)
	}
	if !src.IsDir() {
		c.removeLegacyHash(from)
	}
	c.prune(Dir(from))
	slog.Info("moved", "from", from, "to", to)
	return nil
}

// removeLegacyHash removes the hidden hash file that versions before v0.3
// kept next to file p, if there is one. It is no longer used, but would keep
// an emptied folder from being pruned.
func (c *Client) removeLegacyHash(p string) {
	_ = c.sftp.Remove(c.abs(Join(Dir(p), legacyHashName(Base(p)))))
}

// Delete removes a file or a folder with everything in it. Parent namespaces
// that became empty are removed.
func (c *Client) Delete(p string) error {
	p = Clean(p)
	if err := validName(p); err != nil {
		return fmt.Errorf("delete %q: %w", p, err)
	}
	fi, err := c.sftp.Lstat(c.abs(p))
	if err != nil {
		return fmt.Errorf("delete %q: %w", p, err)
	}
	if fi.IsDir() {
		err = c.sftp.RemoveAll(c.abs(p))
	} else {
		err = c.sftp.Remove(c.abs(p))
		if err == nil {
			c.removeLegacyHash(p)
		}
	}
	if err != nil {
		return fmt.Errorf("delete %q: %w", p, err)
	}
	c.prune(Dir(p))
	slog.Info("deleted", "path", p, "dir", fi.IsDir())
	return nil
}

// prune removes empty namespaces from dir upwards, stopping at the root or at
// the first namespace that is not empty. rmdir fails on non-empty
// directories, so this is safe while others write concurrently.
func (c *Client) prune(dir string) {
	for dir = Clean(dir); dir != ""; dir = Dir(dir) {
		if err := c.sftp.RemoveDirectory(c.abs(dir)); err != nil {
			return
		}
	}
}
