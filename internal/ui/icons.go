//go:build windows

package ui

import (
	"path"
	"strings"
	"syscall"
	"unsafe"

	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/win"
)

var procSHGetFileInfoW = shell32.NewProc("SHGetFileInfoW")

const (
	shgfiSmallIcon         = 0x0001
	shgfiUseFileAttributes = 0x0010
	shgfiTypeName          = 0x0400
	shgfiSysIconIndex      = 0x4000

	fileAttributeDirectory = 0x10
	fileAttributeNormal    = 0x80
)

// shFileInfo is SHFILEINFOW (x64 layout).
type shFileInfo struct {
	hIcon       uintptr
	iIcon       int32
	attributes  uint32
	displayName [260]uint16
	typeName    [80]uint16
}

type shellType struct {
	icon int32
	name string
}

// shellTypes caches icon index and type name per extension ("" = folder).
// Only used on the UI thread.
var shellTypes = map[string]shellType{}

// systemImageList returns the shared small system image list. It must never
// be destroyed, so controls using it need LVS_SHAREIMAGELISTS (tree views
// never destroy their image lists).
func systemImageList() win.HIMAGELIST {
	var sfi shFileInfo
	p, _ := syscall.UTF16PtrFromString("file")
	h, _, _ := procSHGetFileInfoW.Call(uintptr(unsafe.Pointer(p)), fileAttributeNormal, uintptr(unsafe.Pointer(&sfi)),
		unsafe.Sizeof(sfi), shgfiUseFileAttributes|shgfiSysIconIndex|shgfiSmallIcon)
	return win.HIMAGELIST(h)
}

// shellTypeOf returns the Explorer icon index and type name for a file or
// folder name, without needing a local file.
func shellTypeOf(name string, isDir bool) shellType {
	key := ""
	if !isDir {
		key = strings.ToLower(path.Ext(name))
		if key == "" {
			key = "."
		}
	}
	if t, ok := shellTypes[key]; ok {
		return t
	}
	probe, attrs := "folder", uint32(fileAttributeDirectory)
	if !isDir {
		probe, attrs = "file", fileAttributeNormal
		if key != "." {
			probe += key // "file.docx": only the extension matters
		}
	}
	var sfi shFileInfo
	p, _ := syscall.UTF16PtrFromString(probe)
	procSHGetFileInfoW.Call(uintptr(unsafe.Pointer(p)), uintptr(attrs), uintptr(unsafe.Pointer(&sfi)),
		unsafe.Sizeof(sfi), shgfiUseFileAttributes|shgfiSysIconIndex|shgfiSmallIcon|shgfiTypeName)
	t := shellType{icon: sfi.iIcon, name: syscall.UTF16ToString(sfi.typeName[:])}
	if t.name == "" {
		t.name = "File"
		if isDir {
			t.name = "File folder"
		}
	}
	shellTypes[key] = t
	return t
}

// setListImage sets the image index of a list view item.
func setListImage(list win.HWND, index int, icon int32) {
	lvi := win.LVITEM{Mask: co.LVIF_IMAGE, IItem: int32(index), IImage: icon}
	list.SendMessage(co.LVM_SETITEM, 0, win.LPARAM(unsafe.Pointer(&lvi)))
}

// setTreeImage sets the image of a tree view item.
func setTreeImage(tree win.HWND, item win.HTREEITEM, icon int32) {
	tvi := win.TVITEMEX{Mask: co.TVIF_IMAGE | co.TVIF_SELECTEDIMAGE, HItem: item, IImage: icon, ISelectedImage: icon}
	tree.SendMessage(co.TVM_SETITEM, 0, win.LPARAM(unsafe.Pointer(&tvi)))
}
