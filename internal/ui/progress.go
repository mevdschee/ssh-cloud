//go:build windows

package ui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/mevdschee/ssh-cloud/internal/app"
	"github.com/mevdschee/ssh-cloud/internal/logging"
	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
)

// runWithProgress runs a long operation (upload, download, copy, delete) on
// a goroutine and shows a progress window with a Cancel button until it
// finishes. Closing the window also cancels. done runs on the UI thread when
// the work has really finished.
func (me *Explorer) runWithProgress(title string, work func(ctx context.Context, a *app.App, progress app.Progress) error, done func(err error)) {
	me.mu.Lock()
	a := me.app
	me.mu.Unlock()
	if a == nil || me.busy {
		return
	}
	ctx, cancel := context.WithCancel(me.ctx)
	me.busy, me.opCancel = true, cancel
	me.setStatus(title + "...")
	me.updateButtons()
	slog.Debug("operation start", "op", title)

	wnd := ui.NewModal(me.wnd, ui.OptsModal().Title(title).Size(ui.Dpi(460, 158)))
	action := ui.NewStatic(wnd, ui.OptsStatic().Text("Preparing...").Position(ui.Dpi(14, 14)).Size(ui.Dpi(432, 20)))
	file := ui.NewStatic(wnd, ui.OptsStatic().Text("").Position(ui.Dpi(14, 36)).Size(ui.Dpi(432, 20)).
		CtrlStyle(co.SS_LEFT|co.SS_NOPREFIX|co.SS_PATHELLIPSIS))
	bar := ui.NewProgressBar(wnd, ui.OptsProgressBar().Position(ui.Dpi(14, 62)).Size(ui.Dpi(432, 18)))
	detail := ui.NewStatic(wnd, ui.OptsStatic().Text("").Position(ui.Dpi(14, 88)).Size(ui.Dpi(432, 20)))
	btnCancel := ui.NewButton(wnd, ui.OptsButton().Text("Cancel").Position(ui.Dpi(358, 120)).
		CtrlId(uint16(co.ID_CANCEL)))

	closed := false
	start := time.Now()
	setText := func(s *ui.Static, text string) {
		if s.Text() != text {
			s.Hwnd().SetWindowText(text)
		}
	}
	show := func(p app.ProgressInfo) {
		if closed {
			return
		}
		if p.FileCount > 0 {
			setText(action, fmt.Sprintf("%s %d of %d", p.Action, p.FileIndex+1, p.FileCount))
		} else {
			setText(action, p.Action+"...")
		}
		setText(file, "/"+p.File)
		pos := 0
		switch {
		case p.TotalBytes > 0:
			pos = int(p.Bytes * 1000 / p.TotalBytes)
		case p.FileCount > 0:
			pos = p.FileIndex * 1000 / p.FileCount
		}
		bar.SetPos(pos)
		if p.TotalBytes > 0 {
			text := fmt.Sprintf("%s of %s", humanSize(p.Bytes), humanSize(p.TotalBytes))
			if secs := time.Since(start).Seconds(); secs > 1 && p.Bytes > 0 {
				rate := float64(p.Bytes) / secs
				left := time.Duration(float64(p.TotalBytes-p.Bytes)/rate) * time.Second
				text += fmt.Sprintf("  (%s/s, %s left)", humanSize(int64(rate)), left.Round(time.Second))
			}
			setText(detail, text)
		}
	}

	// Progress arrives on the worker goroutine, possibly very often; show
	// the latest state at most every 100 ms.
	var latest atomic.Pointer[app.ProgressInfo]
	var scheduled atomic.Bool
	progress := func(p app.ProgressInfo) {
		latest.Store(&p)
		if scheduled.CompareAndSwap(false, true) {
			go func() {
				time.Sleep(100 * time.Millisecond)
				me.wnd.UiThread(func() {
					scheduled.Store(false)
					show(*latest.Load())
				})
			}()
		}
	}

	btnCancel.On().BnClicked(func() {
		slog.Info("operation cancelled by user", "op", title)
		cancel()
		setText(action, "Cancelling...")
		btnCancel.Hwnd().EnableWindow(false)
	})
	wnd.On().WmDestroy(func() {
		closed = true
		me.progressWnd = nil
		cancel() // closing the window cancels; done still waits for the work
	})
	wnd.On().WmCreate(func(_ ui.WmCreate) int {
		bar.SetRange(0, 1000)
		go func() {
			err := errors.New("internal error, see the log")
			func() {
				defer logging.Recover(title)
				err = work(ctx, a, progress)
			}()
			if err != nil {
				slog.Error("operation failed", "op", title, "err", err)
			}
			me.wnd.UiThread(func() {
				if !closed {
					closeModal(wnd)
				}
				me.progressWnd = nil
				me.busy, me.opCancel = false, nil
				cancel()
				if done != nil {
					done(err)
				}
				me.updateButtons()
			})
		}()
		return 0
	})
	me.progressWnd = wnd
	wnd.ShowModal()
}

// dialogParent is the window prompts from a worker should be modal to: the
// progress window while one is open, otherwise the main window.
func (me *Explorer) dialogParent() ui.Parent {
	if me.progressWnd != nil {
		return me.progressWnd
	}
	return me.wnd
}
