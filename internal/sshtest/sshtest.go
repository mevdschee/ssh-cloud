// Package sshtest runs an in-process SSH server with an SFTP subsystem for
// tests. It serves the real local filesystem, so tests can set up and inspect
// server state with plain os calls.
//
// By default the server behaves like OpenSSH's sftp-server: a plain SFTP
// rename never overwrites an existing target. With Overwriting set it uses
// pkg/sftp's stock server, whose rename replaces the target.
package sshtest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/mevdschee/ssh-cloud/internal/remote"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Options configure the test server.
type Options struct {
	// Overwriting makes plain SFTP rename replace existing targets.
	Overwriting bool
}

// Server is a running test server with one cloud root.
type Server struct {
	Addr    string
	Root    string // cloud root (a local temp dir)
	HostKey ssh.Signer

	t         *testing.T
	clientKey ssh.Signer
	keyPEM    []byte
}

// NewKey generates an ed25519 key and returns it as signer and PEM.
func NewKey(t *testing.T) (ssh.Signer, []byte) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	return signer, pem.EncodeToMemory(block)
}

// Start starts a server that accepts one generated client key.
func Start(t *testing.T, opts Options) *Server {
	t.Helper()
	hostKey, _ := NewKey(t)
	clientKey, keyPEM := NewKey(t)
	s := &Server{Root: t.TempDir(), HostKey: hostKey, t: t, clientKey: clientKey, keyPEM: keyPEM}

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), clientKey.PublicKey().Marshal()) {
				return nil, nil
			}
			return nil, os.ErrPermission
		},
	}
	cfg.AddHostKey(hostKey)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveConn(conn, cfg, opts)
		}
	}()
	s.Addr = ln.Addr().String()
	return s
}

// Config returns a client config for this server that trusts its host key and
// uses the accepted client key, stored in a fresh temp dir.
func (s *Server) Config() remote.Config {
	s.t.Helper()
	local := s.t.TempDir()
	keyFile := filepath.Join(local, "id_ed25519")
	if err := os.WriteFile(keyFile, s.keyPEM, 0o600); err != nil {
		s.t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(s.Addr)
	portNum, _ := strconv.Atoi(port)
	return remote.Config{
		Host:           host,
		Port:           portNum,
		User:           "team",
		Root:           s.Root,
		KeyFile:        keyFile,
		KnownHostsFile: filepath.Join(local, "known_hosts"),
		TrustHost:      func(string, ssh.PublicKey) bool { return true },
		KeepAlive:      -1,
	}
}

// Dial connects a new client (closed at test end).
func (s *Server) Dial() *remote.Client {
	s.t.Helper()
	c, err := remote.Dial(context.Background(), s.Config())
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { c.Close() })
	return c
}

// Path returns the local path of a cloud path on the server.
func (s *Server) Path(rel string) string {
	return filepath.Join(s.Root, filepath.FromSlash(rel))
}

// Write creates a file on the server, including parent directories.
func (s *Server) Write(rel, content string) {
	s.t.Helper()
	p := s.Path(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

// Read returns a file's content on the server ("" if missing).
func (s *Server) Read(rel string) string {
	data, _ := os.ReadFile(s.Path(rel))
	return string(data)
}

// Exists reports whether a cloud path exists on the server.
func (s *Server) Exists(rel string) bool {
	_, err := os.Lstat(s.Path(rel))
	return err == nil
}

// KnownHostsLine formats a known_hosts line.
func KnownHostsLine(addr string, key ssh.PublicKey) string {
	return knownhosts.Line([]string{knownhosts.Normalize(addr)}, key)
}

func serveConn(conn net.Conn, cfg *ssh.ServerConfig, opts Options) {
	sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		conn.Close()
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		ch, chReqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range chReqs {
				ok := req.Type == "subsystem" && len(req.Payload) >= 4 && string(req.Payload[4:]) == "sftp"
				req.Reply(ok, nil)
				if ok {
					go serveSFTP(ch, opts)
				}
			}
		}()
	}
}

func serveSFTP(ch ssh.Channel, opts Options) {
	defer ch.Close()
	if opts.Overwriting {
		srv, err := sftp.NewServer(ch)
		if err == nil {
			srv.Serve()
		}
		return
	}
	h := osHandler{}
	srv := sftp.NewRequestServer(ch, sftp.Handlers{FileGet: h, FilePut: h, FileCmd: h, FileList: h})
	srv.Serve()
}

// osHandler serves the local filesystem with OpenSSH rename semantics.
type osHandler struct{}

func (osHandler) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	return os.Open(r.Filepath)
}

func (osHandler) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	pf := r.Pflags()
	flags := os.O_WRONLY
	if pf.Read {
		flags = os.O_RDWR
	}
	if pf.Creat {
		flags |= os.O_CREATE
	}
	if pf.Trunc {
		flags |= os.O_TRUNC
	}
	if pf.Excl {
		flags |= os.O_EXCL
	}
	if pf.Append {
		flags |= os.O_APPEND
	}
	return os.OpenFile(r.Filepath, flags, 0o644)
}

func (osHandler) Filecmd(r *sftp.Request) error {
	switch r.Method {
	case "Rename":
		// Like OpenSSH: link + unlink for files, so an existing target is
		// never replaced; an explicit check for directories.
		fi, err := os.Lstat(r.Filepath)
		if err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			if err := os.Link(r.Filepath, r.Target); err != nil {
				return err
			}
			if err := os.Remove(r.Filepath); err != nil {
				os.Remove(r.Target) // like OpenSSH: drop the spare link
				return err
			}
			return nil
		}
		if _, err := os.Lstat(r.Target); err == nil {
			return os.ErrExist
		}
		return os.Rename(r.Filepath, r.Target)
	case "Rmdir":
		fi, err := os.Lstat(r.Filepath)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return errors.New("not a directory")
		}
		return os.Remove(r.Filepath)
	case "Remove":
		return os.Remove(r.Filepath)
	case "Mkdir":
		return os.Mkdir(r.Filepath, 0o755)
	case "Setstat":
		if r.AttrFlags().Size {
			return os.Truncate(r.Filepath, int64(r.Attributes().Size))
		}
		return nil
	}
	return sftp.ErrSSHFxOpUnsupported
}

func (osHandler) PosixRename(r *sftp.Request) error {
	return os.Rename(r.Filepath, r.Target)
}

func (osHandler) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	switch r.Method {
	case "List":
		des, err := os.ReadDir(r.Filepath)
		if err != nil {
			return nil, err
		}
		infos := make([]os.FileInfo, 0, len(des))
		for _, de := range des {
			if fi, err := de.Info(); err == nil {
				infos = append(infos, fi)
			}
		}
		return listerAt(infos), nil
	case "Stat":
		fi, err := os.Stat(r.Filepath)
		if err != nil {
			return nil, err
		}
		return listerAt{fi}, nil
	}
	return nil, sftp.ErrSSHFxOpUnsupported
}

func (osHandler) Lstat(r *sftp.Request) (sftp.ListerAt, error) {
	fi, err := os.Lstat(r.Filepath)
	if err != nil {
		return nil, err
	}
	return listerAt{fi}, nil
}

type listerAt []os.FileInfo

func (l listerAt) ListAt(ls []os.FileInfo, offset int64) (int, error) {
	if offset >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(ls, l[offset:])
	if n < len(ls) {
		return n, io.EOF
	}
	return n, nil
}
