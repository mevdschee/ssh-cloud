//go:build windows

package ui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mevdschee/ssh-cloud/internal/app"
	"github.com/mevdschee/ssh-cloud/internal/remote"
	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
	"github.com/rodrigocfd/windigo/win"
)

// accelerators are the keyboard shortcuts of the main window.
func accelerators() win.HACCEL {
	v := co.ACCELF_VIRTKEY
	ctrl, shift, alt := co.ACCELF_CONTROL, co.ACCELF_SHIFT, co.ACCELF_ALT
	h, err := win.CreateAcceleratorTable(
		win.ACCEL{FVirt: v | ctrl, Key: co.VK('X'), Cmd: cmdCut},
		win.ACCEL{FVirt: v | ctrl, Key: co.VK('C'), Cmd: cmdCopy},
		win.ACCEL{FVirt: v | ctrl, Key: co.VK('V'), Cmd: cmdPaste},
		win.ACCEL{FVirt: v | ctrl, Key: co.VK('A'), Cmd: cmdSelectAll},
		win.ACCEL{FVirt: v | ctrl | shift, Key: co.VK('N'), Cmd: cmdNewFolder},
		win.ACCEL{FVirt: v | ctrl, Key: co.VK('L'), Cmd: cmdAddress},
		win.ACCEL{FVirt: v | alt, Key: co.VK('D'), Cmd: cmdAddress},
		win.ACCEL{FVirt: v, Key: co.VK_F5, Cmd: cmdRefresh},
		win.ACCEL{FVirt: v, Key: co.VK_F2, Cmd: cmdRename},
		win.ACCEL{FVirt: v, Key: co.VK_DELETE, Cmd: cmdDelete},
		win.ACCEL{FVirt: v | alt, Key: co.VK_LEFT, Cmd: cmdBack},
		win.ACCEL{FVirt: v | alt, Key: co.VK_RIGHT, Cmd: cmdForward},
		win.ACCEL{FVirt: v | alt, Key: co.VK_UP, Cmd: cmdUp},
	)
	if err != nil {
		slog.Error("accelerator table", "err", err)
		return 0
	}
	return h
}

// editFocus returns the edit control that has the keyboard focus (the
// address bar or a list label being renamed), or 0. Shortcuts like Ctrl+C
// are then forwarded to it instead of acting on files.
func (me *Explorer) editFocus() win.HWND {
	focus := win.GetFocus()
	if focus == me.address.Hwnd() {
		return focus
	}
	if h, _ := me.list.Hwnd().SendMessage(co.LVM_GETEDITCONTROL, 0, 0); h != 0 && win.HWND(h) == focus {
		return focus
	}
	return 0
}

func (me *Explorer) listHasFocus() bool { return win.GetFocus() == me.list.Hwnd() }

func (me *Explorer) commandEvents() {
	on := me.wnd.On().WmCommandAccelMenu
	editOr := func(msg co.WM, wp win.WPARAM, lp win.LPARAM, fn func()) func() {
		return func() {
			if h := me.editFocus(); h != 0 {
				h.SendMessage(msg, wp, lp)
				return
			}
			fn()
		}
	}
	on(cmdCut, editOr(co.WM_CUT, 0, 0, func() { me.toClipboard(true) }))
	on(cmdCopy, editOr(co.WM_COPY, 0, 0, func() { me.toClipboard(false) }))
	on(cmdPaste, editOr(co.WM_PASTE, 0, 0, me.paste))
	on(cmdSelectAll, editOr(co.EM_SETSEL, 0, win.LPARAM(^uintptr(0)), func() {
		me.list.SelectAllItems(true)
		me.list.Focus()
	}))
	on(cmdDelete, editOr(co.WM_KEYDOWN, win.WPARAM(co.VK_DELETE), 0, func() {
		if me.listHasFocus() {
			me.delete()
		}
	}))
	on(cmdRename, func() {
		if me.editFocus() == 0 && me.listHasFocus() {
			me.startRename()
		}
	})
	on(cmdNewFolder, me.newFolder)
	on(cmdRefresh, me.refresh)
	on(cmdBack, me.goBack)
	on(cmdForward, me.goForward)
	on(cmdUp, me.goUp)
	on(cmdAddress, func() {
		me.address.Focus()
		me.address.SetSelection(0, -1)
	})

	// The dialog manager turns Enter and Esc into IDOK and IDCANCEL. While a
	// list label is being renamed they belong to that edit: Enter commits,
	// Esc cancels.
	on(uint16(co.ID_OK), func() {
		if h, _ := me.list.Hwnd().SendMessage(co.LVM_GETEDITCONTROL, 0, 0); h != 0 {
			win.HWND(h).SendMessage(co.WM_KEYDOWN, win.WPARAM(co.VK_RETURN), 0)
			return
		}
		if win.GetFocus() == me.address.Hwnd() {
			me.navigate(me.address.Text(), true)
		} else {
			me.open()
		}
	})
	on(uint16(co.ID_CANCEL), func() {
		if h, _ := me.list.Hwnd().SendMessage(co.LVM_GETEDITCONTROL, 0, 0); h != 0 {
			win.HWND(h).SendMessage(co.WM_KEYDOWN, win.WPARAM(co.VK_ESCAPE), 0)
		}
	})
}

func copyMenuText(canCopy bool) string {
	if canCopy {
		return "&Copy\tCtrl+C"
	}
	return "&Copy (not supported by this server)"
}

// toClipboard remembers the selection for a later paste (an internal
// clipboard; cloud items are not put on the Windows clipboard).
func (me *Explorer) toClipboard(cut bool) {
	sel := me.selected()
	if len(sel) == 0 || !me.isConnected() {
		return
	}
	if !cut && !me.canCopy {
		ui.MsgWarn(me.wnd, "Copy", "Server-side copy is not available",
			"This server does not support copying files on the server (it needs OpenSSH 9.0 or newer). "+
				"Download the files and upload them again instead.")
		return
	}
	me.clip, me.clipCut = sel, cut
	verb := "Copied"
	if cut {
		verb = "Cut"
	}
	me.setStatus(fmt.Sprintf("%s %d item(s); paste with Ctrl+V", verb, len(sel)))
}

// paste moves (after cut) or copies (after copy) the clipboard items into
// the current folder.
func (me *Explorer) paste() {
	if len(me.clip) == 0 || !me.isConnected() || me.busy {
		return
	}
	items, dir := me.clip, me.dir
	if me.clipCut {
		slog.Info("paste move", "items", len(items), "into", dir)
		me.run("Moving...", func(ctx context.Context, a *app.App) error {
			var errs []error
			for _, e := range items {
				if remote.Dir(e.Path) == dir {
					continue // already here
				}
				if err := a.Move(ctx, e.Path, remote.Join(dir, e.Name)); err != nil {
					errs = append(errs, err)
				}
			}
			return errors.Join(errs...)
		}, func(err error) {
			if err != nil {
				ui.MsgError(me.wnd, "Move", "Some items could not be moved", err.Error())
			} else {
				me.clip = nil // a cut is pasted once
			}
			me.refreshAll()
		})
		return
	}
	slog.Info("paste copy", "items", len(items), "into", dir)
	var summary app.Summary
	me.runWithProgress("Copying", func(ctx context.Context, a *app.App, progress app.Progress) error {
		summary = a.Copy(ctx, items, dir, progress)
		return errors.Join(summary.Errors...)
	}, func(err error) {
		me.setStatus("Copy: " + summary.String())
		if err != nil {
			ui.MsgError(me.wnd, "Copy", "Some items could not be copied", err.Error())
		}
		me.refreshAll()
	})
}

// refreshAll reloads the current folder; the tree node of the current
// folder is updated from the new listing.
func (me *Explorer) refreshAll() { me.refresh() }
