package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mevdschee/ssh-cloud/internal/remote"
	"github.com/mevdschee/ssh-cloud/internal/sshtest"
	"github.com/mevdschee/ssh-cloud/internal/transferlog"
)

var bg = context.Background()

// machine is one client: its own connection, transfer list and local disk.
type machine struct {
	t     *testing.T
	e     *Engine
	local string
}

func newMachine(t *testing.T, srv *sshtest.Server) *machine {
	t.Helper()
	c := srv.Dial()
	log, err := transferlog.Open(filepath.Join(t.TempDir(), "transfers.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	e := New(c, log)
	e.ClaimWait = 300 * time.Millisecond
	return &machine{t: t, e: e, local: t.TempDir()}
}

func (m *machine) file(name string) string { return filepath.Join(m.local, name) }

func (m *machine) write(name, content string) string {
	m.t.Helper()
	if err := os.WriteFile(m.file(name), []byte(content), 0o644); err != nil {
		m.t.Fatal(err)
	}
	return m.file(name)
}

func (m *machine) download(remotePath, name string) {
	m.t.Helper()
	if _, err := m.e.Download(bg, remotePath, m.file(name)); err != nil {
		m.t.Fatal(err)
	}
}

func (m *machine) upload(name, remotePath string, mode Mode) (Result, error) {
	return m.e.Upload(bg, m.file(name), remotePath, mode)
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func hashFile(content string) string {
	return fmt.Sprintf("sha256 %s %d\n", sha(content), len(content))
}

func conflictReason(t *testing.T, err error) Reason {
	t.Helper()
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("want conflict, got %v", err)
	}
	return ce.Reason
}

// internalFiles lists leftover temp and claim files below the root.
func internalFiles(t *testing.T, srv *sshtest.Server) []string {
	t.Helper()
	var left []string
	filepath.WalkDir(srv.Root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && (strings.Contains(d.Name(), ".tmp-") || strings.Contains(d.Name(), ".claim-")) {
			left = append(left, p)
		}
		return nil
	})
	return left
}

func TestUploadNewFileWritesHashFile(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{})
	a := newMachine(t, srv)
	a.write("report.docx", "v1")

	res, err := a.upload("report.docx", "docs/report.docx", Check)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != transferlog.Done || srv.Read("docs/report.docx") != "v1" {
		t.Fatalf("unexpected result %+v", res)
	}
	if got := srv.Read("docs/.report.docx.sha256"); got != hashFile("v1") {
		t.Fatalf("hash file = %q", got)
	}
	if left := internalFiles(t, srv); len(left) != 0 {
		t.Fatalf("leftover files: %v", left)
	}
}

func TestDownloadEditUploadReplacesWithoutPrompt(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{})
	a := newMachine(t, srv)
	a.write("r.txt", "v1")
	if _, err := a.upload("r.txt", "r.txt", Check); err != nil {
		t.Fatal(err)
	}
	a.download("r.txt", "copy.txt")
	a.write("copy.txt", "v2 longer")
	if _, err := a.e.Upload(bg, a.file("copy.txt"), "r.txt", Check); err != nil {
		t.Fatal(err)
	}
	if srv.Read("r.txt") != "v2 longer" || srv.Read(".r.txt.sha256") != hashFile("v2 longer") {
		t.Fatal("server should have v2 with a matching hash file")
	}
}

func TestUnchangedUploadSendsNothing(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{})
	a := newMachine(t, srv)
	a.write("r.txt", "v1")
	a.upload("r.txt", "r.txt", Check)

	sent := false
	a.e.beforeCommit = func() { sent = true }
	res, err := a.upload("r.txt", "r.txt", Check)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != transferlog.Unchanged || sent {
		t.Fatalf("unchanged upload must not send content, got %+v sent=%v", res, sent)
	}
}

func TestSameContentFromOtherClientIsUnchanged(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{})
	a, b := newMachine(t, srv), newMachine(t, srv)
	a.write("r.txt", "same")
	a.upload("r.txt", "r.txt", Check)
	b.write("r.txt", "same")
	res, err := b.upload("r.txt", "r.txt", Check)
	if err != nil || res.Outcome != transferlog.Unchanged {
		t.Fatalf("identical content should be skipped, got %+v %v", res, err)
	}
}

func TestOtherClientsChangeTriggersConflict(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{})
	a, b := newMachine(t, srv), newMachine(t, srv)
	a.write("r.txt", "v1")
	a.upload("r.txt", "r.txt", Check)
	b.download("r.txt", "r.txt")

	b.write("r.txt", "b's edit")
	if _, err := b.upload("r.txt", "r.txt", Check); err != nil {
		t.Fatal(err)
	}

	a.write("r.txt", "a's edit")
	_, err := a.upload("r.txt", "r.txt", Check)
	if r := conflictReason(t, err); r != ChangedOnServer {
		t.Fatalf("want ChangedOnServer, got %s", r)
	}
	if srv.Read("r.txt") != "b's edit" {
		t.Fatal("conflicting upload must not change the server")
	}

	// The user chooses Replace.
	res, err := a.upload("r.txt", "r.txt", Replace)
	if err != nil || res.Outcome != transferlog.Replaced || srv.Read("r.txt") != "a's edit" {
		t.Fatalf("replace failed: %+v %v", res, err)
	}
}

func TestNeverTransferredPrompts(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{})
	a, b := newMachine(t, srv), newMachine(t, srv)
	a.write("r.txt", "v1")
	a.upload("r.txt", "r.txt", Check)
	b.write("r.txt", "b's own file")
	_, err := b.upload("r.txt", "r.txt", Check)
	if r := conflictReason(t, err); r != NeverTransferred {
		t.Fatalf("want NeverTransferred, got %s", r)
	}
}

func TestKeepBoth(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{})
	a, b := newMachine(t, srv), newMachine(t, srv)
	a.write("report.docx", "v1")
	a.upload("report.docx", "report.docx", Check)
	srv.Write("report (2).docx", "taken")
	b.write("report.docx", "b")
	if _, err := b.upload("report.docx", "report.docx", Check); !errors.Is(err, ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	res, err := b.upload("report.docx", "report.docx", KeepBoth)
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != "report (3).docx" || srv.Read("report (3).docx") != "b" || srv.Read("report.docx") != "v1" {
		t.Fatalf("keep both wrote %+v", res)
	}
	if srv.Read(".report (3).docx.sha256") != hashFile("b") {
		t.Fatal("copy needs its own hash file")
	}
}

func TestSkipRemovesPendingTemp(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{})
	a, b := newMachine(t, srv), newMachine(t, srv)
	a.write("r.txt", "v1")
	a.upload("r.txt", "r.txt", Check)
	a.download("r.txt", "r.txt")

	// b changes the file after a's content is sent, before a commits.
	a.write("r.txt", "a2")
	a.e.beforeCommit = func() {
		b.write("x", "b2")
		b.e.Upload(bg, b.file("x"), "r.txt", Replace)
	}
	_, err := a.upload("r.txt", "r.txt", Check)
	if r := conflictReason(t, err); r != ChangedOnServer {
		t.Fatalf("want ChangedOnServer, got %s", r)
	}
	if len(internalFiles(t, srv)) != 1 {
		t.Fatal("the sent content should be kept as one pending temp file")
	}
	if err := a.e.Skip(a.file("r.txt"), "r.txt"); err != nil {
		t.Fatal(err)
	}
	if left := internalFiles(t, srv); len(left) != 0 {
		t.Fatalf("skip should remove the temp, left %v", left)
	}
}

func TestConflictAfterSendReusesTemp(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{})
	a, b := newMachine(t, srv), newMachine(t, srv)
	a.write("r.txt", "v1")
	a.upload("r.txt", "r.txt", Check)
	a.write("r.txt", "a2")
	a.e.beforeCommit = func() {
		b.write("x", "b2")
		b.e.Upload(bg, b.file("x"), "r.txt", Replace)
	}
	if _, err := a.upload("r.txt", "r.txt", Check); !errors.Is(err, ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}

	a.e.beforeCommit = nil
	pendingBefore := internalFiles(t, srv)
	if _, err := a.upload("r.txt", "r.txt", Replace); err != nil {
		t.Fatal(err)
	}
	if srv.Read("r.txt") != "a2" {
		t.Fatal("replace should put a2 in place")
	}
	if len(pendingBefore) != 1 || srv.Exists(strings.TrimPrefix(pendingBefore[0], srv.Root+"/")) {
		t.Fatal("replace should have used (renamed) the pending temp")
	}
}

func TestOutsideChangeIsDetectedAndStaleHashRemoved(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{})
	a := newMachine(t, srv)
	a.write("r.txt", "v1")
	a.upload("r.txt", "r.txt", Check)
	srv.Write("r.txt", "changed with scp") // hash file not updated

	a.write("r.txt", "a2")
	_, err := a.upload("r.txt", "r.txt", Check)
	if r := conflictReason(t, err); r != ChangedOutside {
		t.Fatalf("want ChangedOutside, got %s", r)
	}
}

func TestDownloadRepairsMissingHashFile(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{})
	srv.Write("legacy.txt", "uploaded with scp")
	a := newMachine(t, srv)
	a.download("legacy.txt", "legacy.txt")
	if got := srv.Read(".legacy.txt.sha256"); got != hashFile("uploaded with scp") {
		t.Fatalf("hash file should be repaired, got %q", got)
	}
	if srv.Read("legacy.txt") != "uploaded with scp" || len(internalFiles(t, srv)) != 0 {
		t.Fatal("repair must restore the file and leave no claim")
	}
	// Now an edit goes through without a prompt.
	a.write("legacy.txt", "edited")
	if _, err := a.upload("legacy.txt", "legacy.txt", Check); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadRepairsStaleHashFile(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{})
	srv.Write("r.txt", "new content")
	srv.Write(".r.txt.sha256", hashFile("old"))
	a := newMachine(t, srv)
	res, err := a.e.Download(bg, "r.txt", a.file("r.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Hash != sha("new content") || srv.Read(".r.txt.sha256") != hashFile("new content") {
		t.Fatal("download must record and repair with the real hash")
	}
}

func TestSimultaneousUploadsAllButOnePrompt(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{})
	const n = 8
	machines := make([]*machine, n)
	for i := range machines {
		machines[i] = newMachine(t, srv)
		machines[i].e.ClaimWait = 5 * time.Second
	}
	machines[0].write("r.txt", "v1")
	machines[0].upload("r.txt", "r.txt", Check)
	for i, m := range machines {
		m.download("r.txt", "r.txt")
		m.write("r.txt", fmt.Sprintf("edit by %d", i))
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i, m := range machines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = m.upload("r.txt", "r.txt", Check)
		}()
	}
	close(start)
	wg.Wait()
	t.Logf("conflicts found after sending content: %d", len(internalFiles(t, srv)))

	winners := 0
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
			if srv.Read("r.txt") != fmt.Sprintf("edit by %d", i) {
				t.Fatalf("server content %q is not the winner's", srv.Read("r.txt"))
			}
		case errors.Is(err, ErrConflict):
			machines[i].e.Skip(machines[i].file("r.txt"), "r.txt")
		default:
			t.Fatalf("machine %d: %v", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("want exactly one upload without prompt, got %d", winners)
	}
	if left := internalFiles(t, srv); len(left) != 0 {
		t.Fatalf("leftover files: %v", left)
	}
}

func TestSimultaneousNewFilesAllButOnePrompt(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{})
	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		m := newMachine(t, srv)
		m.write("new.txt", fmt.Sprintf("from %d", i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = m.upload("new.txt", "new.txt", Check)
		}()
	}
	close(start)
	wg.Wait()
	winners := 0
	for i, err := range errs {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatalf("machine %d: %v", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("want exactly one creator, got %d", winners)
	}
}

func TestCrashedClaimIsRecovered(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{})
	a := newMachine(t, srv)
	a.write("r.txt", "v1")
	a.upload("r.txt", "r.txt", Check)
	a.download("r.txt", "r.txt")
	// Simulate a client that crashed while holding the claim.
	if err := os.Rename(srv.Path("r.txt"), srv.Path(remote.ClaimName("r.txt"))); err != nil {
		t.Fatal(err)
	}

	a.write("r.txt", "v2")
	if _, err := a.upload("r.txt", "r.txt", Check); err != nil {
		t.Fatalf("upload should wait, recover the claim and succeed: %v", err)
	}
	if srv.Read("r.txt") != "v2" || srv.Exists(remote.ClaimName("r.txt")) {
		t.Fatal("claim should be recovered and replaced")
	}
}

func TestRecover(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{})
	a := newMachine(t, srv)
	srv.Write("d/.a.txt.claim-1", "a")
	srv.Write("d/b.txt", "b")
	srv.Write("d/.b.txt.claim-2", "old b") // target exists: left alone
	n, err := a.e.Recover("d")
	if err != nil || n != 1 || srv.Read("d/a.txt") != "a" || !srv.Exists("d/.b.txt.claim-2") {
		t.Fatalf("recover: n=%d err=%v", n, err)
	}
}

func TestMoveKeepsTransferListInStep(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{})
	a := newMachine(t, srv)
	a.write("r.txt", "v1")
	a.upload("r.txt", "docs/r.txt", Check)
	if err := a.e.Move("docs", "archive/docs"); err != nil {
		t.Fatal(err)
	}
	a.write("r.txt", "v2")
	if _, err := a.upload("r.txt", "archive/docs/r.txt", Check); err != nil {
		t.Fatalf("upload after own move should not prompt: %v", err)
	}
}

func TestInternalTargetsRefused(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{})
	a := newMachine(t, srv)
	a.write("x", "x")
	if _, err := a.upload("x", ".r.txt.sha256", Check); !errors.Is(err, remote.ErrInvalidPath) {
		t.Fatalf("want ErrInvalidPath, got %v", err)
	}
}

func TestOverwritingServerStillSafe(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{Overwriting: true})
	a, b := newMachine(t, srv), newMachine(t, srv)
	if replaces, err := a.e.c.RenameReplaces(); err != nil || !replaces {
		t.Fatalf("probe should detect an overwriting rename, got %v %v", replaces, err)
	}
	a.write("r.txt", "v1")
	a.upload("r.txt", "r.txt", Check)
	b.download("r.txt", "r.txt")
	a.write("r.txt", "a2")
	a.upload("r.txt", "r.txt", Check)
	b.write("r.txt", "b2")
	if _, err := b.upload("r.txt", "r.txt", Check); !errors.Is(err, ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	if srv.Read("r.txt") != "a2" {
		t.Fatal("server must keep a2")
	}
}

func TestOpenSSHServerDoesNotReplace(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{})
	a := newMachine(t, srv)
	if replaces, err := a.e.c.RenameReplaces(); err != nil || replaces {
		t.Fatalf("OpenSSH-like server must not replace, got %v %v", replaces, err)
	}
}

// localCopy stands in for the copy-data extension, which the test server
// does not have: it copies the file on the server's filesystem directly.
func localCopy(srv *sshtest.Server) func(context.Context, string, string) error {
	return func(_ context.Context, from, to string) error {
		data, err := os.ReadFile(srv.Path(from))
		if err != nil {
			return err
		}
		f, err := os.OpenFile(srv.Path(to), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.Write(data)
		return err
	}
}

func TestCopyCarriesHashFile(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{})
	a := newMachine(t, srv)
	if a.e.CanCopy() {
		t.Fatal("the test server has no copy-data")
	}
	if err := a.e.Copy(bg, "x", "y"); !errors.Is(err, remote.ErrCopyUnsupported) {
		t.Fatalf("want ErrCopyUnsupported, got %v", err)
	}
	a.e.copyData, a.e.canCopy = localCopy(srv), func() bool { return true }

	a.write("r.txt", "v1")
	a.upload("r.txt", "docs/r.txt", Check)
	if err := a.e.Copy(bg, "docs/r.txt", "other/r copy.txt"); err != nil {
		t.Fatal(err)
	}
	if srv.Read("other/r copy.txt") != "v1" || srv.Read("other/.r copy.txt.sha256") != hashFile("v1") {
		t.Fatal("copy should have the content and the hash file")
	}
	if err := a.e.Copy(bg, "docs/r.txt", "other/r copy.txt"); !errors.Is(err, remote.ErrExists) {
		t.Fatalf("copy must never overwrite, got %v", err)
	}
	if left := internalFiles(t, srv); len(left) != 0 {
		t.Fatalf("leftover files: %v", left)
	}

	// A source without a valid hash file gives an "unknown version" copy.
	srv.Write("legacy.txt", "scp")
	if err := a.e.Copy(bg, "legacy.txt", "legacy2.txt"); err != nil {
		t.Fatal(err)
	}
	if srv.Exists(".legacy2.txt.sha256") {
		t.Fatal("no hash file may be invented for content nobody hashed")
	}
}

func TestExistingTargetMissingAtCommitPrompts(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{})
	a := newMachine(t, srv)
	a.write("r.txt", "v1")
	a.upload("r.txt", "r.txt", Check)
	a.write("r.txt", "v2")
	// The file disappears after the check (deleted, moved, or claimed by a
	// client whose claim a folder listing did not show).
	a.e.beforeCommit = func() { os.Remove(srv.Path("r.txt")) }
	_, err := a.upload("r.txt", "r.txt", Check)
	if r := conflictReason(t, err); r != MissingMeanwhile {
		t.Fatalf("want MissingMeanwhile, got %s", r)
	}
	if srv.Exists("r.txt") {
		t.Fatal("an upload that started on an existing file must not recreate it unasked")
	}
	a.e.beforeCommit = nil
	if res, err := a.upload("r.txt", "r.txt", Replace); err != nil || srv.Read("r.txt") != "v2" {
		t.Fatalf("replace should recreate it: %+v %v", res, err)
	}
}
