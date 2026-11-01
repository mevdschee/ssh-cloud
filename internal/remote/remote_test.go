package remote_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/mevdschee/ssh-cloud/internal/remote"
	"github.com/mevdschee/ssh-cloud/internal/sshtest"
	"golang.org/x/crypto/ssh"
)

type fixture struct {
	srv    *sshtest.Server
	root   string
	cfg    remote.Config
	client *remote.Client
}

func setup(t *testing.T) *fixture {
	t.Helper()
	srv := sshtest.Start(t, sshtest.Options{})
	f := &fixture{srv: srv, root: srv.Root, cfg: srv.Config()}
	c, err := remote.Dial(context.Background(), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	f.client = c
	return f
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(root, rel string) bool {
	_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil
}

func TestHostKeyTrustOnFirstUse(t *testing.T) {
	f := setup(t) // first dial trusted and stored the host key

	asked := false
	cfg := f.cfg
	cfg.TrustHost = func(string, ssh.PublicKey) bool { asked = true; return false }
	c, err := remote.Dial(context.Background(), cfg)
	if err != nil {
		t.Fatalf("second dial with stored key: %v", err)
	}
	c.Close()
	if asked {
		t.Fatal("known host must not be asked again")
	}
}

func TestHostKeyUnknownRejected(t *testing.T) {
	f := setup(t)
	cfg := f.cfg
	cfg.KnownHostsFile = filepath.Join(t.TempDir(), "known_hosts")
	cfg.TrustHost = nil
	_, err := remote.Dial(context.Background(), cfg)
	if !errors.Is(err, remote.ErrHostUnknown) {
		t.Fatalf("want ErrHostUnknown, got %v", err)
	}
}

func TestHostKeyMismatch(t *testing.T) {
	f := setup(t)

	// A different server on the same address would present another key;
	// simulate by writing a wrong key for this host into known_hosts.
	other, _ := sshtest.NewKey(t)
	cfg := f.cfg
	cfg.KnownHostsFile = filepath.Join(t.TempDir(), "known_hosts")
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	line := sshtest.KnownHostsLine(addr, other.PublicKey())
	if err := os.WriteFile(cfg.KnownHostsFile, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := remote.Dial(context.Background(), cfg)
	if !errors.Is(err, remote.ErrHostKeyMismatch) {
		t.Fatalf("want ErrHostKeyMismatch, got %v", err)
	}
}

func TestWrongClientKeyRejected(t *testing.T) {
	f := setup(t)
	_, otherPEM := sshtest.NewKey(t)
	cfg := f.cfg
	cfg.KeyFile = filepath.Join(t.TempDir(), "other")
	if err := os.WriteFile(cfg.KeyFile, otherPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Dial(context.Background(), cfg); err == nil {
		t.Fatal("dial with unknown client key must fail")
	}
}

func TestRootMustExist(t *testing.T) {
	f := setup(t)
	cfg := f.cfg
	cfg.Root = filepath.Join(f.root, "missing")
	if _, err := remote.Dial(context.Background(), cfg); err == nil {
		t.Fatal("dial with missing root must fail")
	}
}

func TestListHidesInternalFilesAndSorts(t *testing.T) {
	f := setup(t)
	writeFile(t, f.root, "b.txt", "b")
	writeFile(t, f.root, "A.txt", "a")
	writeFile(t, f.root, ".A.txt.sha256", "hash")
	writeFile(t, f.root, ".A.txt.tmp-123", "tmp")
	writeFile(t, f.root, ".A.txt.claim-456", "claim")
	writeFile(t, f.root, ".profile", "dotfile")
	writeFile(t, f.root, "zdir/x", "x")

	got, err := f.client.List("")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range got {
		names = append(names, e.Name)
	}
	want := []string{"zdir", ".profile", "A.txt", "b.txt"}
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got %v, want %v", names, want)
		}
	}
	if !got[0].IsDir || got[0].Path != "zdir" {
		t.Fatalf("first entry should be folder zdir, got %+v", got[0])
	}
}

func TestStat(t *testing.T) {
	f := setup(t)
	writeFile(t, f.root, "docs/report.docx", "12345")
	e, err := f.client.Stat("/docs//report.docx")
	if err != nil {
		t.Fatal(err)
	}
	if e.Path != "docs/report.docx" || e.Size != 5 || e.IsDir {
		t.Fatalf("unexpected entry %+v", e)
	}
	if _, err := f.client.Stat("docs/missing"); !errors.Is(err, remote.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestPathsCannotEscapeRoot(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd": "etc/passwd",
		`docs\a.txt`:       "docs/a.txt",
		"/":                "",
		"a/./b/../c":       "a/c",
	}
	for in, want := range cases {
		if got := remote.Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMoveFileCreatesTargetAndPrunesSource(t *testing.T) {
	f := setup(t)
	writeFile(t, f.root, "a/b/report.docx", "data")
	writeFile(t, f.root, "a/b/.report.docx.sha256", "hash")

	if err := f.client.Move("a/b/report.docx", "x/y/final.docx"); err != nil {
		t.Fatal(err)
	}
	if !exists(f.root, "x/y/final.docx") || !exists(f.root, "x/y/.final.docx.sha256") {
		t.Fatal("file and hash file should be at the new path")
	}
	if exists(f.root, "a") {
		t.Fatal("empty source namespaces should be pruned")
	}
}

func TestMoveNeverOverwrites(t *testing.T) {
	f := setup(t)
	writeFile(t, f.root, "a.txt", "a")
	writeFile(t, f.root, "b.txt", "b")
	err := f.client.Move("a.txt", "b.txt")
	if !errors.Is(err, remote.ErrExists) {
		t.Fatalf("want ErrExists, got %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(f.root, "b.txt"))
	if string(data) != "b" {
		t.Fatal("target was overwritten")
	}
}

func TestMoveFolderOntoEmptyFolderRefused(t *testing.T) {
	f := setup(t)
	writeFile(t, f.root, "src/a.txt", "a")
	if err := os.Mkdir(filepath.Join(f.root, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Move("src", "dst"); !errors.Is(err, remote.ErrExists) {
		t.Fatalf("want ErrExists, got %v", err)
	}
}

func TestMoveFolderIntoItselfRefused(t *testing.T) {
	f := setup(t)
	writeFile(t, f.root, "src/a.txt", "a")
	if err := f.client.Move("src", "src/sub"); !errors.Is(err, remote.ErrInvalidPath) {
		t.Fatalf("want ErrInvalidPath, got %v", err)
	}
}

func TestMoveFolder(t *testing.T) {
	f := setup(t)
	writeFile(t, f.root, "proj/a.txt", "a")
	writeFile(t, f.root, "proj/sub/b.txt", "b")
	if err := f.client.Move("proj", "archive/2026/proj"); err != nil {
		t.Fatal(err)
	}
	if !exists(f.root, "archive/2026/proj/sub/b.txt") || exists(f.root, "proj") {
		t.Fatal("folder should be moved as a whole")
	}
}

func TestRename(t *testing.T) {
	f := setup(t)
	writeFile(t, f.root, "docs/a.txt", "a")
	writeFile(t, f.root, "docs/.a.txt.sha256", "hash")
	if err := f.client.Rename("docs/a.txt", "b.txt"); err != nil {
		t.Fatal(err)
	}
	if !exists(f.root, "docs/b.txt") || !exists(f.root, "docs/.b.txt.sha256") {
		t.Fatal("rename should keep the namespace and move the hash file")
	}
	if err := f.client.Rename("docs/b.txt", "../c.txt"); !errors.Is(err, remote.ErrInvalidPath) {
		t.Fatalf("names with slashes must be refused, got %v", err)
	}
}

func TestInternalNamesRefused(t *testing.T) {
	f := setup(t)
	writeFile(t, f.root, "a.txt", "a")
	if err := f.client.Rename("a.txt", ".a.txt.sha256"); !errors.Is(err, remote.ErrInvalidPath) {
		t.Fatalf("want ErrInvalidPath, got %v", err)
	}
	if err := f.client.Delete(".a.txt.sha256"); !errors.Is(err, remote.ErrInvalidPath) {
		t.Fatalf("want ErrInvalidPath, got %v", err)
	}
}

func TestDeleteFileRemovesHashAndPrunes(t *testing.T) {
	f := setup(t)
	writeFile(t, f.root, "a/b/c.txt", "c")
	writeFile(t, f.root, "a/b/.c.txt.sha256", "hash")
	writeFile(t, f.root, "a/keep.txt", "k")
	if err := f.client.Delete("a/b/c.txt"); err != nil {
		t.Fatal(err)
	}
	if exists(f.root, "a/b") {
		t.Fatal("empty namespace a/b should be pruned")
	}
	if !exists(f.root, "a/keep.txt") {
		t.Fatal("non-empty namespace a must stay")
	}
}

func TestDeleteFolder(t *testing.T) {
	f := setup(t)
	writeFile(t, f.root, "p/q/r.txt", "r")
	writeFile(t, f.root, "p/q/.r.txt.sha256", "hash")
	if err := f.client.Delete("p"); err != nil {
		t.Fatal(err)
	}
	if exists(f.root, "p") {
		t.Fatal("folder should be deleted")
	}
	if !exists(f.root, "") {
		t.Fatal("root must never be pruned")
	}
}

func TestRootCannotBeDeletedOrMoved(t *testing.T) {
	f := setup(t)
	if err := f.client.Delete("/"); !errors.Is(err, remote.ErrInvalidPath) {
		t.Fatalf("want ErrInvalidPath, got %v", err)
	}
	if err := f.client.Move("", "x"); !errors.Is(err, remote.ErrInvalidPath) {
		t.Fatalf("want ErrInvalidPath, got %v", err)
	}
}

func TestCloseEndsConnection(t *testing.T) {
	f := setup(t)
	f.client.Close()
	<-f.client.Done()
	if !errors.Is(f.client.Err(), remote.ErrClosed) {
		t.Fatalf("want ErrClosed, got %v", f.client.Err())
	}
}
