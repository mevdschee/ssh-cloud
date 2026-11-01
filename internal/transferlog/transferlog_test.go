package transferlog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLatestSurvivesReopen(t *testing.T) {
	file := filepath.Join(t.TempDir(), "transfers.jsonl")
	l, err := Open(file)
	if err != nil {
		t.Fatal(err)
	}
	must(t, l.Add(Entry{Direction: Download, RemotePath: "a.txt", Hash: "h1", Result: Done}))
	must(t, l.Add(Entry{Direction: Upload, RemotePath: "a.txt", Hash: "h2", Result: Done}))
	must(t, l.Add(Entry{Direction: Upload, RemotePath: "a.txt", Hash: "h3", Result: Conflict}))
	must(t, l.Add(Entry{Direction: Upload, RemotePath: "a.txt", Hash: "h4", Result: Failed}))
	l.Close()

	l, err = Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	e, ok := l.Latest("a.txt")
	if !ok || e.Hash != "h2" {
		t.Fatalf("latest should be the last successful entry h2, got %+v", e)
	}
	if n := len(l.Entries()); n != 4 {
		t.Fatalf("want 4 entries, got %d", n)
	}
}

func TestMovedRekeysFilesAndFolders(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "t.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	must(t, l.Add(Entry{Direction: Download, RemotePath: "docs/a.txt", Hash: "ha", Result: Done}))
	must(t, l.Add(Entry{Direction: Download, RemotePath: "docs/sub/b.txt", Hash: "hb", Result: Done}))
	must(t, l.Add(Entry{Direction: Download, RemotePath: "docsx/c.txt", Hash: "hc", Result: Done}))

	must(t, l.Moved("docs", "archive/docs"))
	if _, ok := l.Latest("docs/a.txt"); ok {
		t.Fatal("old path should be gone")
	}
	if e, ok := l.Latest("archive/docs/sub/b.txt"); !ok || e.Hash != "hb" {
		t.Fatalf("nested file should move along, got %+v", e)
	}
	if _, ok := l.Latest("docsx/c.txt"); !ok {
		t.Fatal("sibling with common prefix must not move")
	}
}

func TestTornLastLineIgnored(t *testing.T) {
	file := filepath.Join(t.TempDir(), "t.jsonl")
	l, err := Open(file)
	if err != nil {
		t.Fatal(err)
	}
	must(t, l.Add(Entry{Direction: Download, RemotePath: "a", Hash: "h", Result: Done}))
	l.Close()
	f, _ := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"time":"2026-11-01T`)
	f.Close()

	l, err = Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, ok := l.Latest("a"); !ok {
		t.Fatal("entries before the torn line must load")
	}
	must(t, l.Add(Entry{Direction: Download, RemotePath: "b", Hash: "h", Result: Done}))
	l.Close()
	l, err = Open(file)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := l.Latest("b"); !ok {
		t.Fatal("entry appended after a torn line must load")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
