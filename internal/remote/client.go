package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// ErrClosed is returned by Err after Close was called.
var ErrClosed = errors.New("connection closed")

// Client is one SSH connection with an SFTP session on top. It is safe for
// concurrent use; SFTP requests are multiplexed over the single connection.
type Client struct {
	ssh   *ssh.Client
	sftp  *sftp.Client
	agent io.Closer
	root  string // absolute, cleaned server path of the cloud folder

	done      chan struct{}
	closeOnce sync.Once
	mu        sync.Mutex
	err       error
}

// Dial connects, authenticates with a public key, verifies the host key and
// opens the SFTP session. The cloud root must exist and be a directory.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	auth, agentConn, err := authMethods(cfg)
	if err != nil {
		return nil, err
	}
	closeAgent := func() {
		if agentConn != nil {
			agentConn.Close()
		}
	}

	hostKey, err := hostKeyCallback(cfg)
	if err != nil {
		closeAgent()
		return nil, err
	}

	sshCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            auth,
		HostKeyCallback: hostKey,
		Timeout:         cfg.timeout(),
	}

	dialer := net.Dialer{Timeout: cfg.timeout()}
	conn, err := dialer.DialContext(ctx, "tcp", cfg.addr())
	if err != nil {
		closeAgent()
		return nil, fmt.Errorf("connect %s: %w", cfg.addr(), err)
	}
	// Bound the handshake; cleared once the connection is up.
	_ = conn.SetDeadline(time.Now().Add(cfg.timeout()))
	cc, chans, reqs, err := ssh.NewClientConn(conn, cfg.addr(), sshCfg)
	if err != nil {
		conn.Close()
		closeAgent()
		return nil, fmt.Errorf("ssh handshake: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})
	sshClient := ssh.NewClient(cc, chans, reqs)

	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		sshClient.Close()
		closeAgent()
		return nil, fmt.Errorf("start sftp: %w", err)
	}

	root := cfg.Root
	if root == "" {
		if root, err = sftpClient.Getwd(); err != nil {
			sftpClient.Close()
			sshClient.Close()
			closeAgent()
			return nil, fmt.Errorf("determine home directory: %w", err)
		}
	}
	root = path.Clean("/" + root)
	fi, err := sftpClient.Stat(root)
	if err != nil || !fi.IsDir() {
		sftpClient.Close()
		sshClient.Close()
		closeAgent()
		if err == nil {
			err = errors.New("not a directory")
		}
		return nil, fmt.Errorf("cloud root %s: %w", root, err)
	}

	c := &Client{
		ssh:   sshClient,
		sftp:  sftpClient,
		agent: agentConn,
		root:  root,
		done:  make(chan struct{}),
	}
	go func() {
		err := sshClient.Wait()
		if err == nil {
			err = io.EOF
		}
		c.fail(fmt.Errorf("connection lost: %w", err))
	}()
	if ka := cfg.keepAlive(); ka > 0 {
		go c.keepAlive(ka)
	}
	return c, nil
}

// Root returns the absolute server path of the cloud folder.
func (c *Client) Root() string { return c.root }

// Done is closed when the connection is closed or lost.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err returns why the connection ended, or nil while it is up.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close closes the SFTP session and the SSH connection.
func (c *Client) Close() error {
	c.fail(ErrClosed)
	return nil
}

func (c *Client) fail(err error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		c.sftp.Close()
		c.ssh.Close()
		if c.agent != nil {
			c.agent.Close()
		}
		close(c.done)
	})
}

func (c *Client) keepAlive(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			if _, _, err := c.ssh.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				c.fail(fmt.Errorf("keepalive failed: %w", err))
				return
			}
		}
	}
}

// abs maps a cloud path to an absolute server path. The cloud path is cleaned
// first, so the result is always inside the root.
func (c *Client) abs(p string) string {
	return path.Join(c.root, Clean(p))
}
