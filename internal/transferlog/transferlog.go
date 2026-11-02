// Package transferlog is the local transfer list: an append-only record of
// every download and upload of one connection profile. The latest successful
// entry per remote path holds the content hash and the server version (size
// and mtime) this client last read or wrote, which is the basis for collision
// detection.
package transferlog

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Direction of a transfer.
type Direction string

const (
	Download Direction = "download"
	Upload   Direction = "upload"
	Move     Direction = "move" // a rename or move made in the app
)

// Result of a transfer.
type Result string

const (
	Done      Result = "done"      // downloaded, or uploaded without conflict
	Replaced  Result = "replaced"  // uploaded over a different version, by choice
	KeptBoth  Result = "kept-both" // uploaded under a new name next to the original
	Unchanged Result = "unchanged" // upload skipped, server already had this content
	Skipped   Result = "skipped"   // upload skipped by the user after a conflict
	Conflict  Result = "conflict"  // upload stopped, waiting for a decision
	Failed    Result = "failed"
)

// Entry is one line of the transfer list.
type Entry struct {
	Time       time.Time `json:"time"`
	Direction  Direction `json:"dir"`
	RemotePath string    `json:"remote"`
	LocalPath  string    `json:"local,omitempty"`
	Size       int64     `json:"size"`
	Hash       string    `json:"sha256,omitempty"`
	// Mtime is the server's modification time (Unix seconds) of the version
	// read or written. Together with Size it identifies that version. Zero
	// means unknown (an entry from before v0.3, or a file changed during a
	// download).
	Mtime  int64  `json:"mtime,omitempty"`
	Result Result `json:"result"`
	Error  string `json:"error,omitempty"`
	From   string `json:"from,omitempty"` // previous remote path, for moves
	// DragOut marks a download made by dragging out to Windows Explorer. Its
	// LocalPath is a temp file, as Explorer does not tell where it put the
	// copy; a file with the same name counts as that copy.
	DragOut bool `json:"dragOut,omitempty"`
}

// known reports whether the entry records a finished transfer.
func (e Entry) known() bool {
	switch e.Result {
	case Done, Replaced, KeptBoth, Unchanged:
		return e.Hash != ""
	}
	return false
}

// Log is the transfer list of one profile, safe for concurrent use.
type Log struct {
	mu      sync.Mutex
	f       *os.File
	entries []Entry
	latest  map[string]Entry // remote path -> latest entry with a known hash
}

// Open opens (or creates) a transfer list file and loads it.
func Open(file string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	l := &Log{f: f, latest: map[string]Entry{}}
	if err := l.load(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("load transfer list %s: %w", file, err)
	}
	// After a crash during append the file may end in a torn line; start the
	// next entry on a fresh line so it is not glued to the torn one.
	if fi, err := f.Stat(); err == nil && fi.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, fi.Size()-1); err == nil && last[0] != '\n' {
			if _, err := f.Write([]byte("\n")); err != nil {
				f.Close()
				return nil, err
			}
		}
	}
	return l, nil
}

func (l *Log) load(r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			// A torn last line (crash during append) is skipped.
			continue
		}
		l.apply(e)
	}
	return sc.Err()
}

// apply updates the in-memory state with an entry.
func (l *Log) apply(e Entry) {
	l.entries = append(l.entries, e)
	switch {
	case e.Direction == Move:
		l.move(e.From, e.RemotePath)
	case e.known():
		l.latest[e.RemotePath] = e
	}
}

// move re-keys the latest entries of a file, or of every file below a folder.
func (l *Log) move(from, to string) {
	for p, e := range l.latest {
		var np string
		switch {
		case p == from:
			np = to
		case strings.HasPrefix(p, from+"/"):
			np = to + p[len(from):]
		default:
			continue
		}
		delete(l.latest, p)
		e.RemotePath = np
		l.latest[np] = e
	}
}

// Add appends an entry and syncs it to disk. A zero Time is set to now.
func (l *Log) Add(e Entry) error {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return errors.New("transfer list closed")
	}
	if _, err := l.f.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := l.f.Sync(); err != nil {
		return err
	}
	l.apply(e)
	return nil
}

// Moved records a rename or move of a file or folder made in the app, so the
// next upload to the new path still matches.
func (l *Log) Moved(from, to string) error {
	return l.Add(Entry{Direction: Move, From: from, RemotePath: to, Result: Done})
}

// Latest returns the latest finished transfer of a remote path.
func (l *Log) Latest(remotePath string) (Entry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.latest[remotePath]
	return e, ok
}

// Entries returns a copy of all entries, oldest first.
func (l *Log) Entries() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Entry(nil), l.entries...)
}

// Close closes the file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
