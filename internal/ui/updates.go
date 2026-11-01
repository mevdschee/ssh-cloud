//go:build windows

package ui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/mevdschee/ssh-cloud/internal/app"
	"github.com/mevdschee/ssh-cloud/internal/logging"
	"github.com/mevdschee/ssh-cloud/internal/update"
	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
)

// checkForUpdates runs when the user clicks Update: it asks GitHub for the
// latest release and offers to install it when it is newer. Nothing is
// checked automatically.
func (me *Explorer) checkForUpdates() {
	if me.busy {
		return
	}
	if !update.IsRelease(me.version) {
		ui.MsgOk(me.wnd, "Update", "Development build",
			"This is a development build ("+me.version+"). Updates are only for installed releases.")
		return
	}
	me.busy = true
	me.setStatus("Checking for updates...")
	me.updateButtons()
	go func() {
		var r update.Release
		var ok bool
		err := errors.New("internal error, see the log")
		func() {
			defer logging.Recover("update check")
			r, ok, err = update.Latest(me.ctx, me.version)
		}()
		me.wnd.UiThread(func() {
			me.busy = false
			me.setStatus("Ready")
			me.updateButtons()
			switch {
			case err != nil:
				slog.Error("update check failed", "err", err)
				ui.MsgError(me.wnd, "Update", "Could not check for updates", err.Error())
			case !ok || !update.Newer(r.Version, me.version):
				slog.Info("no update", "latest", r.Version, "current", me.version)
				ui.MsgOk(me.wnd, "Update", "SSH-Cloud is up to date", "You have the latest version ("+me.version+").")
			default:
				slog.Info("update available", "latest", r.Version, "current", me.version)
				me.offerUpdate(r)
			}
		})
	}()
}

func (me *Explorer) offerUpdate(r update.Release) {
	if !ui.MsgOkCancel(me.wnd, "Update", "SSH-Cloud "+r.Version+" is available",
		fmt.Sprintf("You have %s. Download and install %s now? SSH-Cloud closes during the installation.",
			me.version, r.Version), "Install") {
		return
	}
	me.busy = true
	me.updateButtons()
	var path string
	err := me.progressDialog("Downloading update", func(ctx context.Context, progress app.Progress) error {
		var err error
		path, err = update.Download(ctx, r, os.TempDir(), func(done, total int64) {
			progress(app.ProgressInfo{Action: "Downloading", File: r.MsiName, FileCount: 1, Bytes: done, TotalBytes: total})
		})
		return err
	})
	me.busy = false
	me.updateButtons()
	if errors.Is(err, context.Canceled) {
		return
	}
	if err == nil {
		// msiexec shows the installer; the MSI replaces the running version,
		// so close to release ssh-cloud.exe.
		log := filepath.Join(os.TempDir(), "ssh-cloud-update.log")
		err = exec.Command("msiexec", "/i", path, "/l*v", log).Start()
	}
	if err != nil {
		slog.Error("update failed", "err", err)
		ui.MsgError(me.wnd, "Update", "Could not install the update", err.Error())
		return
	}
	slog.Info("installing update, closing", "msi", path)
	me.wnd.Hwnd().PostMessage(co.WM_CLOSE, 0, 0)
}
