//go:build windows

package ui

// The divider between the folder tree and the file list can be dragged, like
// in Explorer. It is the gap between the two controls (they get all mouse
// input over themselves), so it is a few pixels wide with the line drawn in
// its middle. The width is saved in 96-DPI units; double-click resets it.

import (
	"log/slog"

	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
	"github.com/rodrigocfd/windigo/win"
)

const defaultTreeWidth = 220 // at 96 DPI

func splitterWidth() int { return ui.DpiX(5) }

// treeWidthFor returns the tree width for a client width: the saved width,
// kept between a minimum and what leaves the list room.
func (me *Explorer) treeWidthFor(cw int) int {
	w := me.cfg.TreeWidth
	if w <= 0 {
		w = defaultTreeWidth
	}
	return clampTreeWidth(ui.DpiX(w), cw)
}

func clampTreeWidth(w, cw int) int {
	return max(ui.DpiX(120), min(w, cw-ui.DpiX(300)))
}

// inSplitter reports whether a point in client coordinates is on the divider.
func (me *Explorer) inSplitter(pt win.POINT) bool {
	return int(pt.X) >= me.treeWidth && int(pt.X) < me.treeWidth+splitterWidth() &&
		int(pt.Y) > me.contentTop && int(pt.Y) < me.contentBottom
}

func (me *Explorer) splitterEvents() {
	on := me.wnd.On()
	sizeWE, _ := win.HINSTANCE(0).LoadCursor(win.CursorResIdc(co.IDC_SIZEWE))
	arrow, _ := win.HINSTANCE(0).LoadCursor(win.CursorResIdc(co.IDC_ARROW))
	// windigo does not call DefWindowProc when a handler exists, so set the
	// cursor for the window's own area here; over a child control (list,
	// tree, edit) return false so the child sets its own cursor.
	on.WmSetCursor(func(p ui.WmSetCursor) bool {
		if p.Hwnd() != me.wnd.Hwnd() {
			return false
		}
		const htClient = 1
		if p.Raw.LParam.LoWord() != htClient { // borders and title bar: resize cursors
			return me.wnd.Hwnd().DefWindowProc(co.WM_SETCURSOR, p.Raw.WParam, p.Raw.LParam) != 0
		}
		pt, err := win.GetCursorPos()
		if err == nil {
			me.wnd.Hwnd().ScreenToClientPt(&pt)
		}
		if me.splitDragging || (err == nil && me.inSplitter(pt)) {
			sizeWE.SetCursor()
		} else {
			arrow.SetCursor()
		}
		return true
	})
	on.WmLButtonDown(func(p ui.WmMouse) {
		if me.styled && me.inSplitter(p.Pos()) {
			me.splitDragging = true
			me.splitOffset = int(p.Pos().X) - me.treeWidth
			me.wnd.Hwnd().SetCapture()
		}
	})
	on.WmMouseMove(func(p ui.WmMouse) {
		if !me.splitDragging {
			return
		}
		rc, err := me.wnd.Hwnd().GetClientRect()
		if err != nil {
			return
		}
		w := clampTreeWidth(int(p.Pos().X)-me.splitOffset, int(rc.Right))
		if w != me.treeWidth {
			me.saveTreeWidth(w, false)
			me.layout()
		}
	})
	on.WmLButtonUp(func(p ui.WmMouse) {
		if me.splitDragging {
			me.splitDragging = false
			_ = win.ReleaseCapture()
			me.saveTreeWidth(me.treeWidth, true)
		}
	})
	on.WmCaptureChanged(func(ui.WmCaptureChanged) { me.splitDragging = false })
	on.WmLButtonDblClk(func(p ui.WmMouse) {
		if me.inSplitter(p.Pos()) {
			me.cfg.TreeWidth = 0
			me.saveTreeWidth(ui.DpiX(defaultTreeWidth), true)
			me.layout()
		}
	})
}

// saveTreeWidth remembers a width (in 96-DPI units); persist writes the
// settings file (only at the end of a drag).
func (me *Explorer) saveTreeWidth(w int, persist bool) {
	me.cfg.TreeWidth = w * 96 / max(me.wnd.Hwnd().GetDpiForWindow(), 1)
	if persist {
		if err := me.cfg.Save(me.cfgDir); err != nil {
			slog.Error("save settings", "err", err)
		}
	}
}
