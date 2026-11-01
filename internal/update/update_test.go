package update

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"0.3.0", "0.2.0", true},
		{"v0.2.1", "0.2.0", true},
		{"0.10.0", "0.9.9", true},
		{"1.0", "0.9.9", true},
		{"0.2.0", "0.2.0", false},
		{"0.1.9", "0.2.0", false},
		{"0.2", "0.2.0", false},
		{"0.3.0", "", false},
		{"0.3.0", "dev", false},
		{"0.3.0-beta", "0.2.0", false},
	} {
		if got := Newer(tc.a, tc.b); got != tc.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
	if IsRelease("dev") || !IsRelease("0.1.0") {
		t.Error("IsRelease")
	}
}

func TestParseRelease(t *testing.T) {
	data := []byte(`{
		"tag_name": "v0.3.0", "draft": false, "prerelease": false,
		"assets": [
			{"name": "notes.txt", "browser_download_url": "https://example.com/notes.txt", "size": 10},
			{"name": "ssh-cloud-0.3.0-amd64.msi", "browser_download_url": "https://github.com/mevdschee/ssh-cloud/releases/download/v0.3.0/ssh-cloud-0.3.0-amd64.msi", "size": 3912192}
		]}`)
	r, ok := ParseRelease(data)
	if !ok {
		t.Fatal("no release found")
	}
	want := Release{Version: "0.3.0", MsiName: "ssh-cloud-0.3.0-amd64.msi", MsiSize: 3912192,
		MsiURL: "https://github.com/mevdschee/ssh-cloud/releases/download/v0.3.0/ssh-cloud-0.3.0-amd64.msi"}
	if r != want {
		t.Errorf("got %+v", r)
	}
	for name, bad := range map[string]string{
		"no msi":     `{"tag_name": "v0.3.0", "assets": [{"name": "a.zip", "browser_download_url": "https://x/a.zip"}]}`,
		"prerelease": `{"tag_name": "v0.3.0", "prerelease": true, "assets": [{"name": "a.msi", "browser_download_url": "https://x/a.msi"}]}`,
		"not https":  `{"tag_name": "v0.3.0", "assets": [{"name": "a.msi", "browser_download_url": "http://x/a.msi"}]}`,
		"invalid":    `not json`,
	} {
		if _, ok := ParseRelease([]byte(bad)); ok {
			t.Errorf("%s: expected no release", name)
		}
	}
}

func TestDownload(t *testing.T) {
	body := strings.Repeat("m", 200000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Write([]byte(body))
	}))
	defer srv.Close()
	dir := t.TempDir()
	var last int64
	r := Release{MsiURL: srv.URL, MsiName: "../evil/x.msi", MsiSize: int64(len(body))}
	path, err := Download(context.Background(), r, dir, func(done, total int64) { last = done })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, dir) || last != int64(len(body)) {
		t.Fatalf("path %s, progress %d", path, last)
	}
	data, _ := os.ReadFile(path)
	if len(data) != len(body) {
		t.Fatal("incomplete file")
	}
	r.MsiSize = 5
	if _, err := Download(context.Background(), r, dir, nil); err == nil {
		t.Fatal("a size mismatch must fail")
	}
}
