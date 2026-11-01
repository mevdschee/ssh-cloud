//go:build windows

package ui

// tabStrip draws Explorer-style tabs in a band below the title bar: rounded
// tabs with the connection name, the active tab white and
// joined to the white row below, a close button on the active and the
// hovered tab, and a + button. It is a small custom control painted with GDI
// (double buffered).

import (
	"syscall"
	"unsafe"

	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
	"github.com/rodrigocfd/windigo/win"
)

var (
	procDrawTextW              = user32.NewProc("DrawTextW")
	procSetBkMode              = gdi32.NewProc("SetBkMode")
	procSetTextColor           = gdi32.NewProc("SetTextColor")
	comctl32                   = syscall.NewLazyDLL("comctl32.dll")
	procImageListDraw          = comctl32.NewProc("ImageList_Draw")
	procCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	procBitBlt                 = gdi32.NewProc("BitBlt")
	procDeleteDC               = gdi32.NewProc("DeleteDC")
	procRoundRect              = gdi32.NewProc("RoundRect")
	procGetStockObject         = gdi32.NewProc("GetStockObject")
	procGetTextExtentPoint32W  = gdi32.NewProc("GetTextExtentPoint32W")
	procTrackMouseEvent        = user32.NewProc("TrackMouseEvent")
)

const (
	dtLeft        = 0x0000
	dtSingleLine  = 0x0020
	dtNoPrefix    = 0x0800
	dtEndEllipsis = 0x8000

	textColor          = 0x001A1A1A
	tabHoverColor      = 0x00EAEAEA
	tabCloseHoverColor = 0x00DCDCDC
	tabSeparatorColor  = 0x00D0D0D0
)

type tabStrip struct {
	ctl *ui.Control

	names   []string
	active  int
	enabled bool

	hover, hoverClose int // tab under the mouse, tab whose close button is under the mouse
	hoverPlus         bool
	tracking          bool

	tabs   []win.RECT
	closes []win.RECT
	plus   win.RECT

	font                uintptr
	icons               uintptr // image list: add (at the strip's icon size)
	smallIcons          uintptr // image list: dismiss (close button)
	iconSize, smallSize int

	onSelect func(i int)
	onClose  func(i int)
	onNew    func()
	onMenu   func(i int) // right-click; i is -1 outside the tabs
}

func newTabStrip(parent ui.Parent) *tabStrip {
	brush, _, _ := procCreateSolidBrush.Call(bandColor)
	t := &tabStrip{active: -1, hover: -1, hoverClose: -1, enabled: true}
	t.ctl = ui.NewControl(parent, ui.OptsControl().
		Position(0, 0).Size(ui.Dpi(800, 40)).
		ClassBrush(win.HBRUSH(brush)).
		Style(co.WS_CHILD|co.WS_VISIBLE|co.WS_CLIPSIBLINGS).
		ExStyle(co.WS_EX_LEFT))
	on := t.ctl.On()
	on.WmPaint(t.paint)
	on.WmEraseBkgnd(func(ui.WmEraseBkgnd) int { return 1 }) // painted in WM_PAINT
	on.WmSize(func(ui.WmSize) { t.invalidate() })
	on.WmMouseMove(func(p ui.WmMouse) { t.mouseMove(p.Pos()) })
	on.WmMouseLeave(func() {
		t.tracking = false
		t.setHover(-1, -1, false)
	})
	on.WmLButtonDown(func(p ui.WmMouse) { t.click(p.Pos()) })
	on.WmMButtonUp(func(p ui.WmMouse) {
		if i, _ := t.hit(p.Pos()); i >= 0 && t.enabled && t.onClose != nil {
			t.onClose(i)
		}
	})
	on.WmRButtonUp(func(p ui.WmMouse) {
		i, _ := t.hit(p.Pos())
		if t.onMenu != nil {
			t.onMenu(i)
		}
	})
	return t
}

// setup prepares fonts and icons; called in WM_CREATE of the window.
func (t *tabStrip) setup(font uintptr, dpi int) {
	t.font = font
	t.iconSize = max(20, 20*dpi/96)
	t.smallSize = max(16, 16*dpi/96)
	t.icons = uintptr(fluentImageList([]string{"add"}, t.iconSize, iconColor))
	t.smallIcons = uintptr(fluentImageList([]string{"dismiss_16"}, t.smallSize, iconColor))
}

func (t *tabStrip) set(names []string, active int) {
	t.names, t.active = names, active
	t.hover, t.hoverClose = -1, -1
	t.invalidate()
}

func (t *tabStrip) setEnabled(on bool) { t.enabled = on }

func (t *tabStrip) invalidate() { procInvalidateRect.Call(uintptr(t.ctl.Hwnd()), 0, 0) }

// measure computes the tab, close and + rectangles for the current names.
func (t *tabStrip) measure(hdc uintptr, height int32) {
	pad := int32(ui.DpiX(12))
	top := int32(ui.DpiY(6))
	x := int32(ui.DpiX(8))
	t.tabs, t.closes = t.tabs[:0], t.closes[:0]
	old, _, _ := procSelectObject.Call(hdc, t.font)
	defer procSelectObject.Call(hdc, old)
	for _, name := range t.names {
		textW := textWidth(hdc, name)
		w := pad + textW + int32(ui.DpiX(8)) + int32(ui.DpiX(24)) + int32(ui.DpiX(6))
		w = max(int32(ui.DpiX(120)), min(w, int32(ui.DpiX(240))))
		r := win.RECT{Left: x, Top: top, Right: x + w, Bottom: height}
		cs := int32(ui.DpiX(24))
		c := win.RECT{Left: r.Right - int32(ui.DpiX(6)) - cs, Top: top + (height-top-cs)/2}
		c.Right, c.Bottom = c.Left+cs, c.Top+cs
		t.tabs = append(t.tabs, r)
		t.closes = append(t.closes, c)
		x += w + int32(ui.DpiX(2))
	}
	ps := int32(ui.DpiX(32))
	t.plus = win.RECT{Left: x + int32(ui.DpiX(4)), Top: top + (height-top-ps)/2}
	t.plus.Right, t.plus.Bottom = t.plus.Left+ps, t.plus.Top+ps
}

func textWidth(hdc uintptr, s string) int32 {
	if s == "" {
		return 0
	}
	p := syscall.StringToUTF16(s)
	var sz win.SIZE
	procGetTextExtentPoint32W.Call(hdc, uintptr(unsafe.Pointer(&p[0])), uintptr(len(p)-1), uintptr(unsafe.Pointer(&sz)))
	return sz.Cx
}

// roundRect fills a rounded rectangle (no outline).
func roundRect(hdc uintptr, r win.RECT, radius int32, color uint32) {
	brush, _, _ := procCreateSolidBrush.Call(uintptr(color))
	nullPen, _, _ := procGetStockObject.Call(8) // NULL_PEN
	oldB, _, _ := procSelectObject.Call(hdc, brush)
	oldP, _, _ := procSelectObject.Call(hdc, nullPen)
	procRoundRect.Call(hdc, uintptr(r.Left), uintptr(r.Top), uintptr(r.Right+1), uintptr(r.Bottom+1), uintptr(2*radius), uintptr(2*radius))
	procSelectObject.Call(hdc, oldB)
	procSelectObject.Call(hdc, oldP)
	procDeleteObject.Call(brush)
}

func (t *tabStrip) paint() {
	var ps paintStruct
	h := t.ctl.Hwnd()
	hdc, _, _ := procBeginPaint.Call(uintptr(h), uintptr(unsafe.Pointer(&ps)))
	defer procEndPaint.Call(uintptr(h), uintptr(unsafe.Pointer(&ps)))
	rc, err := h.GetClientRect()
	if hdc == 0 || err != nil || rc.Right == 0 {
		return
	}
	// Double buffer: draw into a memory bitmap, then copy at once.
	mem, _, _ := procCreateCompatibleDC.Call(hdc)
	bmp, _, _ := procCreateCompatibleBitmap.Call(hdc, uintptr(rc.Right), uintptr(rc.Bottom))
	oldBmp, _, _ := procSelectObject.Call(mem, bmp)
	defer func() {
		procSelectObject.Call(mem, oldBmp)
		procDeleteObject.Call(bmp)
		procDeleteDC.Call(mem)
	}()

	fillRect(mem, rc, bandColor)
	t.measure(mem, rc.Bottom)
	radius := int32(ui.DpiX(7))
	procSetBkMode.Call(mem, 1)
	oldFont, _, _ := procSelectObject.Call(mem, t.font)
	defer procSelectObject.Call(mem, oldFont)

	for i, r := range t.tabs {
		active, hover := i == t.active, i == t.hover
		switch {
		case active:
			// Rounded at the top only: extend below the strip, so the bottom
			// corners are cut off and the tab joins the white row below.
			roundRect(mem, win.RECT{Left: r.Left, Top: r.Top, Right: r.Right, Bottom: r.Bottom + radius}, radius, windowColor)
		case hover:
			roundRect(mem, win.RECT{Left: r.Left, Top: r.Top, Right: r.Right, Bottom: r.Bottom - int32(ui.DpiY(4))}, radius, tabHoverColor)
		default:
			// Thin separator after an inactive tab, unless next to the
			// active or hovered one.
			next := i + 1
			if next < len(t.tabs) && next != t.active && next != t.hover {
				x := r.Right + int32(ui.DpiX(1))
				fillRect(mem, win.RECT{Left: x, Top: r.Top + int32(ui.DpiY(8)), Right: x + 1, Bottom: r.Bottom - int32(ui.DpiY(10))}, tabSeparatorColor)
			}
		}
		textL := r.Left + int32(ui.DpiX(12))
		textR := r.Right - int32(ui.DpiX(6))
		if active || hover {
			textR = t.closes[i].Left - int32(ui.DpiX(4))
		}
		procSetTextColor.Call(mem, textColor)
		tr := win.RECT{Left: textL, Top: r.Top, Right: textR, Bottom: r.Bottom}
		if !active {
			tr.Bottom -= int32(ui.DpiY(4))
		}
		drawTextV(mem, t.names[i], tr)
		if active || hover {
			c := t.closes[i]
			if i == t.hoverClose {
				roundRect(mem, c, int32(ui.DpiX(4)), tabCloseHoverColor)
			}
			off := (c.Right - c.Left - int32(t.smallSize)) / 2
			procImageListDraw.Call(t.smallIcons, 0, mem, uintptr(c.Left+off), uintptr(c.Top+off), 1)
		}
	}
	if t.hoverPlus {
		roundRect(mem, t.plus, int32(ui.DpiX(4)), tabHoverColor)
	}
	off := (t.plus.Right - t.plus.Left - int32(t.iconSize)) / 2
	procImageListDraw.Call(t.icons, 0, mem, uintptr(t.plus.Left+off), uintptr(t.plus.Top+off), 1)

	procBitBlt.Call(hdc, 0, 0, uintptr(rc.Right), uintptr(rc.Bottom), mem, 0, 0, 0x00CC0020) // SRCCOPY
}

// drawTextV draws one line, vertically centred, with an ellipsis.
func drawTextV(hdc uintptr, s string, r win.RECT) {
	p, _ := syscall.UTF16FromString(s)
	procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(&p[0])), ^uintptr(0), uintptr(unsafe.Pointer(&r)),
		dtLeft|dtSingleLine|dtNoPrefix|dtEndEllipsis|0x0004) // DT_VCENTER
}

func inRect(r win.RECT, pt win.POINT) bool {
	return pt.X >= r.Left && pt.X < r.Right && pt.Y >= r.Top && pt.Y < r.Bottom
}

// hit returns the tab under pt (or -1) and whether pt is on its close button.
func (t *tabStrip) hit(pt win.POINT) (int, bool) {
	for i, r := range t.tabs {
		if inRect(r, pt) {
			onClose := (i == t.active || i == t.hover) && inRect(t.closes[i], pt)
			return i, onClose
		}
	}
	return -1, false
}

func (t *tabStrip) mouseMove(pt win.POINT) {
	if !t.tracking {
		type trackMouseEvent struct {
			size, flags uint32
			hwnd        win.HWND
			hoverTime   uint32
		}
		tme := trackMouseEvent{flags: 0x2, hwnd: t.ctl.Hwnd()} // TME_LEAVE
		tme.size = uint32(unsafe.Sizeof(tme))
		procTrackMouseEvent.Call(uintptr(unsafe.Pointer(&tme)))
		t.tracking = true
	}
	i, onClose := t.hit(pt)
	closeIdx := -1
	if i >= 0 && (onClose || inRect(t.closes[i], pt)) {
		closeIdx = i
	}
	t.setHover(i, closeIdx, inRect(t.plus, pt))
}

func (t *tabStrip) setHover(i, closeIdx int, plus bool) {
	if i != t.hover || closeIdx != t.hoverClose || plus != t.hoverPlus {
		t.hover, t.hoverClose, t.hoverPlus = i, closeIdx, plus
		t.invalidate()
	}
}

func (t *tabStrip) click(pt win.POINT) {
	if !t.enabled {
		return
	}
	if inRect(t.plus, pt) {
		if t.onNew != nil {
			t.onNew()
		}
		return
	}
	i, _ := t.hit(pt)
	if i < 0 {
		return
	}
	if inRect(t.closes[i], pt) {
		if t.onClose != nil {
			t.onClose(i)
		}
		return
	}
	if t.onSelect != nil {
		t.onSelect(i)
	}
}
