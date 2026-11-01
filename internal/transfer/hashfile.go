package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/mevdschee/ssh-cloud/internal/remote"
)

// hashInfo is the content of a hash file: SHA-256 and size of the current
// version of the file next to it.
type hashInfo struct {
	Hash string
	Size int64
}

var hashLine = regexp.MustCompile(`^sha256 ([0-9a-f]{64}) ([0-9]+)$`)

func hashPath(p string) string {
	return remote.Join(remote.Dir(p), remote.HashFileName(remote.Base(p)))
}

// readHash reads the hash file of p. ok is false when it is missing or
// invalid, both meaning "unknown version".
func (e *Engine) readHash(p string) (h hashInfo, ok bool, err error) {
	f, err := e.c.RawOpen(hashPath(p))
	if errors.Is(err, fs.ErrNotExist) {
		return hashInfo{}, false, nil
	}
	if err != nil {
		return hashInfo{}, false, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 256))
	if err != nil {
		return hashInfo{}, false, err
	}
	m := hashLine.FindStringSubmatch(strings.TrimSpace(string(data)))
	if m == nil {
		return hashInfo{}, false, nil
	}
	size, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil {
		return hashInfo{}, false, nil
	}
	return hashInfo{Hash: m[1], Size: size}, true, nil
}

// writeHash writes the hash file of p. Only called by the client that just
// put this version in place, or that holds the claim.
func (e *Engine) writeHash(p string, h hashInfo) error {
	f, err := e.c.RawCreate(hashPath(p), os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("write hash file of %q: %w", p, err)
	}
	if _, err := fmt.Fprintf(f, "sha256 %s %d\n", h.Hash, h.Size); err != nil {
		f.Close()
		return fmt.Errorf("write hash file of %q: %w", p, err)
	}
	return f.Close()
}

// removeHash deletes the hash file of p, marking it "unknown version".
func (e *Engine) removeHash(p string) error {
	err := e.c.RawRemove(hashPath(p))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove hash file of %q: %w", p, err)
	}
	return nil
}

// hashLocal hashes a local file.
func hashLocal(file string) (hashInfo, error) {
	f, err := os.Open(file)
	if err != nil {
		return hashInfo{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return hashInfo{}, err
	}
	return hashInfo{Hash: hex.EncodeToString(h.Sum(nil)), Size: n}, nil
}

// numbered returns "dir/stem (n).ext" for "dir/stem.ext".
func numbered(p string, n int) string {
	dir, name := remote.Dir(p), remote.Base(p)
	ext := path.Ext(name)
	if ext == name { // ".profile" has no extension
		ext = ""
	}
	stem := strings.TrimSuffix(name, ext)
	return remote.Join(dir, fmt.Sprintf("%s (%d)%s", stem, n, ext))
}
