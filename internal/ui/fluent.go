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

// Colours of Windows 11 text and disabled text on a light background.
var (
	iconColor         = color.RGBA{0x1a, 0x1a, 0x1a, 0xff}
	iconDisabledColor = color.RGBA{0xa0, 0xa0, 0xa0, 0xff}
)

// Common control styles missing from windigo's co package.
const (
	ccsNoResize         = 0x0004
	ccsNoParentAlign    = 0x0008
	ccsNoDivider        = 0x0040
	iImageNone          = -2
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

// bitmapFromImage makes a 32-bit top-down DIB section with premultiplied
// alpha (what image lists with ILC_COLOR32 expect) from an image.RGBA.
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
	for i := 0; i < w*h; i++ { // RGBA (premultiplied) to BGRA
		dst[i*4+0] = img.Pix[i*4+2]
		dst[i*4+1] = img.Pix[i*4+1]
		dst[i*4+2] = img.Pix[i*4+0]
		dst[i*4+3] = img.Pix[i*4+3]
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
}

// toolbarStrings keeps button labels alive for the toolbars.
var toolbarStrings [][]uint16

// newToolbar creates a flat, transparent toolbar that does not position
// itself (it is placed by the window's layout code).
func newToolbar(parent ui.Parent) *ui.Toolbar {
	return ui.NewToolbar(parent, ui.OptsToolbar().
		CtrlStyle(co.TBSTYLE_FLAT|co.TBSTYLE_LIST|co.TBSTYLE_TRANSPARENT|co.TBSTYLE_TOOLTIPS).
		CtrlExStyle(co.TBSTYLE_EX_MIXEDBUTTONS|co.TBSTYLE_EX_DOUBLEBUFFER).
		WndStyle(co.WS_CHILD|co.WS_VISIBLE|co.WS(ccsNoResize|ccsNoParentAlign|ccsNoDivider)))
}

// fillToolbar sets the Fluent image lists and adds the buttons. Call it
// after the toolbar exists (in WM_CREATE).
func fillToolbar(tb *ui.Toolbar, buttons []tbButton, size int) {
	var names []string
	for _, b := range buttons {
		if b.icon != "" {
			names = append(names, b.icon)
		}
	}
	h := tb.Hwnd()
	h.SendMessage(co.TB_SETIMAGELIST, 0, win.LPARAM(fluentImageList(names, size, iconColor)))
	h.SendMessage(co.TB_SETDISABLEDIMAGELIST, 0, win.LPARAM(fluentImageList(names, size, iconDisabledColor)))
	pad := ui.DpiX(10)
	h.SendMessage(co.TB_SETPADDING, 0, win.LPARAM(uint32(pad)|uint32(pad)<<16))

	idx := int32(0)
	for _, b := range buttons {
		tbb := win.TBBUTTON{FsState: co.TBSTATE_ENABLED}
		if b.icon == "" {
			tbb.FsStyle = co.BTNS_SEP
			tbb.IBitmap = int32(ui.DpiX(8))
		} else {
			text := syscall.StringToUTF16(b.text)
			toolbarStrings = append(toolbarStrings, text)
			tbb.IBitmap = idx
			tbb.IdCommand = int32(b.id)
			tbb.FsStyle = co.BTNS_AUTOSIZE
			if b.showText {
				tbb.FsStyle |= co.BTNS_SHOWTEXT
			}
			tbb.IString = &text[0]
			idx++
		}
		h.SendMessage(co.TB_ADDBUTTONS, 1, win.LPARAM(unsafe.Pointer(&tbb)))
	}
	h.SendMessage(co.TB_AUTOSIZE, 0, 0)
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

// setExplorerTheme gives a list or tree view the look of Windows Explorer.
func setExplorerTheme(h win.HWND) {
	name, _ := syscall.UTF16PtrFromString("Explorer")
	procSetWindowTheme.Call(uintptr(h), uintptr(unsafe.Pointer(name)), 0)
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
// of the system message font, or the system message font itself when the
// variable font is not installed (Windows 10).
func uiFont() uintptr {
	var ncm nonClientMetrics
	ncm.size = uint32(unsafe.Sizeof(ncm))
	if r, _, _ := procSystemParametersInfo.Call(spiNonClientMetrics, uintptr(ncm.size), uintptr(unsafe.Pointer(&ncm)), 0); r == 0 {
		return 0
	}
	lf := ncm.messageFont
	const face = "Segoe UI Variable Text"
	copy(lf.faceName[:], syscall.StringToUTF16(face))
	hFont, _, _ := procCreateFontIndirectW.Call(uintptr(unsafe.Pointer(&lf)))
	if hFont != 0 && fontFace(hFont) == face {
		return hFont
	}
	if hFont != 0 {
		procDeleteObject.Call(hFont)
	}
	hFont, _, _ = procCreateFontIndirectW.Call(uintptr(unsafe.Pointer(&ncm.messageFont)))
	return hFont
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
