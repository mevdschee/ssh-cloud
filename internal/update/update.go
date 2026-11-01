// Package update checks GitHub for a newer SSH-Cloud release and downloads
// its MSI (ported from otp4win). Installing is left to msiexec.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// LatestReleaseURL is the GitHub API endpoint for the newest release.
const LatestReleaseURL = "https://api.github.com/repos/mevdschee/ssh-cloud/releases/latest"

// Release is the part of the GitHub release JSON the update check needs.
type Release struct {
	Version string // without the leading "v"
	MsiURL  string
	MsiName string
	MsiSize int64
}

// ParseRelease reads a GitHub "latest release" response and finds the MSI.
func ParseRelease(data []byte) (Release, bool) {
	var r struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(data, &r); err != nil || r.Draft || r.Prerelease {
		return Release{}, false
	}
	for _, a := range r.Assets {
		if strings.HasSuffix(strings.ToLower(a.Name), ".msi") && strings.HasPrefix(a.URL, "https://") {
			return Release{
				Version: strings.TrimPrefix(r.TagName, "v"),
				MsiURL:  a.URL, MsiName: a.Name, MsiSize: a.Size,
			}, true
		}
	}
	return Release{}, false
}

// Newer reports whether version a is newer than b. Versions are dotted
// numbers like "0.2.0"; anything else (such as "dev") is never newer.
func Newer(a, b string) bool {
	pa, okA := parseVersion(a)
	pb, okB := parseVersion(b)
	if !okA || !okB {
		return false
	}
	for i := range max(len(pa), len(pb)) {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// IsRelease reports whether a version is a release version (development
// builds are "dev" and skip the update check).
func IsRelease(v string) bool {
	_, ok := parseVersion(v)
	return ok
}

func parseVersion(s string) ([]int, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if s == "" {
		return nil, false
	}
	var parts []int
	for _, p := range strings.Split(s, ".") {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, false
		}
		parts = append(parts, n)
	}
	return parts, true
}

// Latest asks GitHub for the newest release.
func Latest(ctx context.Context, current string) (Release, bool, error) {
	data, err := httpGet(ctx, LatestReleaseURL, current, 1<<20)
	if err != nil {
		return Release{}, false, err
	}
	r, ok := ParseRelease(data)
	return r, ok, nil
}

func httpGet(ctx context.Context, url, current string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ssh-cloud/"+current)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// Download saves the MSI in dir, reporting bytes done, and checks its size.
func Download(ctx context.Context, r Release, dir string, progress func(done, total int64)) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", r.MsiURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed: %s", resp.Status)
	}
	path := filepath.Join(dir, filepath.Base(r.MsiName))
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	total := r.MsiSize
	if total <= 0 {
		total = resp.ContentLength
	}
	var n int64
	buf := make([]byte, 64*1024)
	body := io.LimitReader(resp.Body, 200<<20)
	for {
		m, rerr := body.Read(buf)
		if m > 0 {
			if _, werr := f.Write(buf[:m]); werr != nil {
				err = werr
				break
			}
			n += int64(m)
			if progress != nil {
				progress(n, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			err = rerr
			break
		}
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && r.MsiSize > 0 && n != r.MsiSize {
		err = fmt.Errorf("download incomplete: %d of %d bytes", n, r.MsiSize)
	}
	if err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}
