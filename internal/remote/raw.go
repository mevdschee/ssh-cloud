package remote

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/pkg/sftp"
)

// Raw operations are the building blocks of the transfer protocol. Unlike the
// user-facing operations they accept SSH-Cloud's internal file names (hash,
// temp and claim files) and do no pruning or bookkeeping. Paths are cloud
// paths and are cleaned, so they never leave the root.

// RawOpen opens a file for reading.
func (c *Client) RawOpen(p string) (*sftp.File, error) {
	return c.sftp.Open(c.abs(p))
}

// RawCreate opens a file for writing with os.O_* flags, for example
// os.O_WRONLY|os.O_CREATE|os.O_EXCL.
func (c *Client) RawCreate(p string, flags int) (*sftp.File, error) {
	return c.sftp.OpenFile(c.abs(p), flags)
}

// RawRemove removes a file.
func (c *Client) RawRemove(p string) error {
	return c.sftp.Remove(c.abs(p))
}

// RawLstat stats a path without following symlinks.
func (c *Client) RawLstat(p string) (os.FileInfo, error) {
	return c.sftp.Lstat(c.abs(p))
}

// RawReadDir lists a namespace including internal files.
func (c *Client) RawReadDir(dir string) ([]os.FileInfo, error) {
	return c.sftp.ReadDir(c.abs(dir))
}

// RawRename does a plain SFTP rename. On OpenSSH it never replaces an existing
// target; when it fails and the target exists the error wraps ErrExists, when
// the source is missing it wraps ErrNotFound. Use RenameReplaces to learn
// whether the server honours the no-replace rule.
func (c *Client) RawRename(from, to string) error {
	err := c.sftp.Rename(c.abs(from), c.abs(to))
	if err == nil {
		return nil
	}
	// SFTP v3 reports a generic failure; find the actual cause.
	if _, serr := c.sftp.Lstat(c.abs(from)); errors.Is(serr, fs.ErrNotExist) {
		return fmt.Errorf("rename %q: %w", from, ErrNotFound)
	}
	if _, terr := c.sftp.Lstat(c.abs(to)); terr == nil {
		return fmt.Errorf("rename to %q: %w", to, ErrExists)
	}
	return err
}

// RawClaim atomically renames a file to a fresh, unique claim name. It uses
// posix-rename@openssh.com (a single rename(2) on the server) when available:
// only one client can move the source away. The target name is unique, so
// the replacing semantics of rename(2) never matter. Without the extension it
// falls back to a plain rename.
func (c *Client) RawClaim(from, claim string) error {
	if _, ok := c.sftp.HasExtension("posix-rename@openssh.com"); !ok {
		return c.RawRename(from, claim)
	}
	err := c.sftp.PosixRename(c.abs(from), c.abs(claim))
	if err == nil {
		return nil
	}
	if _, serr := c.sftp.Lstat(c.abs(from)); errors.Is(serr, fs.ErrNotExist) {
		return fmt.Errorf("claim %q: %w", from, ErrNotFound)
	}
	return err
}

// RenameReplaces reports whether a plain SFTP rename on this server replaces
// an existing target (pkg/sftp's server and some others do; OpenSSH does
// not). It is probed once per connection with two small hidden files in the
// root and cached.
func (c *Client) RenameReplaces() (bool, error) {
	c.probeOnce.Do(func() {
		c.replaces, c.probeErr = c.probeRename()
	})
	return c.replaces, c.probeErr
}

func (c *Client) probeRename() (bool, error) {
	id := randomID()
	a := ".sshcloud-probe" + tmpMarker + id + "-a"
	b := ".sshcloud-probe" + tmpMarker + id + "-b"
	defer c.sftp.Remove(c.abs(a))
	defer c.sftp.Remove(c.abs(b))
	for _, p := range []string{a, b} {
		f, err := c.RawCreate(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
		if err != nil {
			return false, fmt.Errorf("probe rename: %w", err)
		}
		f.Close()
	}
	return c.sftp.Rename(c.abs(a), c.abs(b)) == nil, nil
}

// Prune removes empty namespaces from dir upwards (see Move and Delete).
func (c *Client) Prune(dir string) { c.prune(dir) }
