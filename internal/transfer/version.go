package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/mevdschee/ssh-cloud/internal/remote"
	"github.com/mevdschee/ssh-cloud/internal/transferlog"
)

// hashInfo is the SHA-256 and size of some content.
type hashInfo struct {
	Hash string
	Size int64
}

// version identifies a version of a file on the server: its size and the
// server's modification time in whole seconds (SFTP v3 has no finer
// resolution). Every upload creates a new file, so the server gives every new
// version a fresh mtime; renames, moves and claims keep it.
type version struct {
	Size  int64
	Mtime int64
}

func versionOf(fi os.FileInfo) version {
	return version{Size: fi.Size(), Mtime: fi.ModTime().Unix()}
}

// recorded returns the server version a transfer list entry recorded. ok is
// false when the entry does not know it.
func recorded(e transferlog.Entry) (v version, ok bool) {
	if e.Mtime == 0 {
		return version{}, false
	}
	return version{Size: e.Size, Mtime: e.Mtime}, true
}

// later makes sure the staged temp file gets a version other than prev, the
// version it replaces: when the server's own mtime is not later than prev's
// (two uploads within one second), it sets the mtime to one second after
// prev's. Along the versions at one path the mtime thus always increases. It
// returns the version of the temp file, which the final rename keeps.
func (e *Engine) later(tmp string, prev version) (version, error) {
	fi, err := e.c.RawLstat(tmp)
	if err != nil {
		return version{}, err
	}
	v := versionOf(fi)
	if v.Mtime > prev.Mtime {
		return v, nil
	}
	mtime := time.Unix(prev.Mtime+1, 0)
	if err := e.c.RawChtimes(tmp, mtime, mtime); err != nil {
		return version{}, fmt.Errorf("set modification time of %q: %w", tmp, err)
	}
	if fi, err = e.c.RawLstat(tmp); err != nil {
		return version{}, err
	}
	if v = versionOf(fi); v.Mtime <= prev.Mtime {
		return version{}, fmt.Errorf("set modification time of %q: server kept %d", tmp, v.Mtime)
	}
	return v, nil
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
