//go:build windows

package remote

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Pageant (PuTTY's agent) speaks the ssh-agent protocol over WM_COPYDATA:
// the request is written to a named file mapping, the name is sent to the
// Pageant window, and Pageant writes the reply into the same mapping. This
// works with every Pageant version.

const (
	pageantCopyDataID = 0x804e50ba // AGENT_COPYDATA_ID
	pageantMaxMsg     = 8192       // AGENT_MAX_MSGLEN
	wmCopyData        = 0x004A
)

var (
	user32              = syscall.NewLazyDLL("user32.dll")
	procFindWindowW     = user32.NewProc("FindWindowW")
	procSendMessageW    = user32.NewProc("SendMessageW")
	pageantRequestCount uint32
)

type copyDataStruct struct {
	dwData uintptr
	cbData uint32
	lpData uintptr
}

func pageantWindow() uintptr {
	name, _ := syscall.UTF16PtrFromString("Pageant")
	h, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(name)))
	return h
}

func dialPageant() (io.ReadWriteCloser, error) {
	if pageantWindow() == 0 {
		return nil, errors.New("Pageant is not running")
	}
	return &pageantConn{}, nil
}

// pageantConn adapts the request/response exchange to the stream the agent
// client expects: writes are collected until a whole message is there, then
// the query runs and the reply is read back.
type pageantConn struct {
	req  []byte
	resp []byte
}

func (p *pageantConn) Write(b []byte) (int, error) {
	p.req = append(p.req, b...)
	if len(p.req) >= 4 && len(p.req) >= 4+int(binary.BigEndian.Uint32(p.req)) {
		resp, err := pageantQuery(p.req)
		p.req = nil
		if err != nil {
			return 0, err
		}
		p.resp = append(p.resp, resp...)
	}
	return len(b), nil
}

func (p *pageantConn) Read(b []byte) (int, error) {
	if len(p.resp) == 0 {
		return 0, io.EOF
	}
	n := copy(b, p.resp)
	p.resp = p.resp[n:]
	return n, nil
}

func (p *pageantConn) Close() error { return nil }

func pageantQuery(msg []byte) ([]byte, error) {
	if len(msg) > pageantMaxMsg {
		return nil, errors.New("Pageant request too large")
	}
	hwnd := pageantWindow()
	if hwnd == 0 {
		return nil, errors.New("Pageant is not running")
	}
	mapName := fmt.Sprintf("PageantRequest%08x%08x", windows.GetCurrentThreadId(), atomic.AddUint32(&pageantRequestCount, 1))
	name16, _ := syscall.UTF16PtrFromString(mapName)
	h, err := windows.CreateFileMapping(windows.InvalidHandle, nil, windows.PAGE_READWRITE, 0, pageantMaxMsg, name16)
	if err != nil {
		return nil, fmt.Errorf("Pageant file mapping: %w", err)
	}
	defer windows.CloseHandle(h)
	addr, err := windows.MapViewOfFile(h, windows.FILE_MAP_WRITE, 0, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("Pageant map view: %w", err)
	}
	defer windows.UnmapViewOfFile(addr)
	// addr is OS memory from MapViewOfFile, not Go memory.
	buf := unsafe.Slice((*byte)(unsafe.Add(unsafe.Pointer(nil), addr)), pageantMaxMsg)
	copy(buf, msg)

	nameA := append([]byte(mapName), 0) // Pageant expects an ANSI name
	cds := copyDataStruct{dwData: pageantCopyDataID, cbData: uint32(len(nameA)), lpData: uintptr(unsafe.Pointer(&nameA[0]))}
	r, _, _ := procSendMessageW.Call(hwnd, wmCopyData, 0, uintptr(unsafe.Pointer(&cds)))
	if r == 0 {
		return nil, errors.New("Pageant refused the request")
	}
	n := binary.BigEndian.Uint32(buf)
	if n+4 > pageantMaxMsg {
		return nil, errors.New("Pageant reply too large")
	}
	return append([]byte(nil), buf[:4+n]...), nil
}
