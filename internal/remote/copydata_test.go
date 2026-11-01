package remote

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// fakeSFTP is a minimal SFTP v3 server for the copy-data conversation. It
// follows the wire format of OpenSSH's PROTOCOL document (section 3.7).
func fakeSFTP(t *testing.T, r io.Reader, w io.Writer, withCopyData bool) {
	s := &rawSFTP{r: bufio.NewReader(r), w: w}
	files := map[string]*os.File{}
	reply := func(id uint32, typ byte, payload []byte) {
		s.send(typ, append(binary.BigEndian.AppendUint32(nil, id), payload...))
	}
	status := func(id, code uint32) {
		var b []byte
		b = binary.BigEndian.AppendUint32(b, code)
		b = appendString(b, "msg")
		b = appendString(b, "")
		reply(id, sshFxpStatus, b)
	}
	for {
		p, err := s.recv()
		if err != nil {
			return
		}
		if p.typ == sshFxpInit {
			v := binary.BigEndian.AppendUint32(nil, 3)
			if withCopyData {
				v = appendString(v, "copy-data")
				v = appendString(v, "1")
			}
			s.send(sshFxpVersion, v)
			continue
		}
		id := binary.BigEndian.Uint32(p.data)
		d := p.data[4:]
		switch p.typ {
		case sshFxpOpen:
			path, rest, _ := readString(d)
			flags := binary.BigEndian.Uint32(rest)
			mode := os.O_RDONLY
			if flags&sshFxfWrite != 0 {
				mode = os.O_WRONLY
			}
			if flags&sshFxfCreat != 0 {
				mode |= os.O_CREATE
			}
			if flags&sshFxfExcl != 0 {
				mode |= os.O_EXCL
			}
			f, err := os.OpenFile(path, mode, 0o644)
			if errors.Is(err, fs.ErrNotExist) {
				status(id, sshFxNoSuchFile)
				continue
			} else if err != nil {
				status(id, 4)
				continue
			}
			h := "h" + strconv.Itoa(len(files))
			files[h] = f
			reply(id, sshFxpHandle, appendString(nil, h))
		case sshFxpExtended:
			name, rest, _ := readString(d)
			if name != "copy-data" || !withCopyData {
				status(id, sshFxOpUnsupported)
				continue
			}
			src, rest, _ := readString(rest)
			rOff := binary.BigEndian.Uint64(rest)
			rLen := binary.BigEndian.Uint64(rest[8:])
			dst, rest, _ := readString(rest[16:])
			wOff := binary.BigEndian.Uint64(rest)
			if rOff != 0 || rLen != 0 || wOff != 0 {
				t.Errorf("unexpected offsets %d %d %d", rOff, rLen, wOff)
			}
			if _, err := io.Copy(files[dst], files[src]); err != nil {
				status(id, 4)
				continue
			}
			status(id, sshFxOK)
		case sshFxpClose:
			h, _, _ := readString(d)
			files[h].Close()
			delete(files, h)
			status(id, sshFxOK)
		default:
			status(id, sshFxOpUnsupported)
		}
	}
}

func runCopy(t *testing.T, from, to string, withCopyData bool) error {
	cr, sw := io.Pipe() // server -> client
	sr, cw := io.Pipe() // client -> server
	go fakeSFTP(t, sr, sw, withCopyData)
	defer cw.Close()
	return copyData(&rawSFTP{w: cw, r: bufio.NewReader(cr)}, from, to)
}

func TestCopyData(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")
	os.WriteFile(from, []byte("content"), 0o644)
	if err := runCopy(t, from, to, true); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(to); string(data) != "content" {
		t.Fatalf("copied %q", data)
	}
	// The target is created exclusively: never overwritten.
	if err := runCopy(t, from, to, true); err == nil {
		t.Fatal("copy onto an existing file must fail")
	}
	if err := runCopy(t, filepath.Join(dir, "missing"), filepath.Join(dir, "c"), true); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
}

func TestCopyDataUnsupported(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join(dir, "a.txt")
	os.WriteFile(from, []byte("x"), 0o644)
	if err := runCopy(t, from, filepath.Join(dir, "b"), false); !errors.Is(err, ErrCopyUnsupported) {
		t.Fatalf("want ErrCopyUnsupported, got %v", err)
	}
}
