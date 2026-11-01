// Package dragdata builds the shell clipboard data for dragging cloud files
// out to Windows Explorer. It is plain Go (no Win32 calls), so the exact byte
// layout can be tested on any platform.
//
// Explorer receives a FILEGROUPDESCRIPTORW listing every file and folder with
// a relative path, and asks for each file's content (CFSTR_FILECONTENTS) only
// when the drop happens. Folders come before their contents.
package dragdata

import (
	"encoding/binary"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"
)

// Item is one file or folder in a drag.
type Item struct {
	Name    string // relative path shown to Explorer, with backslashes, e.g. `proj\sub\a.txt`
	Remote  string // cloud path
	IsDir   bool
	Size    int64
	ModTime time.Time
}

// Layout of FILEDESCRIPTORW (shlobj_core.h). All members are 4-byte aligned,
// so there is no padding: 4+16+8+8+4+8+8+8+4+4+520 = 592 bytes.
const (
	descriptorSize = 592
	maxPath        = 260

	offFlags      = 0
	offAttributes = 36
	offWriteTime  = 56
	offSizeHigh   = 64
	offSizeLow    = 68
	offFileName   = 72
)

// FILEDESCRIPTOR flags and file attributes.
const (
	fdAttributes = 0x00000004
	fdWritesTime = 0x00000020
	fdFileSize   = 0x00000040
	fdProgressUI = 0x00004000
	fdUnicode    = 0x80000000

	fileAttributeDirectory = 0x10
	fileAttributeNormal    = 0x80
)

// FileGroupDescriptor returns the FILEGROUPDESCRIPTORW bytes for items: a
// UINT count followed by one FILEDESCRIPTORW per item.
func FileGroupDescriptor(items []Item) ([]byte, error) {
	buf := make([]byte, 4+len(items)*descriptorSize)
	binary.LittleEndian.PutUint32(buf, uint32(len(items)))
	for i, it := range items {
		name := utf16.Encode([]rune(it.Name))
		if len(name) == 0 || len(name) >= maxPath {
			return nil, fmt.Errorf("path too long for Windows Explorer (max %d characters): %s", maxPath-1, it.Name)
		}
		d := buf[4+i*descriptorSize : 4+(i+1)*descriptorSize]
		flags := uint32(fdAttributes | fdUnicode | fdProgressUI)
		attrs := uint32(fileAttributeNormal)
		if it.IsDir {
			attrs = fileAttributeDirectory
		} else {
			flags |= fdFileSize
			binary.LittleEndian.PutUint32(d[offSizeHigh:], uint32(uint64(it.Size)>>32))
			binary.LittleEndian.PutUint32(d[offSizeLow:], uint32(it.Size))
		}
		if !it.ModTime.IsZero() {
			flags |= fdWritesTime
			binary.LittleEndian.PutUint64(d[offWriteTime:], FileTime(it.ModTime))
		}
		binary.LittleEndian.PutUint32(d[offFlags:], flags)
		binary.LittleEndian.PutUint32(d[offAttributes:], attrs)
		for j, c := range name {
			binary.LittleEndian.PutUint16(d[offFileName+2*j:], c)
		}
	}
	return buf, nil
}

// FileTime converts a time to a Windows FILETIME value (100 ns intervals
// since 1601-01-01 UTC).
func FileTime(t time.Time) uint64 {
	const epochDiff = 116444736000000000 // 1601 to 1970 in 100 ns
	return uint64(t.UnixNano()/100) + epochDiff
}

// WindowsName turns a relative cloud path into the backslash form Explorer
// expects.
func WindowsName(rel string) string { return strings.ReplaceAll(rel, "/", `\`) }
