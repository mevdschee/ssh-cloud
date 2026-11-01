//go:build windows

package ui

// The About window, laid out like most desktop apps' (Firefox, Notepad++,
// the CDE style guide), everything left aligned: the logo on the left; on the
// right the name, the version, a one-line description, the author's
// copyright, the licence, a link to the project and the icon credits, then
// "Check for updates" and "Close".

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

	linkColor = 0x00C06700 // Windows 11 accent blue #0067C0 (COLORREF)
)

var (
	procShellExecuteW = shell32.NewProc("ShellExecuteW")
	procGetSysColor   = user32.NewProc("GetSysColor")
	procGetObjectW    = gdi32.NewProc("GetObjectW")
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

// underlined returns an underlined copy of a font.
func underlined(hFont uintptr) uintptr {
	var lf logFont
	if r, _, _ := procGetObjectW.Call(hFont, unsafe.Sizeof(lf), uintptr(unsafe.Pointer(&lf))); r == 0 {
		return hFont
	}
	lf.underline = 1
	h, _, _ := procCreateFontIndirectW.Call(uintptr(unsafe.Pointer(&lf)))
	return h
}

// showAbout shows the About window. checkUpdates runs when the user chooses
// "Check for updates" (after the window closed).
func showAbout(parent ui.Parent, version string, checkUpdates func()) {
	wnd := ui.NewModal(parent, ui.OptsModal().Title("About SSH-Cloud").Size(ui.Dpi(460, 250)))
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
	// A plain label as link: blue and underlined, hand cursor, opens the page.
	link := text("github.com/mevdschee/ssh-cloud", 158, 20, co.SS_NOTIFY)
	text("Icons: Fluent UI System Icons by Microsoft (MIT).", 180, 20, 0)

	btnUpdates := ui.NewButton(wnd, ui.OptsButton().Text("Check for &updates").Position(ui.Dpi(x, 212)).
		Width(ui.DpiX(140)))
	btnClose := ui.NewButton(wnd, ui.OptsButton().Text("Close").Position(ui.Dpi(x+148, 212)).
		CtrlId(uint16(co.ID_OK)).CtrlStyle(co.BS_DEFPUSHBUTTON))
	wantUpdates := false
	btnUpdates.On().BnClicked(func() { wantUpdates = true; closeModal(wnd) })
	btnClose.On().BnClicked(func() { closeModal(wnd) })
	wnd.On().WmCommandAccelMenu(uint16(co.ID_CANCEL), func() { closeModal(wnd) }) // Esc

	link.On().StnClicked(func() { openURL(wnd.Hwnd(), repoURL) })
	hand, _ := win.HINSTANCE(0).LoadCursor(win.CursorResIdc(co.IDC_HAND))
	link.OnSubclass().WmSetCursor(func(ui.WmSetCursor) bool {
		hand.SetCursor()
		return true
	})

	normal := uiFont(0)
	titleFont := uiFontWeight(ui.DpiY(10), 600)
	linkFont := underlined(normal)
	wnd.On().WmCreate(func(_ ui.WmCreate) int {
		applyFont(wnd.Hwnd(), normal)
		title.Hwnd().SendMessage(co.WM_SETFONT, win.WPARAM(titleFont), 1)
		link.Hwnd().SendMessage(co.WM_SETFONT, win.WPARAM(linkFont), 1)
		return 0
	})
	// Labels keep the dialog's own background; only the link's text is blue.
	dlgBrush, _ := win.GetSysColorBrush(co.COLOR_BTNFACE)
	btnFace, _, _ := procGetSysColor.Call(uintptr(co.COLOR_BTNFACE))
	wnd.On().WmCtlColorStatic(func(p ui.WmCtlColor) win.HBRUSH {
		p.Hdc().SetBkColor(win.COLORREF(btnFace))
		if p.HwndControl() == link.Hwnd() {
			p.Hdc().SetTextColor(win.COLORREF(linkColor))
		}
		return dlgBrush
	})

	iconSize := ui.DpiX(80)
	iconList := appIconList(iconSize)
	wnd.On().WmPaint(func() {
		var ps paintStruct
		hdc, _, _ := procBeginPaint.Call(uintptr(wnd.Hwnd()), uintptr(unsafe.Pointer(&ps)))
		defer procEndPaint.Call(uintptr(wnd.Hwnd()), uintptr(unsafe.Pointer(&ps)))
		if hdc != 0 && iconList != 0 {
			procImageListDraw.Call(iconList, 0, hdc, uintptr(ui.DpiX(26)), uintptr(ui.DpiY(24)), 1) // ILD_TRANSPARENT
		}
	})
	wnd.ShowModal()

	if iconList != 0 {
		win.HIMAGELIST(iconList).Destroy()
	}
	for _, f := range []uintptr{normal, titleFont, linkFont} {
		procDeleteObject.Call(f)
	}
	if wantUpdates && checkUpdates != nil {
		checkUpdates()
	}
}
