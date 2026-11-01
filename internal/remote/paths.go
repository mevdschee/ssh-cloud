package remote

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"path"
	"strings"
)

// ErrInvalidPath is returned for paths that are empty where a file is needed,
// or whose name is reserved for SSH-Cloud's own files.
var ErrInvalidPath = errors.New("invalid path")

// Suffixes of SSH-Cloud's internal files, which live next to user files as
// hidden dotfiles: ".<name>.sha256", ".<name>.tmp-<random>" and
// ".<name>.claim-<random>". They are never shown and cannot be created,
// renamed or deleted directly by the user.
const (
	hashSuffix  = ".sha256"
	tmpMarker   = ".tmp-"
	claimMarker = ".claim-"
)

// Clean normalises a cloud path: forward slashes, no leading or trailing
// slash, no "." or ".." elements. The root is "". Because the path is cleaned
// as an absolute path, ".." can never escape the root.
func Clean(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	return strings.TrimPrefix(path.Clean("/"+p), "/")
}

// Join joins cloud path elements and cleans the result.
func Join(elem ...string) string { return Clean(path.Join(elem...)) }

// Dir returns the parent namespace of a cloud path ("" for the root).
func Dir(p string) string {
	d := path.Dir(Clean(p))
	if d == "." {
		return ""
	}
	return d
}

// Base returns the last element of a cloud path.
func Base(p string) string { return path.Base(Clean(p)) }

// IsInternal reports whether a file name belongs to SSH-Cloud itself.
func IsInternal(name string) bool {
	if !strings.HasPrefix(name, ".") {
		return false
	}
	return strings.HasSuffix(name, hashSuffix) ||
		strings.Contains(name, tmpMarker) ||
		strings.Contains(name, claimMarker)
}

// HashFileName returns the name of the hash file for a file name.
func HashFileName(name string) string { return "." + name + hashSuffix }

// validName checks a cleaned, non-root path whose last element is usable.
func validName(p string) error {
	if p == "" {
		return ErrInvalidPath
	}
	if IsInternal(path.Base(p)) {
		return ErrInvalidPath
	}
	return nil
}

// isWithin reports whether p equals dir or lies below it.
func isWithin(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// TempName returns a fresh hidden temp file name for an upload of name.
func TempName(name string) string { return "." + name + tmpMarker + randomID() }

// ClaimName returns a fresh hidden claim file name for name. Claim names are
// unique so a client's cleanup can never remove another client's claim.
func ClaimName(name string) string { return "." + name + claimMarker + randomID() }

// ClaimOf reports whether file is a claim file and returns the name of the
// file it claims.
func ClaimOf(file string) (string, bool) {
	if !strings.HasPrefix(file, ".") {
		return "", false
	}
	i := strings.LastIndex(file, claimMarker)
	if i <= 1 {
		return "", false
	}
	return file[1:i], true
}

func randomID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
