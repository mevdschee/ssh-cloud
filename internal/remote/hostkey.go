package remote

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// ErrHostKeyMismatch means the server presented a different key than the one
// in known_hosts. This is always a hard error.
var ErrHostKeyMismatch = errors.New("host key mismatch: the server key differs from known_hosts")

// ErrHostUnknown means the host is not in known_hosts and was not trusted.
var ErrHostUnknown = errors.New("host key unknown and not trusted")

var knownHostsMu sync.Mutex

// hostKeyCallback verifies the server against known_hosts. Unknown hosts are
// passed to cfg.TrustHost (trust on first use); mismatches are rejected.
func hostKeyCallback(cfg Config) (ssh.HostKeyCallback, error) {
	if cfg.KnownHostsFile == "" {
		return nil, errors.New("no known_hosts file configured")
	}
	if err := ensureFile(cfg.KnownHostsFile); err != nil {
		return nil, err
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		knownHostsMu.Lock()
		defer knownHostsMu.Unlock()

		check, err := knownhosts.New(cfg.KnownHostsFile)
		if err != nil {
			return fmt.Errorf("read known_hosts: %w", err)
		}
		err = check(hostname, remote, key)
		var keyErr *knownhosts.KeyError
		if !errors.As(err, &keyErr) {
			return err // nil (known and matching) or another error
		}
		fp := ssh.FingerprintSHA256(key)
		if len(keyErr.Want) > 0 {
			slog.Error("host key mismatch", "host", hostname, "fingerprint", fp, "knownHosts", cfg.KnownHostsFile)
			return ErrHostKeyMismatch
		}
		if cfg.TrustHost == nil || !cfg.TrustHost(hostname, key) {
			slog.Warn("unknown host not trusted", "host", hostname, "fingerprint", fp)
			return ErrHostUnknown
		}
		slog.Info("host trusted and added to known_hosts", "host", hostname, "fingerprint", fp, "knownHosts", cfg.KnownHostsFile)
		return appendKnownHost(cfg.KnownHostsFile, hostname, key)
	}, nil
}

func appendKnownHost(file, hostname string, key ssh.PublicKey) error {
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
	_, err = f.WriteString(line + "\n")
	return err
}

func ensureFile(file string) error {
	if _, err := os.Stat(file); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}
