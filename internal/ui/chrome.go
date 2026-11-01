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
	tbDetails
	tbNewTab
	tbUpdate
)

// tbTab is the id of the first tab button; tab i has id tbTab+i.
const (
	tbTab   uint16 = 4001
	maxTabs        = 32
)

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
	setupToolbar(me.tabBar, []string{"cloud", "add"}, size, ui.DpiX(20), ui.DpiY(12))
	me.rebuildTabs()
	fillToolbar(me.navBar, []tbButton{
		{id: tbBack, icon: "arrow_left", text: "Back (Alt+Left)"},
		{id: tbForward, icon: "arrow_right", text: "Forward (Alt+Right)"},
		{id: tbUp, icon: "arrow_up", text: "Up (Alt+Up)"},
		{id: tbRefresh, icon: "arrow_clockwise", text: "Refresh (F5)"},
	}, size, ui.DpiX(22), ui.DpiY(14))
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
	}, size, ui.DpiX(16), ui.DpiY(14))
	fillToolbar(me.connBar, []tbButton{
		{id: tbUpdate, icon: "arrow_sync", text: "Check for updates"},
		{id: tbDetails, icon: "panel_right", text: "Details", showText: true},
		{id: tbConnections, icon: "plug_connected", text: "Connections", showText: true},
	}, size, ui.DpiX(16), ui.DpiY(14))
	// The details toggle shows its state like Explorer's: pressed when on.
	setButtonStyle(me.connBar, tbDetails, co.BTNS_AUTOSIZE|co.BTNS_SHOWTEXT|co.BTNS_CHECK)
	checkButton(me.connBar, tbDetails, me.cfg.ShowDetails)
	me.styled = true
	slog.Debug("window styled", "iconSize", size, "dpi", me.wnd.Hwnd().GetDpiForWindow())
}

// setButtonStyle changes a toolbar button's style (TB_SETBUTTONINFO).
func setButtonStyle(tb *ui.Toolbar, id uint16, style co.BTNS) {
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
	on(tbConnections, func() { me.promptConnect(false) })
	on(tbDetails, me.toggleDetails)
	on(tbUpdate, me.checkForUpdates)
	on(tbNewTab, func() { me.promptConnect(true) })
	for i := 0; i < maxTabs; i++ {
		on(tbTab+uint16(i), func() { me.switchTab(i) })
	}
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
	checkButton(me.connBar, tbDetails, me.cfg.ShowDetails)
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
	h.SetWindowPos(0, win.POINT{X: int32(x), Y: int32(y)}, win.SIZE{Cx: int32(cx), Cy: int32(cy)},
		co.SWP_NOZORDER|co.SWP_NOACTIVATE)
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

	tabs := toolbarSize(me.tabBar)
	me.bandHeight = int(tabs.Cy) + ui.DpiY(8)
	move(me.tabBar.Hwnd(), m, ui.DpiY(4), min(int(tabs.Cx), cw-2*m), int(tabs.Cy))

	nav := toolbarSize(me.navBar)
	row1Y, row1H := me.bandHeight+ui.DpiY(6), max(int(nav.Cy), ui.DpiY(40))
	move(me.navBar.Hwnd(), m, row1Y+(row1H-int(nav.Cy))/2, int(nav.Cx), int(nav.Cy))
	addrX, addrH := m+int(nav.Cx)+ui.DpiX(12), ui.DpiY(30)
	move(me.address.Hwnd(), addrX, row1Y+(row1H-addrH)/2, cw-addrX-ui.DpiX(12), addrH)
	me.navLineY = row1Y + row1H + ui.DpiY(6)

	cmd, conn := toolbarSize(me.cmdBar), toolbarSize(me.connBar)
	row2Y, row2H := me.navLineY+1+ui.DpiY(4), max(int(cmd.Cy), int(conn.Cy))
	move(me.cmdBar.Hwnd(), m, row2Y, min(int(cmd.Cx), cw-int(conn.Cx)-3*m), int(cmd.Cy))
	move(me.connBar.Hwnd(), cw-m-int(conn.Cx), row2Y, int(conn.Cx), int(conn.Cy))
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
	me.fitLastColumn()
	procInvalidateRect.Call(uintptr(me.wnd.Hwnd()), 0, 1)
}

// fitLastColumn stretches the Status column to the list's right edge, so
// there is no horizontal scrollbar while the other columns fit.
func (me *Explorer) fitLastColumn() {
	autosizeUseHeader := -2 // LVSCW_AUTOSIZE_USEHEADER, passed as a negative LPARAM
	me.list.Hwnd().SendMessage(co.LVM_SETCOLUMNWIDTH, win.WPARAM(me.list.ColCount()-1), win.LPARAM(autosizeUseHeader))
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
