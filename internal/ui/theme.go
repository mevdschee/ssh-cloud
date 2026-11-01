//go:build windows

package ui

// Light and dark mode. The window follows the "app mode" of Windows
// (Settings > Personalization > Colors), also when it changes while the app
// runs. Everything painted by the app itself takes its colours from the
// current palette; the common controls get the dark variants of the
// Explorer themes, the same ones Windows Explorer and Notepad use. Menus and
// scrollbars only turn dark through two undocumented uxtheme functions,
// which exist on Windows 10 1809 and later.
//
// SSHCLOUD_THEME=dark (or light) overrides the system setting, which helps
// to try the dark look under Wine.

import (
	"image/color"
	"os"
	"syscall"
	"unsafe"

	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
	"github.com/rodrigocfd/windigo/win"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// palette holds the colours of one mode (COLORREF, 0x00BBGGRR, except the
// icon colours).
type palette struct {
	window        uint32 // command bar, tree, list and status line
	nav           uint32 // navigation row and the active tab
	band          uint32 // the tab band at the top
	divider       uint32 // thin lines between the rows
	addressBorder uint32
	addressFill   uint32
	accent        uint32 // focus underline of the address bar, links
	text          uint32
	textDisabled  uint32
	tabHover      uint32
	tabCloseHover uint32
	tabSeparator  uint32
	dialog        uint32 // dialog background
	field         uint32 // edit and combo box background in dialogs
	selection     uint32 // selected line in a list (dark mode only)
	selBorder     uint32 // frame around the focused selected line (dark mode only)
	icon          color.RGBA
	iconDisabled  color.RGBA
}

// Windows 11 Explorer colours, light and dark.
var (
	lightPalette = palette{
		window:        0x00FFFFFF,
		nav:           0x00FFFFFF,
		band:          0x00F3F3F3,
		divider:       0x00E5E5E5,
		addressBorder: 0x00D6D6D6,
		addressFill:   0x00FFFFFF,
		accent:        0x00C06700, // #0067C0
		text:          0x001A1A1A,
		textDisabled:  0x00A0A0A0,
		tabHover:      0x00EAEAEA,
		tabCloseHover: 0x00DCDCDC,
		tabSeparator:  0x00D0D0D0,
		dialog:        0x00F0F0F0, // replaced by COLOR_BTNFACE at startup
		field:         0x00FFFFFF,
		icon:          color.RGBA{0x1a, 0x1a, 0x1a, 0xff},
		iconDisabled:  color.RGBA{0xa0, 0xa0, 0xa0, 0xff},
	}
	darkPalette = palette{
		window:        0x00191919,
		nav:           0x002C2C2C,
		band:          0x00202020,
		divider:       0x002B2B2B,
		addressBorder: 0x00383838,
		addressFill:   0x00383838,
		accent:        0x00FFCD60, // #60CDFF
		text:          0x00FFFFFF,
		textDisabled:  0x005C5C5C,
		tabHover:      0x002D2D2D,
		tabCloseHover: 0x003D3D3D,
		tabSeparator:  0x003A3A3A,
		dialog:        0x00202020,
		field:         0x002D2D2D,
		selection:     0x00505050,
		selBorder:     0x00C3C3C3,
		icon:          color.RGBA{0xe0, 0xe0, 0xe0, 0xff},
		iconDisabled:  color.RGBA{0x5c, 0x5c, 0x5c, 0xff},
	}
)

var (
	darkMode bool           // the current mode
	colors   = lightPalette // the current palette
)

var (
	procSetWindowSubclass = comctl32.NewProc("SetWindowSubclass")
	procDefSubclassProc   = comctl32.NewProc("DefSubclassProc")
	procGetClassNameW     = user32.NewProc("GetClassNameW")
	procRedrawWindow      = user32.NewProc("RedrawWindow")
)

// Undocumented uxtheme exports, by ordinal (0 when missing).
var (
	uxAllowDarkModeForWindow uintptr // 133
	uxSetPreferredAppMode    uintptr // 135 (AllowDarkModeForApp on 1809)
	uxFlushMenuThemes        uintptr // 136
	uxRefreshImmersivePolicy uintptr // 104
)

const (
	dwmaUseImmersiveDarkMode   = 20
	dwmaUseImmersiveDarkModeV1 = 19 // before Windows 10 20H1
	spiGetHighContrast         = 0x0042
	hcfHighContrastOn          = 0x0001
)

func init() {
	if btnFace, _, _ := procGetSysColor.Call(uintptr(co.COLOR_BTNFACE)); btnFace != 0 {
		lightPalette.dialog = uint32(btnFace)
	}
	colors = lightPalette
	if isWine() {
		return // Wine has no such exports, and might have others there
	}
	if _, _, build := windows.RtlGetNtVersionNumbers(); build&0xffff < 17763 {
		return
	}
	h, err := windows.LoadLibraryEx("uxtheme.dll", 0, windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
	if err != nil {
		return
	}
	get := func(ord uintptr) uintptr {
		p, _ := windows.GetProcAddressByOrdinal(h, ord)
		return p
	}
	uxAllowDarkModeForWindow = get(133)
	uxSetPreferredAppMode = get(135)
	uxFlushMenuThemes = get(136)
	uxRefreshImmersivePolicy = get(104)
	if uxSetPreferredAppMode != 0 {
		syscall.SyscallN(uxSetPreferredAppMode, 1) // AllowDark: follow the system
	}
}

// systemDark reports whether Windows apps should be dark. With a High
// Contrast theme the app stays light, which uses the system colours.
func systemDark() bool {
	switch os.Getenv("SSHCLOUD_THEME") {
	case "dark":
		return true
	case "light":
		return false
	}
	type highContrast struct {
		size, flags uint32
		scheme      uintptr
	}
	hc := highContrast{size: uint32(unsafe.Sizeof(highContrast{}))}
	if r, _, _ := procSystemParametersInfo.Call(spiGetHighContrast, uintptr(hc.size), uintptr(unsafe.Pointer(&hc)), 0); r != 0 && hc.flags&hcfHighContrastOn != 0 {
		return false
	}
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("AppsUseLightTheme")
	return err == nil && v == 0
}

// loadTheme reads the system mode into darkMode and colors; it reports
// whether the mode changed.
func loadTheme() bool {
	if uxRefreshImmersivePolicy != 0 {
		syscall.SyscallN(uxRefreshImmersivePolicy)
	}
	dark := systemDark()
	changed := dark != darkMode
	darkMode = dark
	colors = lightPalette
	if dark {
		colors = darkPalette
	}
	if changed && uxFlushMenuThemes != 0 {
		syscall.SyscallN(uxFlushMenuThemes)
	}
	return changed
}

// isThemeChange reports whether a WM_SETTINGCHANGE is about light and dark.
func isThemeChange(p ui.Wm) bool {
	if p.LParam == 0 {
		return false
	}
	lp := uintptr(p.LParam)
	return windows.UTF16PtrToString(*(**uint16)(unsafe.Pointer(&lp))) == "ImmersiveColorSet"
}

// brushes caches one solid brush per colour; there are only a few.
var brushes = map[uint32]uintptr{}

func solidBrush(c uint32) win.HBRUSH {
	b, ok := brushes[c]
	if !ok {
		b, _, _ = procCreateSolidBrush.Call(uintptr(c))
		brushes[c] = b
	}
	return win.HBRUSH(b)
}

// shade lightens a colour by d per channel (darkens for negative d).
func shade(c uint32, d int) uint32 {
	var out uint32
	for i := 0; i < 3; i++ {
		v := int(c>>(8*i)&0xff) + d
		v = max(0, min(255, v))
		out |= uint32(v) << (8 * i)
	}
	return out
}

func setTheme(h win.HWND, app string, idList *string) {
	var a, l *uint16
	if app != "" || idList != nil {
		a, _ = syscall.UTF16PtrFromString(app)
	}
	if idList != nil {
		l, _ = syscall.UTF16PtrFromString(*idList)
	}
	procSetWindowTheme.Call(uintptr(h), uintptr(unsafe.Pointer(a)), uintptr(unsafe.Pointer(l)))
}

// themeFrame makes a window's title bar dark or light and allows dark
// menus and scrollbars for it.
func themeFrame(h win.HWND) {
	v := uint32(0)
	if darkMode {
		v = 1
	}
	allowDark(h, darkMode)
	if r, _, _ := procDwmSetWindowAttr.Call(uintptr(h), uintptr(dwmaUseImmersiveDarkMode), uintptr(unsafe.Pointer(&v)), 4); r != 0 {
		procDwmSetWindowAttr.Call(uintptr(h), uintptr(dwmaUseImmersiveDarkModeV1), uintptr(unsafe.Pointer(&v)), 4)
	}
}

// allowDark lets a window use the dark variants of its theme (and of its
// menus and scrollbars), where Windows has the export for it.
func allowDark(h win.HWND, on bool) {
	if uxAllowDarkModeForWindow != 0 {
		v := uintptr(0)
		if on {
			v = 1
		}
		syscall.SyscallN(uxAllowDarkModeForWindow, uintptr(h), v)
	}
}

// redrawAll repaints a window, its frame and all its children.
func redrawAll(h win.HWND) {
	const flags = 0x0001 | 0x0004 | 0x0080 | 0x0400 // RDW_INVALIDATE|RDW_ERASE|RDW_ALLCHILDREN|RDW_FRAME
	procRedrawWindow.Call(uintptr(h), 0, 0, flags)
}

// themeListView gives a list view (and its header and scrollbars) the
// Explorer look of the current mode.
func themeListView(h win.HWND) {
	header := listHeader(h)
	bg, _, _ := procGetSysColor.Call(uintptr(co.COLOR_WINDOW))
	fg, _, _ := procGetSysColor.Call(uintptr(co.COLOR_WINDOWTEXT))
	allowDark(h, darkMode)
	if darkMode {
		// ItemsView is the theme of Explorer's file list: a grey selection
		// and normal icons on selected rows (DarkMode_Explorer selects in
		// blue and tints the icon). Allowed dark, the window gets the dark
		// variant with dark scrollbars; without that export, ask for the
		// dark variant by name.
		if uxAllowDarkModeForWindow != 0 {
			setTheme(h, "ItemsView", nil)
		} else {
			setTheme(h, "DarkMode_ItemsView", nil)
		}
		if header != 0 {
			setTheme(header, "DarkMode_ItemsView", nil)
		}
		bg, fg = uintptr(colors.window), uintptr(colors.text)
	} else {
		setTheme(h, "Explorer", nil)
		if header != 0 {
			setTheme(header, "", nil)
		}
	}
	h.SendMessage(co.LVM_SETBKCOLOR, 0, win.LPARAM(bg))
	h.SendMessage(co.LVM_SETTEXTBKCOLOR, 0, win.LPARAM(bg))
	h.SendMessage(co.LVM_SETTEXTCOLOR, 0, win.LPARAM(fg))
	subclassOnce(h)
}

// themeTreeView gives a tree view the Explorer look of the current mode.
func themeTreeView(h win.HWND) {
	bg, fg := ^uintptr(0), ^uintptr(0) // -1: the system colours
	if darkMode {
		setTheme(h, "DarkMode_Explorer", nil)
		bg, fg = uintptr(colors.window), uintptr(colors.text)
	} else {
		setTheme(h, "Explorer", nil)
	}
	h.SendMessage(co.TVM_SETBKCOLOR, 0, win.LPARAM(bg))
	h.SendMessage(co.TVM_SETTEXTCOLOR, 0, win.LPARAM(fg))
}

// drawSelection draws the selected lines of a list in dark mode like
// Explorer: a grey fill, and a light frame around the focused line while the
// list has the focus. The dark list theme would select in grey with a blue
// frame. The control then draws the line as not selected, on top of the
// fill. Register it on every list view (it does nothing in light mode).
func drawSelection(list *ui.ListView) {
	list.On().NmCustomDraw(func(p *win.NMLVCUSTOMDRAW) co.CDRF {
		if !darkMode {
			return co.CDRF_DODEFAULT
		}
		h := list.Hwnd()
		idx := p.Nmcd.DwItemSpec
		state := func() co.LVIS {
			s, _ := h.SendMessage(co.LVM_GETITEMSTATE, win.WPARAM(idx), win.LPARAM(co.LVIS_SELECTED|co.LVIS_FOCUSED))
			return co.LVIS(s)
		}
		bounds := func() win.RECT {
			rc := win.RECT{Left: int32(co.LVIR_BOUNDS)}
			h.SendMessage(co.LVM_GETITEMRECT, win.WPARAM(idx), win.LPARAM(unsafe.Pointer(&rc)))
			return rc
		}
		hdc := uintptr(p.Nmcd.Hdc)
		switch p.Nmcd.DwDrawStage {
		case co.CDDS_PREPAINT:
			return co.CDRF_NOTIFYITEMDRAW
		case co.CDDS_ITEMPREPAINT:
			p.Nmcd.UItemState &^= co.CDIS_FOCUS // no dotted focus rectangle
			if state()&co.LVIS_SELECTED == 0 {
				return co.CDRF_DODEFAULT
			}
			p.Nmcd.UItemState &^= co.CDIS_SELECTED | co.CDIS_HOT
			fillRect(hdc, bounds(), colors.selection)
			p.ClrText = win.COLORREF(colors.text)
			p.ClrTextBk = win.COLORREF(colors.selection)
			return co.CDRF_NOTIFYPOSTPAINT
		case co.CDDS_ITEMPOSTPAINT:
			if st := state(); st&co.LVIS_SELECTED != 0 && st&co.LVIS_FOCUSED != 0 && win.GetFocus() == h {
				rc := bounds()
				for _, r := range []win.RECT{
					{Left: rc.Left, Top: rc.Top, Right: rc.Right, Bottom: rc.Top + 1},
					{Left: rc.Left, Top: rc.Bottom - 1, Right: rc.Right, Bottom: rc.Bottom},
					{Left: rc.Left, Top: rc.Top, Right: rc.Left + 1, Bottom: rc.Bottom},
					{Left: rc.Right - 1, Top: rc.Top, Right: rc.Right, Bottom: rc.Bottom},
				} {
					fillRect(hdc, r, colors.selBorder)
				}
			}
		}
		return co.CDRF_DODEFAULT
	})
}

// The list view subclass colours the header text (the dark header theme
// keeps black text) and the edit box for renaming in dark mode.
var (
	subclassed     = map[win.HWND]bool{}
	listSubclassCb = syscall.NewCallback(listSubclassProc)
)

const (
	nmCustomDraw   = ^uint32(12 - 1) // NM_CUSTOMDRAW (-12)
	wmCtlColorEdit = 0x0133
	wmNotify       = 0x004E
)

func listHeader(list win.HWND) win.HWND {
	h, _ := list.SendMessage(co.LVM_GETHEADER, 0, 0)
	return win.HWND(h)
}

func subclassOnce(h win.HWND) {
	if subclassed[h] {
		return
	}
	subclassed[h] = true
	procSetWindowSubclass.Call(uintptr(h), listSubclassCb, 0x5343, 0) // id "SC"
}

func listSubclassProc(hwnd, msg, wParam, lParam, id, ref uintptr) uintptr {
	if darkMode {
		switch uint32(msg) {
		case wmNotify:
			// lParam points to the notification (the cast keeps go vet quiet).
			hdr := *(**win.NMHDR)(unsafe.Pointer(&lParam))
			if hdr.HWndFrom == listHeader(win.HWND(hwnd)) && hdr.Code == nmCustomDraw {
				cd := *(**win.NMCUSTOMDRAW)(unsafe.Pointer(&lParam))
				switch cd.DwDrawStage {
				case co.CDDS_PREPAINT:
					return uintptr(co.CDRF_NOTIFYITEMDRAW)
				case co.CDDS_ITEMPREPAINT:
					procSetTextColor.Call(uintptr(cd.Hdc), uintptr(colors.text))
					return uintptr(co.CDRF_DODEFAULT)
				}
			}
		case wmCtlColorEdit:
			procSetTextColor.Call(wParam, uintptr(colors.text))
			procSetBkColor.Call(wParam, uintptr(colors.field))
			return uintptr(solidBrush(colors.field))
		}
	}
	r, _, _ := procDefSubclassProc.Call(hwnd, msg, wParam, lParam)
	return r
}

var (
	procSetBkColor      = gdi32.NewProc("SetBkColor")
	procIsWindowEnabled = user32.NewProc("IsWindowEnabled")
)

func className(h win.HWND) string {
	var buf [64]uint16
	procGetClassNameW.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf[:])
}

// newModal is ui.NewModal in the current mode: dialog background, label,
// edit, button and list colours, and the dark control themes. Callers must
// not handle WM_CTLCOLOR* or WM_SHOWWINDOW themselves.
func newModal(parent ui.Parent, opts *ui.VarOptsModal) *ui.Modal {
	pal := colors
	opts.ClassBrush(solidBrush(pal.dialog))
	wnd := ui.NewModal(parent, opts)
	on := wnd.On()
	on.WmCtlColorStatic(func(p ui.WmCtlColor) win.HBRUSH {
		fg := pal.text
		if r, _, _ := procIsWindowEnabled.Call(uintptr(p.HwndControl())); r == 0 {
			fg = pal.textDisabled
		}
		procSetTextColor.Call(uintptr(p.Hdc()), uintptr(fg))
		p.Hdc().SetBkColor(win.COLORREF(pal.dialog))
		return solidBrush(pal.dialog)
	})
	if darkMode {
		field := func(p ui.WmCtlColor) win.HBRUSH {
			procSetTextColor.Call(uintptr(p.Hdc()), uintptr(pal.text))
			p.Hdc().SetBkColor(win.COLORREF(pal.field))
			return solidBrush(pal.field)
		}
		on.WmCtlColorEdit(field)
		on.WmCtlColorListBox(field)
		on.WmCtlColorBtn(func(p ui.WmCtlColor) win.HBRUSH {
			p.Hdc().SetBkColor(win.COLORREF(pal.dialog))
			return solidBrush(pal.dialog)
		})
	}
	on.Wm(co.WM_SHOWWINDOW, func(p ui.Wm) uintptr {
		if p.WParam != 0 {
			themeFrame(wnd.Hwnd())
			if darkMode {
				for _, c := range wnd.Hwnd().EnumChildWindows() {
					themeControl(c)
				}
			}
		}
		return wnd.Hwnd().DefWindowProc(p.Msg, p.WParam, p.LParam)
	})
	return wnd
}

// themeControl gives a control in a dialog its dark theme.
func themeControl(h win.HWND) {
	switch className(h) {
	case "Button", "ScrollBar":
		setTheme(h, "DarkMode_Explorer", nil)
	case "Edit", "ComboBox":
		setTheme(h, "DarkMode_CFD", nil)
	case "SysListView32":
		themeListView(h)
	case "SysTreeView32":
		themeTreeView(h)
	default:
		return
	}
	h.SendMessage(co.WM_THEMECHANGED, 0, 0)
}
