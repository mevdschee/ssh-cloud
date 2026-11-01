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
	tbDetails
	tbUpdate
)

// maxTabs limits the number of open connections.
const maxTabs = 32

var (
	procBeginPaint       = user32.NewProc("BeginPaint")
	procEndPaint         = user32.NewProc("EndPaint")
	procFillRect         = user32.NewProc("FillRect")
	procInvalidateRect   = user32.NewProc("InvalidateRect")
	procCreateSolidBrush = gdi32.NewProc("CreateSolidBrush")
)

// Windows 11 Explorer colours (COLORREF, 0x00BBGGRR).
const (
	windowColor  = 0x00FFFFFF // content, navigation row and command bar
	bandColor    = 0x00F3F3F3 // the tab band at the top
	dividerColor = 0x00E5E5E5 // thin lines between the rows
)

var windowBrush, _, _ = procCreateSolidBrush.Call(windowColor)

// styleWindow applies the Windows 11 look; called in WM_CREATE, when all
// controls exist.
func (me *Explorer) styleWindow() {
	setWin11Frame(me.wnd.Hwnd())
	me.fontNormal, me.fontBold, me.fontTitle = uiFonts()
	applyFont(me.wnd.Hwnd(), me.fontNormal)
	setExplorerTheme(me.list.Hwnd())
	setExplorerTheme(me.tree.Hwnd())
	me.tree.SetExtendedStyle(true, co.TVS_EX_FADEINOUTEXPANDOS|co.TVS_EX_DOUBLEBUFFER)
	me.tree.Hwnd().SendMessage(co.TVM_SETITEMHEIGHT, win.WPARAM(ui.DpiY(28)), 0)
	// Inner margins in the address bar.
	m := ui.DpiX(8)
	me.address.Hwnd().SendMessage(co.EM_SETMARGINS, 0x1|0x2, win.LPARAM(uint32(m)|uint32(m)<<16))

	size := iconSize(me.wnd.Hwnd())
	me.tabs.setup(me.fontNormal, me.wnd.Hwnd().GetDpiForWindow())
	me.rebuildTabs()
	fillToolbar(me.navBar, []tbButton{
		{id: tbBack, icon: "arrow_left", text: "Back (Alt+Left)"},
		{id: tbForward, icon: "arrow_right", text: "Forward (Alt+Right)"},
		{id: tbUp, icon: "arrow_up", text: "Up (Alt+Up)"},
		{id: tbRefresh, icon: "arrow_clockwise", text: "Refresh (F5)"},
	}, size, ui.DpiX(26), ui.DpiY(14))
	fillToolbar(me.cmdBar, []tbButton{
		{id: tbNewFolder, icon: "folder_add", text: "New folder", showText: true},
		{},
		{id: tbCut, icon: "cut", text: "Cut (Ctrl+X)"},
		{id: tbCopy, icon: "copy", text: "Copy (Ctrl+C)"},
		{id: tbPaste, icon: "clipboard_paste", text: "Paste (Ctrl+V)"},
		{id: tbRename, icon: "rename", text: "Rename (F2)"},
		{id: tbDelete, icon: "delete", text: "Delete (Del)"},
		{},
		{id: tbUpload, icon: "arrow_upload", text: "Upload", showText: true},
		{id: tbDownload, icon: "arrow_download", text: "Download", showText: true},
		{id: tbStop, icon: "stop", text: "Stop", showText: true},
		{id: tbTransfers, icon: "history", text: "Transfers", showText: true},
		{},
		{id: tbDetails, icon: "panel_right", text: "Details", showText: true},
		{id: tbUpdate, icon: "arrow_sync", text: "Check for updates"},
	}, size, ui.DpiX(20), ui.DpiY(14))
	// The details toggle shows its state like Explorer's: pressed when on.
	setButtonStyle(me.cmdBar, tbDetails, co.BTNS_AUTOSIZE|co.BTNS_SHOWTEXT|co.BTNS_CHECK)
	checkButton(me.cmdBar, tbDetails, me.cfg.ShowDetails)
	me.styled = true
	slog.Debug("window styled", "iconSize", size, "dpi", me.wnd.Hwnd().GetDpiForWindow())
}

// setButtonStyle changes a toolbar button's style (TB_SETBUTTONINFO).
type tbButtonInfo struct {
	size, mask uint32
	idCommand  int32
	image      int32
	state      uint8
	style      uint8
	cx         uint16
	lParam     uintptr
	text       uintptr
	cchText    int32
	_          int32
}

func setButtonStyle(tb *ui.Toolbar, id uint16, style co.BTNS) {
	bi := tbButtonInfo{mask: 0x8, style: uint8(style)} // TBIF_STYLE
	bi.size = uint32(unsafe.Sizeof(bi))
	tb.Hwnd().SendMessage(co.WM_USER+64, win.WPARAM(id), win.LPARAM(unsafe.Pointer(&bi))) // TB_SETBUTTONINFOW
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
	on(tbDetails, me.toggleDetails)
	on(tbUpdate, me.checkForUpdates)
	me.tabEvents()
	me.wnd.On().WmPaint(me.paint)
	// Labels on the window (the status line) get the window background.
	me.wnd.On().WmCtlColorStatic(func(p ui.WmCtlColor) win.HBRUSH {
		p.Hdc().SetBkColor(win.COLORREF(windowColor))
		return win.HBRUSH(windowBrush)
	})
}

// toggleDetails shows or hides the details pane (Alt+Shift+P).
func (me *Explorer) toggleDetails() {
	me.cfg.ShowDetails = !me.cfg.ShowDetails
	checkButton(me.cmdBar, tbDetails, me.cfg.ShowDetails)
	if err := me.cfg.Save(me.cfgDir); err != nil {
		slog.Error("save settings", "err", err)
	}
	me.layout()
}

func move(h win.HWND, x, y, cx, cy int) {
	if cx < 0 {
		cx = 0
	}
	if cy < 0 {
		cy = 0
	}
	// SWP_NOCOPYBITS and the invalidate: a moved or resized control must be
	// redrawn, not shown with its old pixels (a right-aligned label would
	// otherwise keep its text at the old position, cut off).
	h.SetWindowPos(0, win.POINT{X: int32(x), Y: int32(y)}, win.SIZE{Cx: int32(cx), Cy: int32(cy)},
		co.SWP_NOZORDER|co.SWP_NOACTIVATE|co.SWP_NOCOPYBITS)
	procInvalidateRect.Call(uintptr(h), 0, 1)
}

// layout positions everything, top to bottom: the grey tab band, the
// navigation row (back, forward, up, refresh, address bar), a divider, the
// command bar (Details and Connections on the right), a divider, then the
// folder tree, the file list and the details pane, and the status line.
func (me *Explorer) layout() {
	if !me.styled {
		return
	}
	rc, err := me.wnd.Hwnd().GetClientRect()
	if err != nil || rc.Right == 0 {
		return
	}
	cw, ch := int(rc.Right), int(rc.Bottom)
	m := ui.DpiX(8)

	me.bandHeight = ui.DpiY(42)
	move(me.tabs.ctl.Hwnd(), 0, 0, cw, me.bandHeight)

	nav := toolbarSize(me.navBar)
	row1Y, row1H := me.bandHeight+ui.DpiY(6), max(int(nav.Cy), ui.DpiY(40))
	move(me.navBar.Hwnd(), m, row1Y+(row1H-int(nav.Cy))/2, int(nav.Cx), int(nav.Cy))
	addrX, addrH := m+int(nav.Cx)+ui.DpiX(12), ui.DpiY(30)
	move(me.address.Hwnd(), addrX, row1Y+(row1H-addrH)/2, cw-addrX-ui.DpiX(12), addrH)
	me.navLineY = row1Y + row1H + ui.DpiY(6)

	cmd := toolbarSize(me.cmdBar)
	row2Y, row2H := me.navLineY+1+ui.DpiY(4), int(cmd.Cy)
	move(me.cmdBar.Hwnd(), m, row2Y, min(int(cmd.Cx), cw-2*m), int(cmd.Cy))
	me.contentTop = row2Y + row2H + ui.DpiY(4)

	statusH := ui.DpiY(28)
	textH := ui.DpiY(20)
	statusY := ch - statusH + (statusH-textH)/2
	move(me.statusText.Hwnd(), ui.DpiX(12), statusY, cw/2, textH)
	move(me.countText.Hwnd(), cw/2, statusY, cw/2-ui.DpiX(16), textH)

	top, h := me.contentTop+1, ch-me.contentTop-1-statusH
	me.treeWidth = ui.DpiX(220)
	listW := cw - me.treeWidth
	me.details = win.RECT{}
	if me.cfg.ShowDetails && cw > ui.DpiX(700) {
		dw := ui.DpiX(280)
		listW -= dw + 1
		me.details = win.RECT{Left: int32(cw - dw), Top: int32(top), Right: int32(cw), Bottom: int32(top + h)}
	}
	move(me.tree.Hwnd(), 0, top, me.treeWidth, h)
	move(me.list.Hwnd(), me.treeWidth, top, listW, h)
	me.fitColumns(listW)
	procInvalidateRect.Call(uintptr(me.wnd.Hwnd()), 0, 1)
}

// fitColumns sizes the columns like Explorer: Date modified, Type, Size and
// Status keep their width and Name takes the rest, so there is no
// horizontal scrollbar until the list gets really narrow.
func (me *Explorer) fitColumns(listW int) {
	fixed := []int{ui.DpiX(130), ui.DpiX(130), ui.DpiX(80), ui.DpiX(110)}
	rest := listW - ui.DpiX(22) // room for the vertical scrollbar
	for i, w := range fixed {
		me.list.Col(i + 1).SetWidth(w)
		rest -= w
	}
	me.list.Col(0).SetWidth(max(rest, ui.DpiX(160)))
}

type paintStruct struct {
	hdc         uintptr
	erase       int32
	rc          win.RECT
	restore     int32
	incUpdate   int32
	rgbReserved [32]byte
}

// paint draws what is not a control: the tab band, the dividers and the
// details pane (the window background is white).
func (me *Explorer) paint() {
	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(uintptr(me.wnd.Hwnd()), uintptr(unsafe.Pointer(&ps)))
	defer procEndPaint.Call(uintptr(me.wnd.Hwnd()), uintptr(unsafe.Pointer(&ps)))
	if hdc == 0 || !me.styled {
		return
	}
	rc, _ := me.wnd.Hwnd().GetClientRect()
	fillRect(hdc, win.RECT{Right: rc.Right, Bottom: int32(me.bandHeight)}, bandColor)
	for _, y := range []int32{int32(me.navLineY), int32(me.contentTop)} {
		fillRect(hdc, win.RECT{Top: y, Right: rc.Right, Bottom: y + 1}, dividerColor)
	}
	if me.details.Right > 0 {
		d := me.details
		fillRect(hdc, win.RECT{Left: d.Left - 1, Top: d.Top, Right: d.Left, Bottom: d.Bottom}, dividerColor)
		me.paintDetails(hdc, d)
	}
}

func (me *Explorer) invalidateDetails() {
	if me.details.Right > 0 {
		d := me.details
		procInvalidateRect.Call(uintptr(me.wnd.Hwnd()), uintptr(unsafe.Pointer(&d)), 1)
	}
}
