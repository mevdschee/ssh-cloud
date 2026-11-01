//go:build windows

package ui

// The About window, laid out like most desktop apps' (Firefox, Notepad++,
// the CDE style guide), everything left aligned: the logo on the left; on the
// right the name, the version, a one-line description, the author's
// copyright, the licence, a link to the project (blue) and the icon credits. No
// buttons: Esc, Enter or the window's close button close it.

import (
	"fmt"
	"log/slog"
	"syscall"
	"time"
	"unsafe"

	"github.com/mevdschee/ssh-cloud/internal/icons"
	"github.com/mevdschee/ssh-cloud/internal/update"
	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
	"github.com/rodrigocfd/windigo/win"
)

const (
	repoURL = "https://github.com/mevdschee/ssh-cloud"
	author  = "Maurits van der Schee"
)

var (
	procShellExecuteW = shell32.NewProc("ShellExecuteW")
	procGetSysColor   = user32.NewProc("GetSysColor")
)

// openURL opens a web page in the default browser.
func openURL(owner win.HWND, url string) {
	verb, _ := syscall.UTF16PtrFromString("open")
	u, _ := syscall.UTF16PtrFromString(url)
	r, _, _ := procShellExecuteW.Call(uintptr(owner), uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(u)), 0, 0, 1) // SW_SHOWNORMAL
	if r <= 32 {
		slog.Error("open url", "url", url, "code", r)
	}
}

// appIconList renders the app icon at an exact size into a one-image list,
// so it is sharp at any DPI (scaling the icon resource blurs it).
func appIconList(size int) uintptr {
	hImg, err := win.ImageListCreate(size, size, co.ILC_COLOR32, 1, 0)
	if err != nil {
		return 0
	}
	if hbmp, err := bitmapFromImage(icons.AppIcon(size)); err == nil {
		_ = hImg.Add(hbmp, 0)
		procDeleteObject.Call(uintptr(hbmp))
	}
	return uintptr(hImg)
}

// showAbout shows the About window.
func showAbout(parent ui.Parent, version string) {
	wnd := newModal(parent, ui.OptsModal().Title("About SSH-Cloud").Size(ui.Dpi(460, 232)))
	x, w := 132, 310
	text := func(t string, y, h int, style co.SS) *ui.Static {
		return ui.NewStatic(wnd, ui.OptsStatic().Text(t).Position(ui.Dpi(x, y)).Size(ui.Dpi(w, h)).
			CtrlStyle(co.SS_LEFT|co.SS_NOPREFIX|style))
	}

	ver := "Version " + version
	if !update.IsRelease(version) {
		ver = "Development build (" + version + ")"
	}
	title := text("SSH-Cloud", 20, 34, 0)
	text(ver, 58, 20, 0)
	text("The ultimate simple cloud storage client.", 86, 20, 0)
	text(fmt.Sprintf("© %d %s", max(time.Now().Year(), 2026), author), 114, 20, 0)
	text("Released under the MIT License.", 136, 20, 0)
	text("Icons: Fluent UI System Icons by Microsoft (MIT).", 180, 20, 0)

	wnd.On().WmCommandAccelMenu(uint16(co.ID_OK), func() { closeModal(wnd) })     // Enter
	wnd.On().WmCommandAccelMenu(uint16(co.ID_CANCEL), func() { closeModal(wnd) }) // Esc

	normal := uiFont(0)
	titleFont := uiFontWeight(ui.DpiY(10), 600)
	wnd.On().WmCreate(func(_ ui.WmCreate) int {
		applyFont(wnd.Hwnd(), normal)
		title.Hwnd().SendMessage(co.WM_SETFONT, win.WPARAM(titleFont), 1)
		return 0
	})
	// The link is painted by the window itself: accent text, no underline (an
	// underlined font got an underline wider than the text under Wine).
	// Clicks and the hand cursor use the text's exact rectangle.
	var linkRect win.RECT
	iconSize := ui.DpiX(80)
	iconList := appIconList(iconSize)
	wnd.On().WmPaint(func() {
		var ps paintStruct
		hdc, _, _ := procBeginPaint.Call(uintptr(wnd.Hwnd()), uintptr(unsafe.Pointer(&ps)))
		defer procEndPaint.Call(uintptr(wnd.Hwnd()), uintptr(unsafe.Pointer(&ps)))
		if hdc == 0 {
			return
		}
		if iconList != 0 {
			procImageListDraw.Call(iconList, 0, hdc, uintptr(ui.DpiX(26)), uintptr(ui.DpiY(24)), 1) // ILD_TRANSPARENT
		}
		old, _, _ := procSelectObject.Call(hdc, normal)
		left, top := int32(ui.DpiX(x)), int32(ui.DpiY(158))
		linkRect = win.RECT{Left: left, Top: top, Right: left + textWidth(hdc, repoURL), Bottom: top + int32(ui.DpiY(20))}
		procSetBkMode.Call(hdc, 1) // TRANSPARENT
		procSetTextColor.Call(hdc, uintptr(colors.accent))
		p, _ := syscall.UTF16FromString(repoURL)
		r := linkRect
		procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(&p[0])), uintptr(len(p)-1), uintptr(unsafe.Pointer(&r)),
			dtLeft|dtSingleLine|dtNoPrefix)
		procSelectObject.Call(hdc, old)
	})
	hand, _ := win.HINSTANCE(0).LoadCursor(win.CursorResIdc(co.IDC_HAND))
	arrow, _ := win.HINSTANCE(0).LoadCursor(win.CursorResIdc(co.IDC_ARROW))
	wnd.On().WmSetCursor(func(p ui.WmSetCursor) bool {
		if p.Hwnd() != wnd.Hwnd() {
			return false // over a label
		}
		if p.Raw.LParam.LoWord() != 1 { // not HTCLIENT: borders and title bar
			return wnd.Hwnd().DefWindowProc(co.WM_SETCURSOR, p.Raw.WParam, p.Raw.LParam) != 0
		}
		pt, err := win.GetCursorPos()
		if err == nil {
			wnd.Hwnd().ScreenToClientPt(&pt)
		}
		if err == nil && inRect(linkRect, pt) {
			hand.SetCursor()
		} else {
			arrow.SetCursor()
		}
		return true
	})
	wnd.On().WmLButtonUp(func(p ui.WmMouse) {
		if inRect(linkRect, p.Pos()) {
			openURL(wnd.Hwnd(), repoURL)
		}
	})
	wnd.ShowModal()

	if iconList != 0 {
		win.HIMAGELIST(iconList).Destroy()
	}
	for _, f := range []uintptr{normal, titleFont} {
		procDeleteObject.Call(f)
	}
}
