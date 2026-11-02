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
	tbMore // "..." with the overflow menu
)

// Overflow menu ids.
const (
	cmdShowTransfers uint16 = 2201 + iota
	cmdShowHidden
	cmdCheckUpdates
	cmdAbout
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

// styleWindow applies the Windows 11 look; called in WM_CREATE, when all
// controls exist.
func (me *Explorer) styleWindow() {
	loadTheme()
	setWin11Frame(me.wnd.Hwnd())
	themeFrame(me.wnd.Hwnd())
	me.fontNormal = uiFont(0)
	me.fontLarge = uiFont(2)
	applyFont(me.wnd.Hwnd(), me.fontNormal)
	me.address.Hwnd().SendMessage(co.WM_SETFONT, win.WPARAM(me.fontLarge), 1)
	themeListView(me.list.Hwnd())
	themeTreeView(me.tree.Hwnd())
	me.tree.SetExtendedStyle(true, co.TVS_EX_FADEINOUTEXPANDOS|co.TVS_EX_DOUBLEBUFFER)
	me.tree.Hwnd().SendMessage(co.TVM_SETITEMHEIGHT, win.WPARAM(ui.DpiY(28)), 0)

	size := iconSize(me.wnd.Hwnd())
	me.tabs.setup(me.fontNormal, me.wnd.Hwnd().GetDpiForWindow())
	me.rebuildTabs()
	fillToolbar(me.navBar, []tbButton{
		{id: tbBack, icon: "arrow_left", text: "Back (Alt+Left)", width: ui.DpiX(40)},
		{id: tbForward, icon: "arrow_right", text: "Forward (Alt+Right)", width: ui.DpiX(40)},
		{id: tbUp, icon: "arrow_up", text: "Up (Alt+Up)", width: ui.DpiX(40)},
		{id: tbRefresh, icon: "arrow_clockwise", text: "Refresh (F5)", width: ui.DpiX(40)},
	}, size, ui.DpiX(20), ui.DpiY(14), ui.DpiX(8))
	fillToolbar(me.cmdBar, []tbButton{
		{id: tbNewFolder, icon: "folder_add", text: "New folder", showText: true},
		{},
		{id: tbCut, icon: "cut", text: "Cut (Ctrl+X)", width: ui.DpiX(40)},
		{id: tbCopy, icon: "copy", text: "Copy (Ctrl+C)", width: ui.DpiX(40)},
		{id: tbPaste, icon: "clipboard_paste", text: "Paste (Ctrl+V)", width: ui.DpiX(40)},
		{id: tbRename, icon: "rename", text: "Rename (F2)", width: ui.DpiX(40)},
		{id: tbDelete, icon: "delete", text: "Delete (Del)", width: ui.DpiX(40)},
		{},
		{id: tbUpload, icon: "arrow_upload", text: "Upload", showText: true},
		{id: tbDownload, icon: "arrow_download", text: "Download", showText: true},
		{},
		{id: tbMore, icon: "more_horizontal", text: "See more", width: ui.DpiX(40)},
	}, size, ui.DpiX(20), ui.DpiY(14), ui.DpiX(8))
	me.styled = true
	slog.Debug("window styled", "iconSize", size, "dpi", me.wnd.Hwnd().GetDpiForWindow(), "dark", darkMode)
}

// themeChanged follows a switch between light and dark mode in Windows.
func (me *Explorer) themeChanged() {
	if !me.styled || !loadTheme() {
		return
	}
	slog.Info("theme changed", "dark", darkMode)
	themeFrame(me.wnd.Hwnd())
	themeListView(me.list.Hwnd())
	themeTreeView(me.tree.Hwnd())
	setToolbarIcons(me.navBar)
	setToolbarIcons(me.cmdBar)
	me.tabs.setup(me.fontNormal, me.wnd.Hwnd().GetDpiForWindow())
	redrawAll(me.wnd.Hwnd())
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
	on(tbMore, me.moreMenu)
	me.tabEvents()
	me.wnd.On().WmPaint(me.paint)
	me.splitterEvents()
	invalidateBox := func() {
		b := me.addrBox
		procInvalidateRect.Call(uintptr(me.wnd.Hwnd()), uintptr(unsafe.Pointer(&b)), 1)
	}
	me.address.On().EnSetFocus(func() { me.addrFocus = true; invalidateBox() })
	me.address.On().EnKillFocus(func() { me.addrFocus = false; invalidateBox() })
	// Labels on the window (the status line) get the window background, the
	// address bar the colours of its box.
	me.wnd.On().WmCtlColorStatic(func(p ui.WmCtlColor) win.HBRUSH {
		procSetTextColor.Call(uintptr(p.Hdc()), uintptr(colors.text))
		p.Hdc().SetBkColor(win.COLORREF(colors.window))
		return solidBrush(colors.window)
	})
	me.wnd.On().WmCtlColorEdit(func(p ui.WmCtlColor) win.HBRUSH {
		procSetTextColor.Call(uintptr(p.Hdc()), uintptr(colors.text))
		p.Hdc().SetBkColor(win.COLORREF(colors.addressFill))
		return solidBrush(colors.addressFill)
	})
	me.wnd.On().WmEraseBkgnd(func(p ui.WmEraseBkgnd) int {
		if rc, err := me.wnd.Hwnd().GetClientRect(); err == nil {
			fillRect(uintptr(p.Hdc()), rc, colors.window)
		}
		return 1
	})
	me.wnd.On().Wm(co.WM_WININICHANGE, func(p ui.Wm) uintptr { // WM_SETTINGCHANGE
		if isThemeChange(p) {
			me.themeChanged()
		}
		return 0
	})
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
// command bar, a divider, then the folder tree and the file list, and the
// status line.
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
	// The address box; the edit inside is one text line high and centred.
	addrX, addrH := m+int(nav.Cx)+ui.DpiX(12), ui.DpiY(32)
	boxTop := row1Y + (row1H-addrH)/2
	me.addrBox = win.RECT{Left: int32(addrX), Top: int32(boxTop), Right: int32(cw - ui.DpiX(12)), Bottom: int32(boxTop + addrH)}
	editH := fontHeight(me.fontLarge) + 2
	move(me.address.Hwnd(), addrX+ui.DpiX(10), boxTop+(addrH-editH)/2, cw-addrX-ui.DpiX(12)-ui.DpiX(20), editH)
	me.navLineY = row1Y + row1H + ui.DpiY(6)

	cmd := toolbarSize(me.cmdBar)
	row2Y, row2H := me.navLineY+1+ui.DpiY(4), int(cmd.Cy)
	move(me.cmdBar.Hwnd(), m, row2Y, min(int(cmd.Cx), cw-2*m), int(cmd.Cy))
	me.contentTop = row2Y + row2H + ui.DpiY(4)

	// The status labels fill the footer; SS_CENTERIMAGE centres the text.
	statusH := ui.DpiY(30)
	move(me.statusText.Hwnd(), ui.DpiX(12), ch-statusH, cw/2, statusH)
	move(me.countText.Hwnd(), cw/2, ch-statusH, cw/2-ui.DpiX(16), statusH)

	// 1px below the divider under the command bar, 1px above the one on top
	// of the status line.
	top, h := me.contentTop+1, ch-me.contentTop-1-statusH-1
	me.treeWidth = me.treeWidthFor(cw)
	// The gap between tree and list is the draggable divider; paint() draws
	// a 1px line in its middle.
	listX := me.treeWidth + splitterWidth()
	me.contentBottom = top + h
	move(me.tree.Hwnd(), 0, top, me.treeWidth, h)
	move(me.list.Hwnd(), listX, top, cw-listX, h)
	me.fitColumns(cw - listX)
	procInvalidateRect.Call(uintptr(me.wnd.Hwnd()), 0, 1)
}

// minWindowWidth is the narrowest the window may get: wide enough for the
// toolbars (the command bar, and the navigation bar with a usable address
// bar) and their margins, plus the window frame.
func (me *Explorer) minWindowWidth() int {
	if !me.styled {
		return 0
	}
	m := ui.DpiX(8)
	cmd, nav := toolbarSize(me.cmdBar), toolbarSize(me.navBar)
	client := max(m+int(cmd.Cx)+m, m+int(nav.Cx)+ui.DpiX(12)+ui.DpiX(120)+ui.DpiX(12))
	hwnd := me.wnd.Hwnd()
	wr, err1 := hwnd.GetWindowRect()
	cr, err2 := hwnd.GetClientRect()
	if err1 != nil || err2 != nil {
		return client
	}
	return client + int(wr.Right-wr.Left) - int(cr.Right-cr.Left)
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

// paint draws what is not a control: the tab band, the navigation row, the
// dividers and the address box (the rest is the window background).
func (me *Explorer) paint() {
	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(uintptr(me.wnd.Hwnd()), uintptr(unsafe.Pointer(&ps)))
	defer procEndPaint.Call(uintptr(me.wnd.Hwnd()), uintptr(unsafe.Pointer(&ps)))
	if hdc == 0 || !me.styled {
		return
	}
	rc, _ := me.wnd.Hwnd().GetClientRect()
	fillRect(hdc, win.RECT{Right: rc.Right, Bottom: int32(me.bandHeight)}, colors.band)
	fillRect(hdc, win.RECT{Top: int32(me.bandHeight), Right: rc.Right, Bottom: int32(me.navLineY)}, colors.nav)
	for _, y := range []int32{int32(me.navLineY), int32(me.contentTop), int32(me.contentBottom)} {
		fillRect(hdc, win.RECT{Top: y, Right: rc.Right, Bottom: y + 1}, colors.divider)
	}
	x := int32(me.treeWidth + splitterWidth()/2)
	fillRect(hdc, win.RECT{Left: x, Top: int32(me.contentTop), Right: x + 1, Bottom: int32(me.contentBottom)}, colors.divider)
	// Address box: 1px border with rounded corners, filled inside, and an
	// accent underline while the address bar has focus.
	if b := me.addrBox; b.Right > b.Left {
		radius := int32(ui.DpiX(4))
		roundRect(hdc, win.RECT{Left: b.Left, Top: b.Top, Right: b.Right - 1, Bottom: b.Bottom - 1}, radius, colors.addressBorder)
		roundRect(hdc, win.RECT{Left: b.Left + 1, Top: b.Top + 1, Right: b.Right - 2, Bottom: b.Bottom - 2}, radius-1, colors.addressFill)
		if me.addrFocus {
			fillRect(hdc, win.RECT{Left: b.Left + radius, Top: b.Bottom - 2, Right: b.Right - radius, Bottom: b.Bottom}, colors.accent)
		}
	}
}

// moreMenu shows the overflow ("...") menu below its button.
func (me *Explorer) moreMenu() {
	rc := buttonScreenRect(me.cmdBar, tbMore)
	items := []menuItem{
		{id: cmdShowTransfers, text: "&Transfers", enabled: me.isConnected() && !me.busy},
		{id: cmdShowHidden, text: "Show &hidden files", enabled: !me.busy, checked: me.cfg.ShowHidden},
		{},
		{id: cmdCheckUpdates, text: "Check for &updates", enabled: !me.busy},
		{},
		{id: cmdAbout, text: "&About ...", enabled: !me.busy},
	}
	switch popupMenuAt(me.wnd.Hwnd(), items, win.POINT{X: rc.Left, Y: rc.Bottom}) {
	case cmdShowTransfers:
		me.showTransfers()
	case cmdShowHidden:
		me.toggleHidden()
	case cmdCheckUpdates:
		me.checkForUpdates()
	case cmdAbout:
		showAbout(me.wnd, me.version)
	}
}
