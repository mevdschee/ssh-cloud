package config

import (
	"os"

	"github.com/mevdschee/ssh-cloud/internal/remote"
	"path/filepath"
	"testing"
)

func TestPutUpdateDeleteSurviveSave(t *testing.T) {
	dir := t.TempDir()
	c := &Config{}
	a := c.Put(Profile{Name: "work", Host: "h1", User: "team"})
	b := c.Put(Profile{Name: "home", Host: "h2", User: "me"})
	if a.ID == "" || a.ID == b.ID {
		t.Fatal("profiles need distinct IDs")
	}
	logBefore := a.TransferLogFile(dir)
	a.Name = "work renamed"
	c.Put(a)
	c.SetLast(a.ID)
	if err := c.Save(dir); err != nil {
		t.Fatal(err)
	}

	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	last, ok := c.Last()
	if !ok || last.Name != "work renamed" || len(c.Profiles) != 2 {
		t.Fatalf("reload: %+v", c)
	}
	if last.TransferLogFile(dir) != logBefore {
		t.Fatal("renaming must keep the transfer list file")
	}
	c.Delete(a.ID)
	if _, ok := c.Last(); ok || len(c.Profiles) != 1 {
		t.Fatal("delete should remove the profile and forget it as last")
	}
}

func TestOldConfigWithoutIDsKeepsTransferList(t *testing.T) {
	dir := t.TempDir()
	old := `{"profiles":[{"name":"team@host","host":"host","user":"team"}],"lastProfile":"team@host"}`
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(old), 0o600)
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := c.Last()
	if !ok {
		t.Fatal("old lastProfile (a name) should still resolve")
	}
	if got := filepath.Base(p.TransferLogFile(dir)); got != "transfers-team@host.jsonl" {
		t.Fatalf("old transfer list name should be kept, got %s", got)
	}
	if p.Auth != remote.AuthAuto {
		t.Fatalf("old profile without key file should become automatic, got %q", p.Auth)
	}
}
