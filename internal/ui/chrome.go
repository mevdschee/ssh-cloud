//go:build windows

package ui

import (
	"log/slog"
	"unsafe"

	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
	"github.com/rodrigocfd/windigo/win"
)

// Toolbar button ids. They differ from the accelerator ids, so a click on a
// toolbar button always acts on files, even while the address bar has focus.
const (
	tbBack uint16 = 3001 + iota
	tbForward
	tbUp
	tbRefresh
	tbNewFolder
	tbUpload
	tbDownload
	tbCut
	tbCopy
	tbPaste
	tbRename
	tbDelete
	tbTransfers
	tbStop
	tbConnections
)

var (
	procBeginPaint       = user32.NewProc("BeginPaint")
	procEndPaint         = user32.NewProc("EndPaint")
	procFillRect         = user32.NewProc("FillRect")
	procInvalidateRect   = user32.NewProc("InvalidateRect")
	procCreateSolidBrush = gdi32.NewProc("CreateSolidBrush")
)

// dividerColor is the light grey of Windows 11 Explorer's dividers (BGR).
const dividerColor = 0x00E5E5E5

// styleWindow applies the Windows 11 look; called in WM_CREATE, when all
// controls exist.
func (me *Explorer) styleWindow() {
	setWin11Frame(me.wnd.Hwnd())
	applyFont(me.wnd.Hwnd(), uiFont())
	setExplorerTheme(me.list.Hwnd())
	setExplorerTheme(me.tree.Hwnd())
	me.tree.SetExtendedStyle(true, co.TVS_EX_FADEINOUTEXPANDOS|co.TVS_EX_DOUBLEBUFFER)

	size := iconSize(me.wnd.Hwnd())
	fillToolbar(me.navBar, []tbButton{
		{id: tbBack, icon: "arrow_left", text: "Back (Alt+Left)"},
		{id: tbForward, icon: "arrow_right", text: "Forward (Alt+Right)"},
		{id: tbUp, icon: "arrow_up", text: "Up (Alt+Up)"},
		{id: tbRefresh, icon: "arrow_clockwise", text: "Refresh (F5)"},
	}, size)
	fillToolbar(me.cmdBar, []tbButton{
		{id: tbNewFolder, icon: "folder_add", text: "New folder", showText: true},
		{},
		{id: tbUpload, icon: "arrow_upload", text: "Upload", showText: true},
		{id: tbDownload, icon: "arrow_download", text: "Download", showText: true},
		{},
		{id: tbCut, icon: "cut", text: "Cut (Ctrl+X)"},
		{id: tbCopy, icon: "copy", text: "Copy (Ctrl+C)"},
		{id: tbPaste, icon: "clipboard_paste", text: "Paste (Ctrl+V)"},
		{id: tbRename, icon: "rename", text: "Rename (F2)"},
		{id: tbDelete, icon: "delete", text: "Delete (Del)"},
		{},
		{id: tbTransfers, icon: "history", text: "Transfers", showText: true},
		{id: tbStop, icon: "stop", text: "Stop", showText: true},
	}, size)
	fillToolbar(me.connBar, []tbButton{
		{id: tbConnections, icon: "plug_connected", text: "Connections", showText: true},
	}, size)
	me.styled = true
	slog.Debug("window styled", "iconSize", size, "dpi", me.wnd.Hwnd().GetDpiForWindow())
}

func (me *Explorer) toolbarEvents() {
	on := me.wnd.On().WmCommandAccelMenu
	on(tbBack, me.goBack)
	on(tbForward, me.goForward)
	on(tbUp, me.goUp)
	on(tbRefresh, me.refresh)
	on(tbNewFolder, me.newFolder)
	on(tbUpload, me.upload)
	on(tbDownload, me.download)
	on(tbCut, func() { me.toClipboard(true) })
	on(tbCopy, func() { me.toClipboard(false) })
	on(tbPaste, me.paste)
	on(tbRename, me.startRename)
	on(tbDelete, me.delete)
	on(tbTransfers, me.showTransfers)
	on(tbStop, func() {
		if me.opCancel != nil {
			slog.Info("operation stopped by user")
			me.opCancel()
			me.setStatus("Stopping...")
		}
	})
	on(tbConnections, me.promptConnect)
	me.wnd.On().WmPaint(me.paintDividers)
}

func move(h win.HWND, x, y, cx, cy int) {
	if cx < 0 {
		cx = 0
	}
	if cy < 0 {
		cy = 0
	}
	h.SetWindowPos(0, win.POINT{X: int32(x), Y: int32(y)}, win.SIZE{Cx: int32(cx), Cy: int32(cy)},
		co.SWP_NOZORDER|co.SWP_NOACTIVATE)
}

// layout positions everything: navigation row (back/forward/up/refresh and
// the address bar), command bar with Connections on the right, then the
// folder tree and the file list separated by thin dividers.
func (me *Explorer) layout() {
	if !me.styled {
		return
	}
	rc, err := me.wnd.Hwnd().GetClientRect()
	if err != nil || rc.Right == 0 {
		return
	}
	cw, ch := int(rc.Right), int(rc.Bottom)
	m := ui.DpiX(6)

	nav, cmd, conn := toolbarSize(me.navBar), toolbarSize(me.cmdBar), toolbarSize(me.connBar)
	row1Y, row1H := ui.DpiY(4), max(int(nav.Cy), ui.DpiY(32))
	move(me.navBar.Hwnd(), m, row1Y+(row1H-int(nav.Cy))/2, int(nav.Cx), int(nav.Cy))
	addrX, addrH := m+int(nav.Cx)+m, ui.DpiY(26)
	move(me.address.Hwnd(), addrX, row1Y+(row1H-addrH)/2, cw-addrX-m, addrH)

	row2Y, row2H := row1Y+row1H+ui.DpiY(2), max(int(cmd.Cy), int(conn.Cy))
	move(me.cmdBar.Hwnd(), m, row2Y, min(int(cmd.Cx), cw-int(conn.Cx)-3*m), int(cmd.Cy))
	move(me.connBar.Hwnd(), cw-m-int(conn.Cx), row2Y, int(conn.Cx), int(conn.Cy))

	sbH := 0
	if r, err := me.status.Hwnd().GetWindowRect(); err == nil {
		sbH = int(r.Bottom - r.Top)
	}
	me.contentTop = row2Y + row2H + ui.DpiY(4)
	me.treeWidth = ui.DpiX(220)
	h := ch - me.contentTop - 1 - sbH
	move(me.tree.Hwnd(), 0, me.contentTop+1, me.treeWidth, h)
	move(me.list.Hwnd(), me.treeWidth+1, me.contentTop+1, cw-me.treeWidth-1, h)
	procInvalidateRect.Call(uintptr(me.wnd.Hwnd()), 0, 1)
}

// paintDividers draws the 1px lines above the content and between the tree
// and the list (the window background is white).
func (me *Explorer) paintDividers() {
	type paintStruct struct {
		hdc         uintptr
		erase       int32
		rc          win.RECT
		restore     int32
		incUpdate   int32
		rgbReserved [32]byte
	}
	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(uintptr(me.wnd.Hwnd()), uintptr(unsafe.Pointer(&ps)))
	defer procEndPaint.Call(uintptr(me.wnd.Hwnd()), uintptr(unsafe.Pointer(&ps)))
	if hdc == 0 || !me.styled {
		return
	}
	rc, _ := me.wnd.Hwnd().GetClientRect()
	brush, _, _ := procCreateSolidBrush.Call(dividerColor)
	defer procDeleteObject.Call(brush)
	top := int32(me.contentTop)
	lines := []win.RECT{
		{Left: 0, Top: top, Right: rc.Right, Bottom: top + 1},
		{Left: int32(me.treeWidth), Top: top, Right: int32(me.treeWidth) + 1, Bottom: rc.Bottom},
	}
	for _, r := range lines {
		procFillRect.Call(hdc, uintptr(unsafe.Pointer(&r)), brush)
	}
}
