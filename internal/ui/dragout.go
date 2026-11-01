//go:build windows

package ui

// Dragging cloud files out to Windows Explorer (or the desktop).
//
// A drag offers two clipboard formats on a COM IDataObject implemented here:
//   - "FileGroupDescriptorW": names, sizes and times of every file and folder,
//     built up front from the listing (metadata only);
//   - "FileContents": the content of file number lindex, rendered only when
//     Explorer asks for it after the drop. Only then is the file downloaded
//     (through the transfer engine, so the transfer list records its hash),
//     into a temp folder, and handed to Explorer as an IStream on that file.
//
// IDropSource is the small companion object DoDragDrop needs.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/mevdschee/ssh-cloud/internal/dragdata"
	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/win"
)

var (
	ole32                      = syscall.NewLazyDLL("ole32.dll")
	procDoDragDrop             = ole32.NewProc("DoDragDrop")
	shell32                    = syscall.NewLazyDLL("shell32.dll")
	procSHCreateStdEnumFmtEtc  = shell32.NewProc("SHCreateStdEnumFmtEtc")
	shlwapi                    = syscall.NewLazyDLL("shlwapi.dll")
	procSHCreateStreamOnFileEx = shlwapi.NewProc("SHCreateStreamOnFileEx")
)

// HRESULTs and constants (winerror.h, objidl.h, oleidl.h).
const (
	sOK                        = 0x00000000
	eNotImpl                   = 0x80004001
	eNoInterface               = 0x80004002
	ePointer                   = 0x80004003
	eAbort                     = 0x80004004
	eFail                      = 0x80004005
	dvEFormatEtc               = 0x80040064
	dvELindex                  = 0x80040068
	dvETymed                   = 0x80040069
	dvEDVAspect                = 0x8004006B
	oleEAdviseNotSupported     = 0x80040003
	dataSSameFormatEtc         = 0x00040130
	dragdropSDrop              = 0x00040100
	dragdropSCancel            = 0x00040101
	dragdropSUseDefaultCursors = 0x00040102

	tymedHGlobal    = 1
	tymedIStream    = 4
	dvaspectContent = 1
	datadirGet      = 1
	dropEffectCopy  = 1
	mkLButton       = 0x0001
	stgmRead        = 0x00000000
	stgmShareDenyWr = 0x00000020
)

type guid struct {
	d1     uint32
	d2, d3 uint16
	d4     [8]byte
}

var (
	iidIUnknown    = guid{0x00000000, 0, 0, [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidIDataObject = guid{0x0000010E, 0, 0, [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidIDropSource = guid{0x00000121, 0, 0, [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
)

// formatEtc is FORMATETC with its x64 layout (32 bytes).
type formatEtc struct {
	cfFormat uint16
	_        [6]byte
	ptd      uintptr
	aspect   uint32
	lindex   int32
	tymed    uint32
	_        uint32
}

// stgMedium is STGMEDIUM with its x64 layout (24 bytes).
type stgMedium struct {
	tymed         uint32
	_             uint32
	data          uintptr // HGLOBAL or IStream*
	unkForRelease uintptr
}

func init() {
	// The structs are passed to Windows; guard their layout.
	if unsafe.Sizeof(formatEtc{}) != 32 || unsafe.Sizeof(stgMedium{}) != 24 {
		panic("unexpected FORMATETC/STGMEDIUM layout")
	}
}

// live keeps COM objects reachable while Windows holds pointers to them.
var live sync.Map

// --- IDataObject ---

type dataObjectVtbl struct {
	QueryInterface, AddRef, Release                                    uintptr
	GetData, GetDataHere, QueryGetData, GetCanonicalFormatEtc, SetData uintptr
	EnumFormatEtc, DAdvise, DUnadvise, EnumDAdvise                     uintptr
}

type dataObject struct {
	vtbl *dataObjectVtbl // must be the first field: the object is the interface pointer
	refs int32

	items        []dragdata.Item
	descriptor   []byte
	cfDescriptor uint16
	cfContents   uint16
	fetch        func(i int) (string, error) // downloads item i, returns the local file
}

var dataObjectVt dataObjectVtbl
var dataObjectVtOnce sync.Once

func newDataObject(items []dragdata.Item, descriptor []byte, cfDescriptor, cfContents uint16, fetch func(int) (string, error)) *dataObject {
	dataObjectVtOnce.Do(func() {
		dataObjectVt = dataObjectVtbl{
			QueryInterface:        syscall.NewCallback(doQueryInterface),
			AddRef:                syscall.NewCallback(doAddRef),
			Release:               syscall.NewCallback(doRelease),
			GetData:               syscall.NewCallback(doGetData),
			GetDataHere:           syscall.NewCallback(func(this *dataObject, f *formatEtc, m *stgMedium) uintptr { return eNotImpl }),
			QueryGetData:          syscall.NewCallback(doQueryGetData),
			GetCanonicalFormatEtc: syscall.NewCallback(doGetCanonicalFormatEtc),
			SetData:               syscall.NewCallback(func(this *dataObject, f *formatEtc, m *stgMedium, release int32) uintptr { return eNotImpl }),
			EnumFormatEtc:         syscall.NewCallback(doEnumFormatEtc),
			DAdvise: syscall.NewCallback(func(this *dataObject, f *formatEtc, advf uint32, sink uintptr, conn *uint32) uintptr {
				return oleEAdviseNotSupported
			}),
			DUnadvise:   syscall.NewCallback(func(this *dataObject, conn uint32) uintptr { return oleEAdviseNotSupported }),
			EnumDAdvise: syscall.NewCallback(func(this *dataObject, ppenum *uintptr) uintptr { return oleEAdviseNotSupported }),
		}
	})
	obj := &dataObject{vtbl: &dataObjectVt, refs: 1, items: items, descriptor: descriptor,
		cfDescriptor: cfDescriptor, cfContents: cfContents, fetch: fetch}
	live.Store(obj, struct{}{})
	return obj
}

func doQueryInterface(this *dataObject, riid *guid, ppv *uintptr) uintptr {
	if ppv == nil {
		return ePointer
	}
	if riid != nil && (*riid == iidIUnknown || *riid == iidIDataObject) {
		*ppv = uintptr(unsafe.Pointer(this))
		atomic.AddInt32(&this.refs, 1)
		return sOK
	}
	*ppv = 0
	return eNoInterface
}

func doAddRef(this *dataObject) uintptr { return uintptr(atomic.AddInt32(&this.refs, 1)) }

func doRelease(this *dataObject) uintptr {
	n := atomic.AddInt32(&this.refs, -1)
	if n == 0 {
		live.Delete(this)
	}
	return uintptr(n)
}

// check validates a requested format; lindexAny accepts -1 for contents
// (used by QueryGetData).
func (this *dataObject) check(f *formatEtc, lindexAny bool) uintptr {
	if f == nil {
		return ePointer
	}
	if f.aspect != dvaspectContent {
		return dvEDVAspect
	}
	switch f.cfFormat {
	case this.cfDescriptor:
		if f.tymed&tymedHGlobal == 0 {
			return dvETymed
		}
		return sOK
	case this.cfContents:
		if f.tymed&tymedIStream == 0 {
			return dvETymed
		}
		if lindexAny && f.lindex == -1 {
			return sOK
		}
		if f.lindex < 0 || int(f.lindex) >= len(this.items) || this.items[f.lindex].IsDir {
			return dvELindex
		}
		return sOK
	}
	return dvEFormatEtc
}

func doQueryGetData(this *dataObject, f *formatEtc) uintptr { return this.check(f, true) }

func doGetCanonicalFormatEtc(this *dataObject, in, out *formatEtc) uintptr {
	if out == nil {
		return ePointer
	}
	if in != nil {
		*out = *in
	}
	out.ptd = 0
	return dataSSameFormatEtc
}

func doGetData(this *dataObject, f *formatEtc, m *stgMedium) (hr uintptr) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("drag GetData panic", "panic", fmt.Sprint(r))
			hr = eFail
		}
	}()
	if m == nil {
		return ePointer
	}
	if hr := this.check(f, false); hr != sOK {
		return hr
	}
	if f.cfFormat == this.cfDescriptor {
		h, err := globalFromBytes(this.descriptor)
		if err != nil {
			slog.Error("drag descriptor", "err", err)
			return eFail
		}
		*m = stgMedium{tymed: tymedHGlobal, data: uintptr(h)}
		return sOK
	}

	i := int(f.lindex)
	local, err := this.fetch(i)
	if errors.Is(err, context.Canceled) {
		slog.Info("drag download cancelled", "remote", this.items[i].Remote)
		return eAbort
	}
	if err != nil {
		slog.Error("drag download failed", "remote", this.items[i].Remote, "err", err)
		return eFail
	}
	stream, err := streamOnFile(local)
	if err != nil {
		slog.Error("drag stream", "file", local, "err", err)
		return eFail
	}
	*m = stgMedium{tymed: tymedIStream, data: stream}
	return sOK
}

func doEnumFormatEtc(this *dataObject, direction uint32, ppenum *uintptr) uintptr {
	if ppenum == nil {
		return ePointer
	}
	if direction != datadirGet {
		*ppenum = 0
		return eNotImpl
	}
	formats := [2]formatEtc{
		{cfFormat: this.cfDescriptor, aspect: dvaspectContent, lindex: -1, tymed: tymedHGlobal},
		{cfFormat: this.cfContents, aspect: dvaspectContent, lindex: -1, tymed: tymedIStream},
	}
	hr, _, _ := procSHCreateStdEnumFmtEtc.Call(uintptr(len(formats)), uintptr(unsafe.Pointer(&formats[0])), uintptr(unsafe.Pointer(ppenum)))
	return hr
}

// --- IDropSource ---

type dropSourceVtbl struct {
	QueryInterface, AddRef, Release uintptr
	QueryContinueDrag, GiveFeedback uintptr
}

type dropSource struct {
	vtbl *dropSourceVtbl
	refs int32
}

var dropSourceVt dropSourceVtbl
var dropSourceVtOnce sync.Once

func newDropSource() *dropSource {
	dropSourceVtOnce.Do(func() {
		dropSourceVt = dropSourceVtbl{
			QueryInterface: syscall.NewCallback(func(this *dropSource, riid *guid, ppv *uintptr) uintptr {
				if ppv == nil {
					return ePointer
				}
				if riid != nil && (*riid == iidIUnknown || *riid == iidIDropSource) {
					*ppv = uintptr(unsafe.Pointer(this))
					atomic.AddInt32(&this.refs, 1)
					return sOK
				}
				*ppv = 0
				return eNoInterface
			}),
			AddRef: syscall.NewCallback(func(this *dropSource) uintptr { return uintptr(atomic.AddInt32(&this.refs, 1)) }),
			Release: syscall.NewCallback(func(this *dropSource) uintptr {
				n := atomic.AddInt32(&this.refs, -1)
				if n == 0 {
					live.Delete(this)
				}
				return uintptr(n)
			}),
			QueryContinueDrag: syscall.NewCallback(func(this *dropSource, escape int32, keys uint32) uintptr {
				switch {
				case escape != 0:
					return dragdropSCancel
				case keys&mkLButton == 0:
					return dragdropSDrop
				}
				return sOK
			}),
			GiveFeedback: syscall.NewCallback(func(this *dropSource, effect uint32) uintptr {
				return dragdropSUseDefaultCursors
			}),
		}
	})
	src := &dropSource{vtbl: &dropSourceVt, refs: 1}
	live.Store(src, struct{}{})
	return src
}

// --- helpers ---

func globalFromBytes(data []byte) (win.HGLOBAL, error) {
	h, err := win.GlobalAlloc(co.GMEM_MOVEABLE, len(data))
	if err != nil {
		return 0, err
	}
	mem, err := h.GlobalLockSlice()
	if err != nil {
		h.GlobalFree()
		return 0, err
	}
	copy(mem, data)
	h.GlobalUnlock()
	return h, nil
}

func streamOnFile(path string) (uintptr, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var stream uintptr
	hr, _, _ := procSHCreateStreamOnFileEx.Call(uintptr(unsafe.Pointer(p)), stgmRead|stgmShareDenyWr, 0, 0, 0, uintptr(unsafe.Pointer(&stream)))
	if hr != sOK {
		return 0, fmt.Errorf("SHCreateStreamOnFileEx: HRESULT %#x", hr)
	}
	return stream, nil
}

var (
	cfOnce                   sync.Once
	cfDescriptor, cfContents uint16
	cfErr                    error
)

func clipboardFormats() (uint16, uint16, error) {
	cfOnce.Do(func() {
		d, err1 := win.RegisterClipboardFormat("FileGroupDescriptorW")
		c, err2 := win.RegisterClipboardFormat("FileContents")
		cfDescriptor, cfContents, cfErr = uint16(d), uint16(c), errors.Join(err1, err2)
	})
	return cfDescriptor, cfContents, cfErr
}

// dragTempRoot holds the files downloaded for drags of this process.
func dragTempRoot() string {
	return filepath.Join(os.TempDir(), "SSH-Cloud", fmt.Sprintf("drag-%d", os.Getpid()))
}

// cleanDragTemp removes this process's drag files, and drag folders of
// earlier runs that are older than a day.
func cleanDragTemp() {
	_ = os.RemoveAll(dragTempRoot())
	base := filepath.Join(os.TempDir(), "SSH-Cloud")
	entries, _ := os.ReadDir(base)
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "drag-") {
			continue
		}
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > 24*time.Hour {
			_ = os.RemoveAll(filepath.Join(base, e.Name()))
		}
	}
}

// doDragDrop runs the OLE drag loop (blocking, on the UI thread) and returns
// whether the items were dropped.
func doDragDrop(items []dragdata.Item, fetch func(int) (string, error)) (bool, error) {
	cfD, cfC, err := clipboardFormats()
	if err != nil {
		return false, err
	}
	desc, err := dragdata.FileGroupDescriptor(items)
	if err != nil {
		return false, err
	}
	obj := newDataObject(items, desc, cfD, cfC, fetch)
	src := newDropSource()
	defer doRelease(obj)
	defer func() {
		if atomic.AddInt32(&src.refs, -1) == 0 {
			live.Delete(src)
		}
	}()

	var effect uint32
	hr, _, _ := procDoDragDrop.Call(uintptr(unsafe.Pointer(obj)), uintptr(unsafe.Pointer(src)), dropEffectCopy, uintptr(unsafe.Pointer(&effect)))
	slog.Info("drag out finished", "hresult", fmt.Sprintf("%#x", hr), "effect", effect, "items", len(items))
	switch hr {
	case dragdropSDrop:
		return true, nil
	case dragdropSCancel:
		return false, nil
	}
	return false, fmt.Errorf("DoDragDrop: HRESULT %#x", hr)
}
