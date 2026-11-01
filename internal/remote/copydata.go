package remote

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
)

// ErrCopyUnsupported is returned when the server has no copy-data extension.
var ErrCopyUnsupported = errors.New("the server does not support server-side copy (copy-data)")

// CanCopy reports whether the server supports server-side copy with the
// copy-data extension (OpenSSH 9.0 and later).
func (c *Client) CanCopy() bool {
	_, ok := c.sftp.HasExtension("copy-data")
	return ok
}

// RawCopyData copies the content of file from into a new file to (created
// exclusively; it must not exist) entirely on the server, with the
// copy-data extension. pkg/sftp has no API for it, so this runs a minimal
// SFTP v3 conversation (open, copy-data, close) on a session of its own.
func (c *Client) RawCopyData(ctx context.Context, from, to string) error {
	if !c.CanCopy() {
		return ErrCopyUnsupported
	}
	sess, err := c.ssh.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	stdin, err := sess.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		return err
	}
	if err := sess.RequestSubsystem("sftp"); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { sess.Close() })
	defer stop()

	err = copyData(&rawSFTP{w: stdin, r: bufio.NewReader(stdout)}, c.abs(from), c.abs(to))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		slog.Error("copy-data failed", "from", from, "to", to, "err", err)
		return fmt.Errorf("copy %q to %q: %w", from, to, err)
	}
	slog.Info("copied on server", "from", from, "to", to)
	return nil
}

func copyData(s *rawSFTP, from, to string) error {
	exts, err := s.init()
	if err != nil {
		return err
	}
	if _, ok := exts["copy-data"]; !ok {
		return ErrCopyUnsupported
	}
	src, err := s.open(from, sshFxfRead)
	if err != nil {
		return err
	}
	defer s.close(src)
	dst, err := s.open(to, sshFxfWrite|sshFxfCreat|sshFxfExcl)
	if err != nil {
		return err
	}
	var req []byte
	req = appendString(req, "copy-data")
	req = appendString(req, src)
	req = binary.BigEndian.AppendUint64(req, 0) // read-from-offset
	req = binary.BigEndian.AppendUint64(req, 0) // read-data-length: 0 = until EOF
	req = appendString(req, dst)
	req = binary.BigEndian.AppendUint64(req, 0) // write-to-offset
	if err := s.status(s.request(sshFxpExtended, req)); err != nil {
		s.close(dst)
		return err
	}
	return s.close(dst)
}

// SFTP v3 packet types and open flags (draft-ietf-secsh-filexfer-02).
const (
	sshFxpInit     = 1
	sshFxpVersion  = 2
	sshFxpOpen     = 3
	sshFxpClose    = 4
	sshFxpStatus   = 101
	sshFxpHandle   = 102
	sshFxpExtended = 200

	sshFxfRead  = 0x01
	sshFxfWrite = 0x02
	sshFxfCreat = 0x08
	sshFxfExcl  = 0x20

	sshFxOK            = 0
	sshFxNoSuchFile    = 2
	sshFxPermission    = 3
	sshFxOpUnsupported = 8
)

// rawSFTP is a minimal, sequential SFTP v3 client.
type rawSFTP struct {
	w      io.Writer
	r      io.Reader
	nextID uint32
}

type packet struct {
	typ  byte
	data []byte // after the type byte
}

func (s *rawSFTP) send(typ byte, payload []byte) error {
	buf := make([]byte, 5, 5+len(payload))
	binary.BigEndian.PutUint32(buf, uint32(1+len(payload)))
	buf[4] = typ
	_, err := s.w.Write(append(buf, payload...))
	return err
}

func (s *rawSFTP) recv() (packet, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(s.r, hdr[:]); err != nil {
		return packet{}, err
	}
	n := binary.BigEndian.Uint32(hdr[:4])
	if n < 1 || n > 256*1024 {
		return packet{}, fmt.Errorf("sftp: bad packet length %d", n)
	}
	data := make([]byte, n-1)
	if _, err := io.ReadFull(s.r, data); err != nil {
		return packet{}, err
	}
	return packet{typ: hdr[4], data: data}, nil
}

// request sends a packet with a fresh id and returns the reply.
func (s *rawSFTP) request(typ byte, payload []byte) (packet, error) {
	s.nextID++
	id := s.nextID
	if err := s.send(typ, append(binary.BigEndian.AppendUint32(nil, id), payload...)); err != nil {
		return packet{}, err
	}
	p, err := s.recv()
	if err != nil {
		return packet{}, err
	}
	if len(p.data) < 4 || binary.BigEndian.Uint32(p.data) != id {
		return packet{}, errors.New("sftp: unexpected reply id")
	}
	p.data = p.data[4:]
	return p, nil
}

func (s *rawSFTP) init() (map[string]string, error) {
	if err := s.send(sshFxpInit, binary.BigEndian.AppendUint32(nil, 3)); err != nil {
		return nil, err
	}
	p, err := s.recv()
	if err != nil {
		return nil, err
	}
	if p.typ != sshFxpVersion || len(p.data) < 4 {
		return nil, errors.New("sftp: no version reply")
	}
	exts := map[string]string{}
	rest := p.data[4:]
	for len(rest) > 0 {
		var name, value string
		var ok bool
		if name, rest, ok = readString(rest); !ok {
			break
		}
		if value, rest, ok = readString(rest); !ok {
			break
		}
		exts[name] = value
	}
	return exts, nil
}

func (s *rawSFTP) open(path string, flags uint32) (string, error) {
	var req []byte
	req = appendString(req, path)
	req = binary.BigEndian.AppendUint32(req, flags)
	req = binary.BigEndian.AppendUint32(req, 0) // no attributes
	p, err := s.request(sshFxpOpen, req)
	if err != nil {
		return "", err
	}
	if p.typ == sshFxpStatus {
		return "", statusError(p.data, path)
	}
	handle, _, ok := readString(p.data)
	if p.typ != sshFxpHandle || !ok {
		return "", errors.New("sftp: bad open reply")
	}
	return handle, nil
}

func (s *rawSFTP) close(handle string) error {
	return s.status(s.request(sshFxpClose, appendString(nil, handle)))
}

func (s *rawSFTP) status(p packet, err error) error {
	if err != nil {
		return err
	}
	if p.typ != sshFxpStatus {
		return errors.New("sftp: expected status reply")
	}
	return statusError(p.data, "")
}

func statusError(data []byte, path string) error {
	if len(data) < 4 {
		return errors.New("sftp: short status")
	}
	code := binary.BigEndian.Uint32(data)
	msg, _, _ := readString(data[4:])
	switch code {
	case sshFxOK:
		return nil
	case sshFxNoSuchFile:
		return fmt.Errorf("%s: %w", path, fs.ErrNotExist)
	case sshFxPermission:
		return fmt.Errorf("%s: %w", path, fs.ErrPermission)
	case sshFxOpUnsupported:
		return ErrCopyUnsupported
	}
	return fmt.Errorf("sftp status %d: %s", code, msg)
}

func appendString(b []byte, s string) []byte {
	b = binary.BigEndian.AppendUint32(b, uint32(len(s)))
	return append(b, s...)
}

func readString(b []byte) (string, []byte, bool) {
	if len(b) < 4 {
		return "", nil, false
	}
	n := binary.BigEndian.Uint32(b)
	if uint32(len(b)-4) < n {
		return "", nil, false
	}
	return string(b[4 : 4+n]), b[4+n:], true
}
