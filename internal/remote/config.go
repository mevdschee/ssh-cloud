// Package remote is the SSH/SFTP layer of SSH-Cloud. It connects to the server
// and performs all metadata operations (list, stat, rename, move, delete)
// directly on the server. Paths passed to this package are always relative to
// the cloud root and use forward slashes, for example "docs/2026/report.docx".
package remote

import (
	"net"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"
)

// Config describes one connection profile.
type Config struct {
	Host string
	Port int // defaults to 22
	User string

	// Root is the absolute path of the cloud folder on the server.
	Root string

	// KeyFile is an optional path to a private key. When empty, only the
	// ssh-agent is used.
	KeyFile string
	// UseAgent enables keys from the running ssh-agent.
	UseAgent bool
	// Passphrase is called when KeyFile is encrypted. May be nil.
	Passphrase func(keyFile string) ([]byte, error)

	// KnownHostsFile is the known_hosts file used to verify the server.
	KnownHostsFile string
	// TrustHost is called for a host that is not in KnownHostsFile yet. When
	// it returns true the key is appended to KnownHostsFile. May be nil, in
	// which case unknown hosts are rejected.
	TrustHost func(hostport string, key ssh.PublicKey) bool

	// Timeout for dialing and the SSH handshake. Defaults to 15s.
	Timeout time.Duration
	// KeepAlive interval. Defaults to 30s. Negative disables keepalives.
	KeepAlive time.Duration
}

func (c Config) addr() string {
	port := c.Port
	if port == 0 {
		port = 22
	}
	return net.JoinHostPort(c.Host, strconv.Itoa(port))
}

func (c Config) timeout() time.Duration {
	if c.Timeout <= 0 {
		return 15 * time.Second
	}
	return c.Timeout
}

func (c Config) keepAlive() time.Duration {
	if c.KeepAlive == 0 {
		return 30 * time.Second
	}
	return c.KeepAlive
}
