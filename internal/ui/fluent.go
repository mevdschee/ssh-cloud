//go:build windows

package ui

// Windows 11 styling: Fluent icons on flat toolbars, the Explorer theme on
// the list and tree, the Segoe UI Variable font and the rounded, Mica-tinted
// window frame. Everything degrades gracefully on Windows 10 (the DWM
// attributes are ignored and the font falls back to the system font).

import (
	"image"
	"image/color"
	"log/slog"
	"syscall"
	"unsafe"

	"github.com/mevdschee/ssh-cloud/internal/icons"
	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
	"github.com/rodrigocfd/windigo/win"
)

var (
	gdi32                    = syscall.NewLazyDLL("gdi32.dll")
	procCreateDIBSection     = gdi32.NewProc("CreateDIBSection")
	procCreateFontIndirectW  = gdi32.NewProc("CreateFontIndirectW")
	procSelectObject         = gdi32.NewProc("SelectObject")
	procGetTextFaceW         = gdi32.NewProc("GetTextFaceW")
	procGetTextMetricsW      = gdi32.NewProc("GetTextMetricsW")
	procDeleteObject         = gdi32.NewProc("DeleteObject")
	user32                   = syscall.NewLazyDLL("user32.dll")
	procGetDC                = user32.NewProc("GetDC")
	procReleaseDC            = user32.NewProc("ReleaseDC")
	procSystemParametersInfo = user32.NewProc("SystemParametersInfoW")
	uxtheme                  = syscall.NewLazyDLL("uxtheme.dll")
	procSetWindowTheme       = uxtheme.NewProc("SetWindowTheme")
	dwmapi                   = syscall.NewLazyDLL("dwmapi.dll")
	procDwmSetWindowAttr     = dwmapi.NewProc("DwmSetWindowAttribute")
)

// Common control styles missing from windigo's co package.
const (
	ccsNoResize         = 0x0004
	ccsNoParentAlign    = 0x0008
	ccsNoDivider        = 0x0040
	dwmaCornerPref      = 33 // DWMWA_WINDOW_CORNER_PREFERENCE
	dwmaBackdropType    = 38 // DWMWA_SYSTEMBACKDROP_TYPE
	dwmcpRound          = 2  // DWMWCP_ROUND
	dwmsbtMainWindow    = 2  // DWMSBT_MAINWINDOW (Mica)
	spiNonClientMetrics = 0x0029
)

// iconSize is the toolbar icon size for the window's DPI: the 20px design
// size at 100%, scaled up (never down) for higher DPI.
func iconSize(hwnd win.HWND) int {
	dpi := hwnd.GetDpiForWindow()
	if dpi <= 0 {
		dpi = 96
	}
	size := 20 * dpi / 96
	if size < 20 {
		size = 20
	}
	return size
}

// bitmapFromImage makes a 32-bit top-down DIB section from an image.RGBA
// for an image list. Image lists treat such bitmaps like icons, with
// straight alpha, so the premultiplied colours of image.RGBA are divided by
// alpha first (otherwise the antialiased edges of light icons turn dark).
func bitmapFromImage(img *image.RGBA) (win.HBITMAP, error) {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	type bitmapInfoHeader struct {
		size                       uint32
		width, height              int32
		planes, bitCount           uint16
		compression, sizeImage     uint32
		xPelsPerMeter, yPelsPerMtr int32
		clrUsed, clrImportant      uint32
	}
	bih := bitmapInfoHeader{width: int32(w), height: -int32(h), planes: 1, bitCount: 32}
	bih.size = uint32(unsafe.Sizeof(bih))
	var bits unsafe.Pointer
	hbmp, _, err := procCreateDIBSection.Call(0, uintptr(unsafe.Pointer(&bih)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hbmp == 0 {
		return 0, err
	}
	dst := unsafe.Slice((*byte)(bits), w*h*4)
	for i := 0; i < w*h; i++ { // RGBA (premultiplied) to BGRA (straight)
		a := uint32(img.Pix[i*4+3])
		straight := func(c uint8) uint8 {
			if a == 0 {
				return 0
			}
			return uint8(min(255, (uint32(c)*255+a/2)/a))
		}
		dst[i*4+0] = straight(img.Pix[i*4+2])
		dst[i*4+1] = straight(img.Pix[i*4+1])
		dst[i*4+2] = straight(img.Pix[i*4+0])
		dst[i*4+3] = uint8(a)
	}
	return win.HBITMAP(hbmp), nil
}

// fluentImageList renders icons into a new image list, in order.
func fluentImageList(names []string, size int, c color.RGBA) win.HIMAGELIST {
	hImg, err := win.ImageListCreate(size, size, co.ILC_COLOR32, len(names), 0)
	if err != nil {
		slog.Error("image list", "err", err)
		return 0
	}
	for _, name := range names {
		img, err := icons.Render(name, size, c)
		if err != nil {
			slog.Error("render icon", "icon", name, "err", err)
			img = image.NewRGBA(image.Rect(0, 0, size, size)) // keep indexes aligned
		}
		hbmp, err := bitmapFromImage(img)
		if err != nil {
			slog.Error("icon bitmap", "icon", name, "err", err)
			continue
		}
		_ = hImg.Add(hbmp, 0)
		procDeleteObject.Call(uintptr(hbmp))
	}
	return hImg
}

// tbButton describes a toolbar button; an empty icon is a separator.
type tbButton struct {
	id       uint16
	icon     string
	text     string // label, or tooltip for icon-only buttons
	showText bool
	width    int // fixed width for icon-only buttons (0: fit the content)
}

// toolbarStrings keeps button labels alive for the toolbars.
var toolbarStrings [][]uint16

// toolbarIcons remembers the image list order of each toolbar's icons, and
// toolbarIconSize their size.
var (
	toolbarIcons    = map[win.HWND][]string{}
	toolbarIconSize = map[win.HWND]int{}
)

// setToolbarIcons (re)renders a toolbar's icons in the current palette.
func setToolbarIcons(tb *ui.Toolbar) {
	h := tb.Hwnd()
	icons, size := toolbarIcons[h], toolbarIconSize[h]
	for _, m := range []co.WM{co.TB_SETIMAGELIST, co.TB_SETDISABLEDIMAGELIST} {
		c := colors.icon
		if m == co.TB_SETDISABLEDIMAGELIST {
			c = colors.iconDisabled
		}
		if old, _ := h.SendMessage(m, 0, win.LPARAM(fluentImageList(icons, size, c))); old != 0 {
			win.HIMAGELIST(old).Destroy()
		}
	}
	procInvalidateRect.Call(uintptr(h), 0, 1)
}

// newToolbar creates a flat toolbar that paints its background in bg() (a
// COLORREF of the current palette) instead of the grey of the themed
// toolbar, and does not position itself (the window's layout code places
// it). The background goes through TBSTYLE_CUSTOMERASE (the toolbar asks its
// owner to erase it) and is also painted at the start of each paint, below
// the buttons. In light mode Windows draws the buttons with their normal
// hover, pressed and checked looks; in dark mode those looks are painted
// here (the toolbar theme has no dark variant), and so are the separators.
func newToolbar(parent ui.Parent, bg func() uint32) *ui.Toolbar {
	tb := ui.NewToolbar(parent, ui.OptsToolbar().
		CtrlStyle(co.TBSTYLE_FLAT|co.TBSTYLE_LIST|co.TBSTYLE_TOOLTIPS|co.TBSTYLE_CUSTOMERASE).
		CtrlExStyle(co.TBSTYLE_EX_MIXEDBUTTONS|co.TBSTYLE_EX_DOUBLEBUFFER).
		WndStyle(co.WS_CHILD|co.WS_VISIBLE|co.WS(ccsNoResize|ccsNoParentAlign|ccsNoDivider)))
	tb.On().NmCustomDraw(func(p *win.NMTBCUSTOMDRAW) co.CDRF {
		hdc := uintptr(p.Nmcd.Hdc)
		switch p.Nmcd.DwDrawStage {
		case co.CDDS_PREERASE:
			if rc, err := tb.Hwnd().GetClientRect(); err == nil {
				fillRect(hdc, rc, bg())
			}
			return co.CDRF_SKIPDEFAULT
		case co.CDDS_PREPAINT:
			if rc, err := tb.Hwnd().GetClientRect(); err == nil {
				fillRect(hdc, rc, bg())
			}
			if darkMode {
				return co.CDRF_NOTIFYITEMDRAW | co.CDRF_NOTIFYPOSTPAINT
			}
		case co.CDDS_ITEMPREPAINT:
			if !darkMode {
				break
			}
			st := p.Nmcd.UItemState
			switch {
			case st&co.CDIS_DISABLED != 0:
			case st&(co.CDIS_SELECTED|co.CDIS_CHECKED) != 0:
				roundRect(hdc, p.Nmcd.Rc, int32(ui.DpiX(4)), shade(bg(), 10))
			case st&co.CDIS_HOT != 0:
				roundRect(hdc, p.Nmcd.Rc, int32(ui.DpiX(4)), shade(bg(), 18))
			}
			drawButton(tb, hdc, p.Nmcd)
			return co.CDRF_SKIPDEFAULT
		case co.CDDS_POSTPAINT:
			if darkMode {
				paintSeparators(tb, hdc, bg())
			}
		}
		return co.CDRF_DODEFAULT
	})
	return tb
}

// buttonLook is what drawButton needs to know about a toolbar button.
type buttonLook struct {
	image    int32
	text     string
	showText bool
}

// toolbarButtons holds the look of each toolbar's buttons, by command id.
var toolbarButtons = map[win.HWND]map[uint16]buttonLook{}

// drawButton paints a button's icon and label, centred in its rectangle,
// in the colours of the current palette (the themed toolbar would draw
// disabled labels in the grey of the light theme).
func drawButton(tb *ui.Toolbar, hdc uintptr, cd win.NMCUSTOMDRAW) {
	h := tb.Hwnd()
	b, ok := toolbarButtons[h][uint16(cd.DwItemSpec)]
	if !ok {
		return
	}
	disabled := cd.UItemState&co.CDIS_DISABLED != 0
	msg, fg := co.TB_GETIMAGELIST, colors.text
	if disabled {
		msg, fg = co.TB_GETDISABLEDIMAGELIST, colors.textDisabled
	}
	imgList, _ := h.SendMessage(msg, 0, 0)
	size := int32(toolbarIconSize[h])
	rc := cd.Rc
	gap, textW := int32(0), int32(0)
	font, _ := h.SendMessage(co.WM_GETFONT, 0, 0)
	old, _, _ := procSelectObject.Call(hdc, font)
	defer procSelectObject.Call(hdc, old)
	if b.showText {
		gap, textW = int32(ui.DpiX(6)), textWidth(hdc, b.text)
	}
	x := rc.Left + (rc.Right-rc.Left-size-gap-textW)/2
	y := rc.Top + (rc.Bottom-rc.Top-size)/2
	procImageListDraw.Call(imgList, uintptr(b.image), hdc, uintptr(x), uintptr(y), 1) // ILD_TRANSPARENT
	if b.showText {
		procSetBkMode.Call(hdc, 1) // TRANSPARENT
		procSetTextColor.Call(hdc, uintptr(fg))
		drawTextV(hdc, b.text, win.RECT{Left: x + size + gap, Top: rc.Top, Right: rc.Right, Bottom: rc.Bottom})
	}
}

// paintSeparators paints the toolbar's separators as a thin line in the
// divider colour over what the light toolbar theme drew.
func paintSeparators(tb *ui.Toolbar, hdc uintptr, bg uint32) {
	h := tb.Hwnd()
	n, _ := h.SendMessage(co.TB_BUTTONCOUNT, 0, 0)
	for i := 0; i < int(n); i++ {
		var tbb win.TBBUTTON
		h.SendMessage(co.TB_GETBUTTON, win.WPARAM(i), win.LPARAM(unsafe.Pointer(&tbb)))
		if tbb.FsStyle&co.BTNS_SEP == 0 {
			continue
		}
		var rc win.RECT
		h.SendMessage(co.TB_GETITEMRECT, win.WPARAM(i), win.LPARAM(unsafe.Pointer(&rc)))
		fillRect(hdc, rc, bg)
		x := (rc.Left + rc.Right) / 2
		pad := (rc.Bottom - rc.Top) / 4
		fillRect(hdc, win.RECT{Left: x, Top: rc.Top + pad, Right: x + 1, Bottom: rc.Bottom - pad}, colors.tabSeparator)
	}
}

// fillRect fills a rectangle with a solid COLORREF.
func fillRect(hdc uintptr, rc win.RECT, color uint32) {
	brush, _, _ := procCreateSolidBrush.Call(uintptr(color))
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(&rc)), brush)
	procDeleteObject.Call(brush)
}

// setupToolbar sets the Fluent image lists for the given icons, the padding
// inside buttons and the spacing between them. Call it once, in WM_CREATE.
func setupToolbar(tb *ui.Toolbar, icons []string, size, padX, padY, spacing int) {
	h := tb.Hwnd()
	toolbarIcons[h] = icons
	toolbarIconSize[h] = size
	setToolbarIcons(tb)
	h.SendMessage(co.TB_SETPADDING, 0, win.LPARAM(uint32(padX)|uint32(padY)<<16))
	// Wine does not implement TB_SETMETRICS (it logs an error); there the
	// buttons simply sit a little closer together.
	if !isWine() {
		type tbMetrics struct {
			size, mask                       uint32
			cxPad, cyPad, cxBarPad, cyBarPad int32
			cxButtonSpacing, cyButtonSpacing int32
		}
		m := tbMetrics{mask: 0x4, cxButtonSpacing: int32(spacing)} // TBMF_BUTTONSPACING
		m.size = uint32(unsafe.Sizeof(m))
		h.SendMessage(co.TB_SETMETRICS, 0, win.LPARAM(unsafe.Pointer(&m)))
	}
}

var procWineGetVersion = syscall.NewLazyDLL("ntdll.dll").NewProc("wine_get_version")

// isWine reports whether the program runs under Wine (ntdll exports
// wine_get_version there, never on Windows).
func isWine() bool { return procWineGetVersion.Find() == nil }

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

// setButtonWidth sets a button's width (TB_SETBUTTONINFO with TBIF_SIZE).
func setButtonWidth(tb *ui.Toolbar, id uint16, cx int) {
	bi := tbButtonInfo{mask: 0x40, cx: uint16(cx)} // TBIF_SIZE
	bi.size = uint32(unsafe.Sizeof(bi))
	tb.Hwnd().SendMessage(co.WM_USER+64, win.WPARAM(id), win.LPARAM(unsafe.Pointer(&bi))) // TB_SETBUTTONINFOW
}

// buttonScreenRect returns a toolbar button's rectangle in screen coordinates.
func buttonScreenRect(tb *ui.Toolbar, id uint16) win.RECT {
	var rc win.RECT
	tb.Hwnd().SendMessage(co.TB_GETRECT, win.WPARAM(id), win.LPARAM(unsafe.Pointer(&rc)))
	tl := win.POINT{X: rc.Left, Y: rc.Top}
	br := win.POINT{X: rc.Right, Y: rc.Bottom}
	tb.Hwnd().ClientToScreenPt(&tl)
	tb.Hwnd().ClientToScreenPt(&br)
	return win.RECT{Left: tl.X, Top: tl.Y, Right: br.X, Bottom: br.Y}
}

// addButtons appends buttons; icons are looked up in the toolbar's icon list.
func addButtons(tb *ui.Toolbar, buttons []tbButton) {
	h := tb.Hwnd()
	index := map[string]int32{}
	for i, name := range toolbarIcons[h] {
		index[name] = int32(i)
	}
	for _, b := range buttons {
		tbb := win.TBBUTTON{FsState: co.TBSTATE_ENABLED}
		if b.icon == "" {
			tbb.FsStyle = co.BTNS_SEP
			tbb.IBitmap = int32(ui.DpiX(16))
		} else {
			text := syscall.StringToUTF16(b.text)
			toolbarStrings = append(toolbarStrings, text)
			tbb.IBitmap = index[b.icon]
			tbb.IdCommand = int32(b.id)
			if toolbarButtons[h] == nil {
				toolbarButtons[h] = map[uint16]buttonLook{}
			}
			toolbarButtons[h][b.id] = buttonLook{image: tbb.IBitmap, text: b.text, showText: b.showText}
			tbb.FsStyle = co.BTNS_AUTOSIZE
			if b.width > 0 && !b.showText {
				tbb.FsStyle = 0 // fixed width, set below
			}
			if b.showText {
				tbb.FsStyle |= co.BTNS_SHOWTEXT
			}
			tbb.IString = &text[0]
		}
		h.SendMessage(co.TB_ADDBUTTONS, 1, win.LPARAM(unsafe.Pointer(&tbb)))
		if b.icon != "" && b.width > 0 && !b.showText {
			setButtonWidth(tb, b.id, b.width)
		}
	}
	h.SendMessage(co.TB_AUTOSIZE, 0, 0)
}

// fillToolbar sets up a toolbar with exactly the icons of its buttons.
func fillToolbar(tb *ui.Toolbar, buttons []tbButton, size, padX, padY, spacing int) {
	var icons []string
	seen := map[string]bool{}
	for _, b := range buttons {
		if b.icon != "" && !seen[b.icon] {
			icons = append(icons, b.icon)
			seen[b.icon] = true
		}
	}
	setupToolbar(tb, icons, size, padX, padY, spacing)
	addButtons(tb, buttons)
}

// toolbarSize returns the size a toolbar needs for all its buttons.
func toolbarSize(tb *ui.Toolbar) win.SIZE {
	var sz win.SIZE
	tb.Hwnd().SendMessage(co.TB_GETMAXSIZE, 0, win.LPARAM(unsafe.Pointer(&sz)))
	return sz
}

func enableButton(tb *ui.Toolbar, id uint16, on bool) {
	v := 0
	if on {
		v = 1
	}
	tb.Hwnd().SendMessage(co.TB_ENABLEBUTTON, win.WPARAM(id), win.LPARAM(v))
}

// setWin11Frame asks for rounded corners and a Mica title bar (Windows 11;
// ignored elsewhere).
func setWin11Frame(h win.HWND) {
	for _, a := range [][2]uint32{{dwmaCornerPref, dwmcpRound}, {dwmaBackdropType, dwmsbtMainWindow}} {
		v := a[1]
		procDwmSetWindowAttr.Call(uintptr(h), uintptr(a[0]), uintptr(unsafe.Pointer(&v)), 4)
	}
}

// logFont is LOGFONTW (92 bytes).
type logFont struct {
	height, width, escapement, orientation, weight int32
	italic, underline, strikeOut, charSet          byte
	outPrecision, clipPrecision, quality, pitch    byte
	faceName                                       [32]uint16
}

// nonClientMetrics is NONCLIENTMETRICSW including iPaddedBorderWidth.
type nonClientMetrics struct {
	size                                   uint32
	borderWidth, scrollWidth, scrollHeight int32
	captionWidth, captionHeight            int32
	captionFont                            logFont
	smCaptionWidth, smCaptionHeight        int32
	smCaptionFont                          logFont
	menuWidth, menuHeight                  int32
	menuFont, statusFont, messageFont      logFont
	paddedBorderWidth                      int32
}

// uiFont returns the Windows 11 UI font (Segoe UI Variable Text) at the size
// of the system message font plus grow pixels, or the system message font
// itself when the variable font is not installed (as under Wine).
func uiFont(grow int) uintptr { return uiFontWeight(grow, 0) }

// uiFontWeight is uiFont with a font weight (0 keeps the normal weight;
// 600 is semibold).
func uiFontWeight(grow, weight int) uintptr {
	var ncm nonClientMetrics
	ncm.size = uint32(unsafe.Sizeof(ncm))
	if r, _, _ := procSystemParametersInfo.Call(spiNonClientMetrics, uintptr(ncm.size), uintptr(unsafe.Pointer(&ncm)), 0); r == 0 {
		return 0
	}
	base := ncm.messageFont
	if weight > 0 {
		base.weight = int32(weight)
	}
	if base.height < 0 { // negative: character height in pixels
		base.height -= int32(grow)
	} else {
		base.height += int32(grow)
	}
	lf := base
	const face = "Segoe UI Variable Text"
	lf.faceName = [32]uint16{}
	copy(lf.faceName[:], syscall.StringToUTF16(face))
	h, _, _ := procCreateFontIndirectW.Call(uintptr(unsafe.Pointer(&lf)))
	if h != 0 && fontFace(h) == face {
		return h
	}
	if h != 0 {
		procDeleteObject.Call(h)
	}
	h, _, _ = procCreateFontIndirectW.Call(uintptr(unsafe.Pointer(&base)))
	return h
}

// fontHeight returns the line height of a font in pixels.
func fontHeight(hFont uintptr) int {
	hdc, _, _ := procGetDC.Call(0)
	if hdc == 0 {
		return 0
	}
	defer procReleaseDC.Call(0, hdc)
	old, _, _ := procSelectObject.Call(hdc, hFont)
	defer procSelectObject.Call(hdc, old)
	var tm [64]byte // TEXTMETRICW; tmHeight is the first field
	procGetTextMetricsW.Call(hdc, uintptr(unsafe.Pointer(&tm[0])))
	return int(int32(uint32(tm[0]) | uint32(tm[1])<<8 | uint32(tm[2])<<16 | uint32(tm[3])<<24))
}

// fontFace returns the face name GDI actually picked for a font.
func fontFace(hFont uintptr) string {
	hdc, _, _ := procGetDC.Call(0)
	if hdc == 0 {
		return ""
	}
	defer procReleaseDC.Call(0, hdc)
	old, _, _ := procSelectObject.Call(hdc, hFont)
	defer procSelectObject.Call(hdc, old)
	var buf [64]uint16
	procGetTextFaceW.Call(hdc, uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])))
	return syscall.UTF16ToString(buf[:])
}

// applyFont sets a font on a window and all its children.
func applyFont(h win.HWND, hFont uintptr) {
	if hFont == 0 {
		return
	}
	h.SendMessage(co.WM_SETFONT, win.WPARAM(hFont), 0)
	for _, c := range h.EnumChildWindows() {
		c.SendMessage(co.WM_SETFONT, win.WPARAM(hFont), 1)
	}
}

func init() {
	if unsafe.Sizeof(logFont{}) != 92 || unsafe.Sizeof(nonClientMetrics{}) != 504 {
		panic("unexpected LOGFONTW/NONCLIENTMETRICSW layout")
	}
}
