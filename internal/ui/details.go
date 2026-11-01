//go:build windows

package ui

// The details pane, like Windows 11 Explorer's: on the right, showing the
// selected item (large icon, name, type and its properties), the total of a
// multiple selection, or the current folder when nothing is selected. It is
// painted directly on the window, so it follows Explorer's typography.

import (
	"fmt"
	"path"
	"syscall"
	"unsafe"

	"github.com/mevdschee/ssh-cloud/internal/remote"
	"github.com/mevdschee/ssh-cloud/internal/transferlog"
	"github.com/rodrigocfd/windigo/ui"
	"github.com/rodrigocfd/windigo/win"
)

var (
	procDrawTextW         = user32.NewProc("DrawTextW")
	procSetBkMode         = gdi32.NewProc("SetBkMode")
	procSetTextColor      = gdi32.NewProc("SetTextColor")
	procSHGetImageList    = shell32.NewProc("SHGetImageList")
	comctl32              = syscall.NewLazyDLL("comctl32.dll")
	procImageListDraw     = comctl32.NewProc("ImageList_Draw")
	iidIImageList         = guid{0x46EB5926, 0x582E, 0x4017, [8]byte{0x9F, 0xDF, 0xE8, 0x99, 0x8D, 0xAA, 0x09, 0x50}}
	extraLargeImages      uintptr
	extraLargeImagesTried bool
)

const (
	dtLeft        = 0x0000
	dtWordBreak   = 0x0010
	dtSingleLine  = 0x0020
	dtNoPrefix    = 0x0800
	dtCalcRect    = 0x0400
	dtEndEllipsis = 0x8000
	dtEditControl = 0x2000

	textColor  = 0x001A1A1A
	labelColor = 0x00616161
	shilExtra  = 2 // SHIL_EXTRALARGE: 48x48
)

// largeImageList returns the 48px system image list (same indexes as the
// small one the list uses).
func largeImageList() uintptr {
	if !extraLargeImagesTried {
		extraLargeImagesTried = true
		procSHGetImageList.Call(shilExtra, uintptr(unsafe.Pointer(&iidIImageList)), uintptr(unsafe.Pointer(&extraLargeImages)))
	}
	return extraLargeImages
}

// drawText draws text in rc and returns the height used.
func drawText(hdc uintptr, text string, rc win.RECT, flags uint32) int32 {
	if text == "" {
		return 0
	}
	p, _ := syscall.UTF16FromString(text)
	calc := rc
	procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(&p[0])), ^uintptr(0), uintptr(unsafe.Pointer(&calc)), uintptr(flags|dtCalcRect))
	procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(&p[0])), ^uintptr(0), uintptr(unsafe.Pointer(&rc)), uintptr(flags))
	return calc.Bottom - calc.Top
}

type detailRow struct{ label, value string }

func (me *Explorer) paintDetails(hdc uintptr, area win.RECT) {
	pad := int32(ui.DpiX(20))
	x, y, right := area.Left+pad, area.Top+pad, area.Right-pad
	procSetBkMode.Call(hdc, 1) // TRANSPARENT
	old, _, _ := procSelectObject.Call(hdc, me.fontNormal)
	defer procSelectObject.Call(hdc, old)

	title, kind, icon, rows := me.detailsContent()
	if title == "" {
		return
	}
	// Large icon, then the name (up to a few lines) and the type.
	if il := largeImageList(); il != 0 && icon >= 0 {
		size := int32(ui.DpiX(48))
		procImageListDraw.Call(il, uintptr(icon), hdc, uintptr(x), uintptr(y), 1) // ILD_TRANSPARENT
		y += size + int32(ui.DpiY(12))
	}
	procSelectObject.Call(hdc, me.fontTitle)
	procSetTextColor.Call(hdc, textColor)
	y += drawText(hdc, title, win.RECT{Left: x, Top: y, Right: right, Bottom: y + int32(ui.DpiY(90))},
		dtLeft|dtWordBreak|dtNoPrefix|dtEditControl|dtEndEllipsis)
	procSelectObject.Call(hdc, me.fontNormal)
	procSetTextColor.Call(hdc, labelColor)
	y += int32(ui.DpiY(2))
	y += drawText(hdc, kind, win.RECT{Left: x, Top: y, Right: right, Bottom: area.Bottom},
		dtLeft|dtSingleLine|dtNoPrefix|dtEndEllipsis)
	y += int32(ui.DpiY(20))

	// Properties: a "Details" heading, then label/value rows.
	procSelectObject.Call(hdc, me.fontBold)
	procSetTextColor.Call(hdc, textColor)
	y += drawText(hdc, "Details", win.RECT{Left: x, Top: y, Right: right, Bottom: area.Bottom}, dtLeft|dtSingleLine|dtNoPrefix)
	y += int32(ui.DpiY(10))
	procSelectObject.Call(hdc, me.fontNormal)
	labelW := int32(ui.DpiX(96))
	for _, r := range rows {
		if y >= area.Bottom-pad {
			break
		}
		procSetTextColor.Call(hdc, labelColor)
		drawText(hdc, r.label, win.RECT{Left: x, Top: y, Right: x + labelW, Bottom: area.Bottom}, dtLeft|dtSingleLine|dtNoPrefix)
		procSetTextColor.Call(hdc, textColor)
		h := drawText(hdc, r.value, win.RECT{Left: x + labelW, Top: y, Right: right, Bottom: area.Bottom - pad},
			dtLeft|dtWordBreak|dtNoPrefix|dtEditControl)
		y += max(h, int32(ui.DpiY(18))) + int32(ui.DpiY(8))
	}
}

// detailsContent describes the selection, or the current folder.
func (me *Explorer) detailsContent() (title, kind string, icon int32, rows []detailRow) {
	if !me.isConnected() {
		return "", "", -1, nil
	}
	sel := me.selected()
	switch len(sel) {
	case 0:
		name := remote.Base(me.dir)
		if me.dir == "" {
			name = me.profile.DisplayName()
		}
		files, folders := 0, 0
		var total int64
		for _, e := range me.entries {
			if e.IsDir {
				folders++
			} else {
				files++
				total += e.Size
			}
		}
		rows = []detailRow{
			{"Location", "/" + me.dir},
			{"Contents", fmt.Sprintf("%d folders, %d files", folders, files)},
			{"Total size", humanSize(total) + " (files in this folder)"},
			{"Connection", me.profile.DisplayName()},
			{"Server", fmt.Sprintf("%s@%s", me.profile.User, me.profile.Host)},
		}
		return name, "File folder", shellTypeOf("", true).icon, rows
	case 1:
		e := sel[0]
		st := shellTypeOf(e.Name, e.IsDir)
		if !e.IsDir {
			rows = append(rows, detailRow{"Size", fmt.Sprintf("%s (%d bytes)", humanSize(e.Size), e.Size)})
		}
		if !e.ModTime.IsZero() {
			rows = append(rows, detailRow{"Modified", e.ModTime.Local().Format("2006-01-02 15:04")})
		}
		if s := statusText(me.statuses[e.Path]); s != "" {
			rows = append(rows, detailRow{"Status", s})
		}
		if last, ok := me.lastTransfer(e.Path); ok {
			what := "Downloaded"
			if last.Direction == transferlog.Upload {
				what = "Uploaded"
			}
			rows = append(rows, detailRow{"Last transfer", fmt.Sprintf("%s %s", what, last.Time.Local().Format("2006-01-02 15:04"))})
		}
		rows = append(rows, detailRow{"Location", "/" + path.Dir("/" + e.Path)[1:]})
		if e.Mode != 0 {
			rows = append(rows, detailRow{"Permissions", e.Mode.Perm().String()})
		}
		return e.Name, st.name, st.icon, rows
	default:
		files, folders := 0, 0
		var total int64
		for _, e := range sel {
			if e.IsDir {
				folders++
			} else {
				files++
				total += e.Size
			}
		}
		rows = []detailRow{
			{"Files", fmt.Sprint(files)},
			{"Folders", fmt.Sprint(folders)},
			{"Total size", humanSize(total) + " (without folder contents)"},
			{"Location", "/" + me.dir},
		}
		return fmt.Sprintf("%d items selected", len(sel)), "", shellTypeOf(sel[0].Name, sel[0].IsDir).icon, rows
	}
}

func (me *Explorer) lastTransfer(p string) (transferlog.Entry, bool) {
	me.mu.Lock()
	a := me.app
	me.mu.Unlock()
	if a == nil {
		return transferlog.Entry{}, false
	}
	return a.Log().Latest(p)
}
