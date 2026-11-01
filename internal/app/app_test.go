package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mevdschee/ssh-cloud/internal/sshtest"
	"github.com/mevdschee/ssh-cloud/internal/transfer"
)

var bg = context.Background()

func open(t *testing.T, srv *sshtest.Server) *App {
	t.Helper()
	a, err := Open(bg, srv.Config(), filepath.Join(t.TempDir(), "t.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

func names(t *testing.T, a *App, dir string) []string {
	t.Helper()
	entries, err := a.List(bg, dir)
	if err != nil {
		t.Fatal(err)
	}
	var n []string
	for _, e := range entries {
		n = append(n, e.Name)
	}
	return n
}

func writeLocal(t *testing.T, dir, rel, content string) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestVirtualFolderBecomesRealOnUpload(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{})
	srv.Write("b.txt", "b")
	a := open(t, srv)

	p, err := a.NewFolder(bg, "", "New folder")
	if err != nil {
		t.Fatal(err)
	}
	if got := names(t, a, ""); len(got) != 2 || got[0] != "New folder" {
		t.Fatalf("virtual folder should be listed first, got %v", got)
	}
	if got := names(t, a, p); len(got) != 0 {
		t.Fatalf("virtual folder lists empty, got %v", got)
	}
	if srv.Exists("New folder") {
		t.Fatal("virtual folder must not exist on the server")
	}
	if _, err := a.NewFolder(bg, "", "new FOLDER"); err == nil {
		t.Fatal("duplicate name (case-insensitive) must be refused")
	}

	local := writeLocal(t, t.TempDir(), "x.txt", "x")
	s := a.Upload(bg, []string{local}, p, nil, nil)
	if s.Done != 1 || !srv.Exists("New folder/x.txt") {
		t.Fatalf("upload into virtual folder: %v", s)
	}
	names(t, a, "")
	if a.IsVirtual(p) {
		t.Fatal("folder should be real now")
	}
}

func TestRenameAndDeleteVirtualFolder(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{})
	a := open(t, srv)
	p, _ := a.NewFolder(bg, "", "tmp")
	a.NewFolder(bg, p, "inner")
	to, err := a.Rename(bg, p, "renamed")
	if err != nil || !a.IsVirtual(to) || !a.IsVirtual("renamed/inner") {
		t.Fatalf("rename virtual: %v", err)
	}
	if err := a.Delete(bg, []string{to}); err != nil || a.IsVirtual(to) {
		t.Fatalf("delete virtual: %v", err)
	}
}

func TestUploadFolderRecursivelyAndDownloadBack(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{})
	a := open(t, srv)
	src := t.TempDir()
	writeLocal(t, src, "proj/a.txt", "a")
	writeLocal(t, src, "proj/sub/b.txt", "b")
	os.MkdirAll(filepath.Join(src, "proj/empty"), 0o755)

	s := a.Upload(bg, []string{filepath.Join(src, "proj")}, "docs", nil, nil)
	if s.Done != 2 || srv.Read("docs/proj/sub/b.txt") != "b" || srv.Exists("docs/proj/empty") {
		t.Fatalf("folder upload: %v", s)
	}

	dst := t.TempDir()
	s = a.Download(bg, []string{"docs/proj"}, dst, nil, nil)
	if s.Done != 2 {
		t.Fatalf("folder download: %v", s)
	}
	data, _ := os.ReadFile(filepath.Join(dst, "proj", "sub", "b.txt"))
	if string(data) != "b" {
		t.Fatal("downloaded folder should mirror the remote structure")
	}

	// Downloading again asks before overwriting local files.
	asked := 0
	s = a.Download(bg, []string{"docs/proj"}, dst, func(string) (bool, bool) { asked++; return false, true }, nil)
	if asked != 1 || s.Skipped != 2 {
		t.Fatalf("overwrite should be asked once with apply-to-all, asked=%d %v", asked, s)
	}
}

func TestUploadConflictChoices(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{})
	srv.Write("r.txt", "server")
	srv.Write("q.txt", "server")
	a := open(t, srv)
	dir := t.TempDir()
	r := writeLocal(t, dir, "r.txt", "mine")
	q := writeLocal(t, dir, "q.txt", "mine")

	var reasons []transfer.Reason
	decide := func(c *transfer.ConflictError, _ string) (Choice, bool) {
		reasons = append(reasons, c.Reason)
		return KeepBoth, true
	}
	s := a.Upload(bg, []string{r, q}, "", decide, nil)
	if s.KeptBoth != 2 || len(reasons) != 1 {
		t.Fatalf("apply-to-all should ask once, asked %d: %v", len(reasons), s)
	}
	if srv.Read("r (2).txt") != "mine" || srv.Read("r.txt") != "server" {
		t.Fatal("keep both should leave the original alone")
	}

	s = a.Upload(bg, []string{r}, "", func(*transfer.ConflictError, string) (Choice, bool) { return Replace, false }, nil)
	if s.Replaced != 1 || srv.Read("r.txt") != "mine" {
		t.Fatalf("replace: %v", s)
	}
	s = a.Upload(bg, []string{r}, "", nil, nil)
	if s.Unchanged != 1 {
		t.Fatalf("second upload of the same content should be unchanged: %v", s)
	}
}

func TestReconnectAfterConnectionLoss(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{})
	srv.Write("a.txt", "a")
	a := open(t, srv)
	a.client.Close()
	if a.Connected() {
		t.Fatal("should report disconnected")
	}
	if got := names(t, a, ""); len(got) != 1 {
		t.Fatalf("list should reconnect, got %v", got)
	}
}

func TestDragItems(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{})
	srv.Write("docs/proj/a.txt", "a")
	srv.Write("docs/proj/sub/b.txt", "bb")
	srv.Write("docs/top.txt", "t")
	a := open(t, srv)
	entries, _ := a.List(bg, "docs")
	items, err := a.DragItems(bg, entries)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range items {
		got = append(got, it.Name)
	}
	// Folders before files, and every folder before its contents.
	want := []string{`proj`, `proj\sub`, `proj\sub\b.txt`, `proj\a.txt`, `top.txt`}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	b := items[2]
	if b.Remote != "docs/proj/sub/b.txt" || b.Size != 2 || !items[1].IsDir {
		t.Fatalf("item %+v", b)
	}

	dst := filepath.Join(t.TempDir(), "x", "b.txt")
	if err := a.DownloadFile(bg, b.Remote, dst); err != nil {
		t.Fatal(err)
	}
	if e, ok := a.Log().Latest("docs/proj/sub/b.txt"); !ok || e.Size != 2 {
		t.Fatal("drag download must be recorded in the transfer list")
	}
}
