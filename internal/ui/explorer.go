//go:build windows

// Package ui is the windigo (Win32) user interface of SSH-Cloud: an
// Explorer-like window on top of package app.
package ui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"strings"
	"sync"
	"unsafe"

	"github.com/mevdschee/ssh-cloud/internal/app"
	"github.com/mevdschee/ssh-cloud/internal/config"
	"github.com/mevdschee/ssh-cloud/internal/logging"
	"github.com/mevdschee/ssh-cloud/internal/remote"
	"github.com/mevdschee/ssh-cloud/internal/transfer"
	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
	"github.com/rodrigocfd/windigo/win"
	"github.com/rodrigocfd/windigo/wstr"
	"golang.org/x/crypto/ssh"
)

// Context menu command ids.
const (
	cmdOpen uint16 = 2001 + iota
	cmdDownload
	cmdRename
	cmdDelete
	cmdCopyPath
	cmdNewFolder
	cmdUpload
	cmdRefresh
)

// Explorer is the main window.
type Explorer struct {
	wnd                        *ui.Main
	btnBack, btnForward, btnUp *ui.Button
	address                    *ui.Edit
	btnGo                      *ui.Button
	btnUpload, btnDownload     *ui.Button
	btnNewFolder, btnRename    *ui.Button
	btnDelete, btnRefresh      *ui.Button
	btnTransfers, btnStop      *ui.Button
	btnConnect                 *ui.Button
	list                       *ui.ListView
	status                     *ui.StatusBar

	cfgDir string
	cfg    *config.Config

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	app      *app.App
	profile  config.Profile
	busy     bool
	opCancel context.CancelFunc // cancels the running operation

	dragging []remote.Entry // entries being dragged within the list
	dropIdx  int            // highlighted drop target, or -1

	dir     string   // current namespace
	back    []string // navigation history
	forward []string
	entries []remote.Entry
}

// Run shows the explorer and blocks until it is closed.
func Run(cfgDir string, cfg *config.Config) int {
	me := &Explorer{cfgDir: cfgDir, cfg: cfg}
	me.ctx, me.cancel = context.WithCancel(context.Background())
	me.build()
	me.events()
	return me.wnd.RunAsMain()
}

func (me *Explorer) build() {
	me.wnd = ui.NewMain(ui.OptsMain().
		Title("SSH-Cloud").
		Size(ui.Dpi(860, 560)).
		Style(co.WS_CAPTION | co.WS_SYSMENU | co.WS_CLIPCHILDREN | co.WS_BORDER | co.WS_VISIBLE |
								co.WS_MINIMIZEBOX | co.WS_MAXIMIZEBOX | co.WS_SIZEBOX).
		ExStyle(co.WS_EX_LEFT | co.WS_EX_ACCEPTFILES). // files dropped from Windows Explorer
		Center(true))

	small := func(text string, x int) *ui.Button {
		return ui.NewButton(me.wnd, ui.OptsButton().Text(text).Position(ui.Dpi(x, 8)).Width(ui.DpiX(32)))
	}
	me.btnBack = small("<", 8)
	me.btnForward = small(">", 42)
	me.btnUp = small("^", 76)
	me.address = ui.NewEdit(me.wnd, ui.OptsEdit().Position(ui.Dpi(116, 9)).Width(ui.DpiX(610)).
		Layout(ui.LAY_RESIZE_HOLD))
	me.btnGo = ui.NewButton(me.wnd, ui.OptsButton().Text("Go").Position(ui.Dpi(732, 8)).Width(ui.DpiX(50)).
		Layout(ui.LAY_MOVE_HOLD))
	me.btnRefresh = ui.NewButton(me.wnd, ui.OptsButton().Text("Refresh").Position(ui.Dpi(786, 8)).Width(ui.DpiX(66)).
		Layout(ui.LAY_MOVE_HOLD))

	action := func(text string, x, w int) *ui.Button {
		return ui.NewButton(me.wnd, ui.OptsButton().Text(text).Position(ui.Dpi(x, 40)).Width(ui.DpiX(w)))
	}
	me.btnUpload = action("&Upload...", 8, 90)
	me.btnDownload = action("&Download to...", 102, 110)
	me.btnNewFolder = action("&New folder", 216, 90)
	me.btnRename = action("Rena&me", 310, 80)
	me.btnDelete = action("De&lete", 394, 80)
	me.btnTransfers = action("&Transfers", 478, 84)
	me.btnStop = action("&Stop", 566, 66)
	me.btnConnect = ui.NewButton(me.wnd, ui.OptsButton().Text("C&onnections...").Position(ui.Dpi(752, 40)).
		Width(ui.DpiX(100)).Layout(ui.LAY_MOVE_HOLD))

	me.list = ui.NewListView(me.wnd, ui.OptsListView().
		Position(ui.Dpi(8, 74)).
		Size(ui.Dpi(844, 452)).
		Layout(ui.LAY_RESIZE_RESIZE).
		CtrlStyle(co.LVS_REPORT|co.LVS_NOSORTHEADER|co.LVS_SHOWSELALWAYS|co.LVS_EDITLABELS).
		CtrlExStyle(co.LVS_EX_FULLROWSELECT|co.LVS_EX_DOUBLEBUFFER).
		Column("Name", ui.DpiX(360)).
		Column("Date modified", ui.DpiX(140)).
		Column("Type", ui.DpiX(160)).
		Column("Size", ui.DpiX(100)))

	me.status = ui.NewStatusBar(me.wnd, ui.OptsStatusBar().
		FlexPart(1, "Not connected").
		FixedPart(ui.DpiX(160), ""))
}

func (me *Explorer) events() {
	me.wnd.On().WmCreate(func(_ ui.WmCreate) int {
		me.list.Col(3).SetJustification(co.HDF_RIGHT)
		me.updateButtons()
		me.wnd.UiThread(me.startup) // after the window is shown
		return 0
	})
	me.wnd.On().WmDestroy(func() {
		me.cancel()
		me.mu.Lock()
		if me.app != nil {
			me.app.Close()
		}
		me.mu.Unlock()
	})

	me.btnBack.On().BnClicked(me.goBack)
	me.btnForward.On().BnClicked(me.goForward)
	me.btnUp.On().BnClicked(me.goUp)
	me.btnGo.On().BnClicked(func() { me.navigate(me.address.Text(), true) })
	me.btnRefresh.On().BnClicked(me.refresh)
	me.btnUpload.On().BnClicked(me.upload)
	me.btnDownload.On().BnClicked(me.download)
	me.btnNewFolder.On().BnClicked(me.newFolder)
	me.btnRename.On().BnClicked(me.startRename)
	me.btnDelete.On().BnClicked(me.delete)
	me.btnConnect.On().BnClicked(me.promptConnect)
	me.btnTransfers.On().BnClicked(me.showTransfers)
	me.btnStop.On().BnClicked(func() {
		if me.opCancel != nil {
			slog.Info("operation stopped by user")
			me.opCancel()
			me.setStatus("Stopping...")
		}
	})

	me.wnd.On().WmDropFiles(func(p ui.WmDropFiles) {
		files, _ := p.HDrop().DragQueryFile()
		pt, _ := p.HDrop().DragQueryPoint()
		p.HDrop().DragFinish()
		me.dropFiles(files, pt)
	})
	me.dragEvents()

	// Enter: the dialog manager sends IDOK. Navigate from the address bar,
	// open the selection from the list.
	me.wnd.On().WmCommandAccelMenu(uint16(co.ID_OK), func() {
		if focus := win.GetFocus(); focus == me.address.Hwnd() {
			me.navigate(me.address.Text(), true)
		} else {
			me.open()
		}
	})

	me.list.On().LvnItemActivate(func(_ *win.NMITEMACTIVATE) { me.open() })
	me.list.On().LvnItemChanged(func(_ *win.NMLISTVIEW) { me.updateButtons() })
	me.list.On().LvnKeyDown(func(p *win.NMLVKEYDOWN) {
		ctrl, _ := win.GetKeyState(co.VK_CONTROL)
		switch {
		case p.WVKey == co.VK_BACK:
			me.goUp()
		case p.WVKey == co.VK_F2:
			me.startRename()
		case p.WVKey == co.VK_DELETE:
			me.delete()
		case p.WVKey == co.VK_F5:
			me.refresh()
		case ctrl && p.WVKey == co.VK('A'):
			me.list.SelectAllItems(true)
		}
	})
	me.list.On().NmRClick(func(_ *win.NMITEMACTIVATE) { me.contextMenu() })
	me.list.On().LvnBeginLabelEdit(func(_ *win.NMLVDISPINFO) bool {
		return !me.isConnected() // returning true cancels the edit
	})
	me.list.On().LvnEndLabelEdit(func(p *win.NMLVDISPINFO) bool {
		name, ok := labelText(p)
		if !ok {
			return false
		}
		item := me.list.Item(int(p.Item.IItem))
		if e, ok := item.Data().(remote.Entry); ok && name != e.Name {
			me.rename(e, name)
		}
		return false // the list is refreshed after the rename
	})
}

// labelText extracts the edited label; ok is false when editing was cancelled.
func labelText(p *win.NMLVDISPINFO) (name string, ok bool) {
	defer func() {
		if recover() != nil {
			name, ok = "", false
		}
	}()
	buf := p.Item.PszText()
	if buf == nil {
		return "", false
	}
	name = strings.TrimSpace(wstr.DecodeSlice(buf))
	return name, name != ""
}

func (me *Explorer) isConnected() bool {
	me.mu.Lock()
	defer me.mu.Unlock()
	return me.app != nil
}

func (me *Explorer) setStatus(text string) { setPartText(me.status.Part(0), text) }

// setPartText sets a status bar part. Unchanged text is skipped: some
// implementations (Wine) report SB_SETTEXT failure for it, which windigo turns
// into a panic. A status bar update is never worth crashing for.
func setPartText(part ui.StatusBarPart, text string) {
	defer func() { _ = recover() }()
	if part.Text() != text {
		part.SetText(text)
	}
}

func (me *Explorer) updateButtons() {
	connected := me.isConnected()
	sel := me.list.SelectedItemCount()
	enable := func(b *ui.Button, on bool) { b.Hwnd().EnableWindow(on && !me.busy) }
	enable(me.btnBack, connected && len(me.back) > 0)
	enable(me.btnForward, connected && len(me.forward) > 0)
	enable(me.btnUp, connected && me.dir != "")
	enable(me.btnGo, connected)
	enable(me.btnRefresh, connected)
	enable(me.btnUpload, connected)
	enable(me.btnDownload, connected && sel > 0)
	enable(me.btnNewFolder, connected)
	enable(me.btnRename, connected && sel == 1)
	enable(me.btnDelete, connected && sel > 0)
	me.btnConnect.Hwnd().EnableWindow(!me.busy)
	enable(me.btnTransfers, connected)
	me.btnStop.Hwnd().EnableWindow(me.busy && me.opCancel != nil)
	if !connected {
		setPartText(me.status.Part(1), "")
	} else if sel > 0 {
		setPartText(me.status.Part(1), fmt.Sprintf("%d of %d selected", sel, len(me.entries)))
	} else {
		setPartText(me.status.Part(1), fmt.Sprintf("%d items", len(me.entries)))
	}
}

// run executes work on a goroutine. done runs on the UI thread afterwards.
func (me *Explorer) run(status string, work func(ctx context.Context, a *app.App) error, done func(err error)) {
	me.mu.Lock()
	a := me.app
	me.mu.Unlock()
	if a == nil || me.busy {
		return
	}
	ctx, cancel := context.WithCancel(me.ctx)
	me.busy, me.opCancel = true, cancel
	me.setStatus(status)
	me.updateButtons()
	slog.Debug("operation start", "op", status)
	go func() {
		defer cancel()
		err := errors.New("internal error, see the log")
		func() {
			defer logging.Recover(status)
			err = work(ctx, a)
		}()
		if err != nil {
			slog.Error("operation failed", "op", status, "err", err)
		}
		me.wnd.UiThread(func() {
			me.busy, me.opCancel = false, nil
			if done != nil {
				done(err)
			}
			me.updateButtons()
		})
	}()
}

// onUI runs fn on the UI thread and waits for it; used by callbacks that must
// ask the user from a worker goroutine.
func (me *Explorer) onUI(fn func()) {
	done := make(chan struct{})
	me.wnd.UiThread(func() {
		fn()
		close(done)
	})
	<-done
}

// --- connection ---

// startup connects to the last used connection, or opens the manager.
func (me *Explorer) startup() {
	if p, ok := me.cfg.Last(); ok {
		me.connect(p, true)
		return
	}
	me.promptConnect()
}

// promptConnect opens the connection manager (or, with no saved connections
// yet, the form for a new one) and connects to the chosen connection.
func (me *Explorer) promptConnect() {
	if me.busy {
		return
	}
	if len(me.cfg.Profiles) == 0 {
		p, ok := editProfile(me.wnd, "New connection", config.Profile{UseAgent: true})
		if !ok {
			return
		}
		p = me.cfg.Put(p)
		if err := me.cfg.Save(me.cfgDir); err != nil {
			slog.Error("save settings", "err", err)
		}
		me.connect(p, false)
		return
	}
	if p, ok := showConnections(me.wnd, me.cfg, me.cfgDir); ok {
		me.connect(p, false)
	}
}

// connect opens a connection. When automatic (at startup) a failure opens
// the connection manager after the error.
func (me *Explorer) connect(p config.Profile, automatic bool) {
	rc := p.RemoteConfig()
	rc.TrustHost = func(host string, key ssh.PublicKey) bool {
		trusted := false
		me.onUI(func() {
			trusted = ui.MsgOkCancel(me.wnd, "Unknown server", "Trust this server?",
				fmt.Sprintf("The server %s is not known yet.\n\n%s key fingerprint:\n%s\n\nOnly continue if this matches the server's real key.",
					host, key.Type(), ssh.FingerprintSHA256(key)), "Trust")
		})
		return trusted
	}
	rc.Passphrase = func(keyFile string) ([]byte, error) {
		var pass string
		var ok bool
		me.onUI(func() {
			pass, ok = showInput(me.wnd, "Passphrase", "Passphrase for "+keyFile, "", true)
		})
		if !ok {
			return nil, errors.New("cancelled")
		}
		return []byte(pass), nil
	}

	me.busy = true
	me.setStatus("Connecting to " + p.DisplayName() + "...")
	me.updateButtons()
	slog.Info("connect", "profile", p.DisplayName(), "host", p.Host, "port", p.Port, "user", p.User, "root", p.Root,
		"keyFile", p.KeyFile, "agent", p.UseAgent, "knownHosts", rc.KnownHostsFile, "transferLog", p.TransferLogFile(me.cfgDir))
	go func() {
		var a *app.App
		err := errors.New("internal error, see the log")
		func() {
			defer logging.Recover("connect")
			a, err = app.Open(me.ctx, rc, p.TransferLogFile(me.cfgDir))
		}()
		me.wnd.UiThread(func() {
			me.busy = false
			if err != nil {
				me.setStatus("Not connected")
				me.updateButtons()
				ui.MsgError(me.wnd, "Connect", "Could not connect to "+p.DisplayName(), err.Error())
				if automatic {
					me.promptConnect()
				}
				return
			}
			me.mu.Lock()
			old := me.app
			me.app, me.profile = a, p
			me.mu.Unlock()
			if old != nil {
				old.Close()
			}
			me.cfg.SetLast(p.ID)
			if err := me.cfg.Save(me.cfgDir); err != nil {
				ui.MsgWarn(me.wnd, "Settings", "Could not save settings", err.Error())
			}
			me.wnd.Hwnd().SetWindowText("SSH-Cloud - " + p.DisplayName())
			me.dir, me.back, me.forward = "", nil, nil
			me.navigate("", false)
		})
	}()
}

// --- navigation ---

func (me *Explorer) navigate(dir string, remember bool) {
	dir = remote.Clean(dir)
	var entries []remote.Entry
	me.run("Loading /"+dir+"...", func(ctx context.Context, a *app.App) error {
		var err error
		entries, err = a.List(ctx, dir)
		return err
	}, func(err error) {
		if err != nil {
			me.address.SetText("/" + me.dir)
			me.setStatus("Ready")
			ui.MsgError(me.wnd, "Open folder", "Could not open /"+dir, err.Error())
			return
		}
		slog.Debug("listed", "dir", dir, "entries", len(entries))
		if remember && dir != me.dir {
			me.back = append(me.back, me.dir)
			me.forward = nil
		}
		me.dir = dir
		me.show(entries)
		me.setStatus("Ready")
	})
}

func (me *Explorer) refresh() { me.navigate(me.dir, false) }

func (me *Explorer) goUp() {
	if me.dir != "" {
		me.navigate(remote.Dir(me.dir), true)
	}
}

func (me *Explorer) goBack() {
	if n := len(me.back); n > 0 {
		prev := me.back[n-1]
		me.back = me.back[:n-1]
		me.forward = append(me.forward, me.dir)
		me.navigate(prev, false)
	}
}

func (me *Explorer) goForward() {
	if n := len(me.forward); n > 0 {
		next := me.forward[n-1]
		me.forward = me.forward[:n-1]
		me.back = append(me.back, me.dir)
		me.navigate(next, false)
	}
}

// show fills the list.
func (me *Explorer) show(entries []remote.Entry) {
	me.entries = entries
	me.address.SetText("/" + me.dir)
	me.list.SetRedraw(false)
	me.list.DeleteAllItems()
	for _, e := range entries {
		modified, size := "", ""
		if !e.ModTime.IsZero() {
			modified = e.ModTime.Local().Format("2006-01-02 15:04")
		}
		if !e.IsDir {
			size = humanSize(e.Size)
		}
		item := me.list.AddItem(e.Name, modified, typeName(e), size)
		item.SetData(e)
	}
	me.list.SetRedraw(true)
	me.updateButtons()
}

func typeName(e remote.Entry) string {
	if e.IsDir {
		return "File folder"
	}
	ext := strings.TrimPrefix(path.Ext(e.Name), ".")
	if ext == "" || ext == e.Name[1:] {
		return "File"
	}
	return strings.ToUpper(ext) + " File"
}

func (me *Explorer) selected() []remote.Entry {
	var sel []remote.Entry
	for _, item := range me.list.SelectedItems() {
		if e, ok := item.Data().(remote.Entry); ok {
			sel = append(sel, e)
		}
	}
	return sel
}

// open enters a folder, or downloads a file.
func (me *Explorer) open() {
	sel := me.selected()
	if len(sel) == 1 && sel[0].IsDir {
		me.navigate(sel[0].Path, true)
		return
	}
	if len(sel) > 0 {
		me.download()
	}
}

func (me *Explorer) contextMenu() {
	if !me.isConnected() || me.busy {
		return
	}
	sel := me.selected()
	n := len(sel)
	openText := "&Open"
	if n == 1 && !sel[0].IsDir {
		openText = "&Download to..."
	}
	items := []menuItem{
		{id: cmdOpen, text: openText, enabled: n == 1, bold: true},
		{id: cmdDownload, text: "Download &to...", enabled: n > 0},
		{},
		{id: cmdRename, text: "Rena&me", enabled: n == 1},
		{id: cmdDelete, text: "&Delete", enabled: n > 0},
		{id: cmdCopyPath, text: "Copy &path", enabled: n > 0},
		{},
		{id: cmdNewFolder, text: "&New folder", enabled: true},
		{id: cmdUpload, text: "&Upload...", enabled: true},
		{id: cmdRefresh, text: "Re&fresh", enabled: true},
	}
	switch popupMenu(me.wnd.Hwnd(), items) {
	case cmdOpen:
		me.open()
	case cmdDownload:
		me.download()
	case cmdRename:
		me.startRename()
	case cmdDelete:
		me.delete()
	case cmdCopyPath:
		var paths []string
		for _, e := range sel {
			paths = append(paths, "/"+e.Path)
		}
		copyText(me.wnd.Hwnd(), strings.Join(paths, "\r\n"))
	case cmdNewFolder:
		me.newFolder()
	case cmdUpload:
		me.upload()
	case cmdRefresh:
		me.refresh()
	}
}

// --- operations ---

func (me *Explorer) startRename() {
	items := me.list.SelectedItems()
	if len(items) != 1 || me.busy {
		return
	}
	me.list.Focus()
	me.list.Hwnd().SendMessage(co.LVM_EDITLABEL, win.WPARAM(items[0].Index()), 0)
}

func (me *Explorer) rename(e remote.Entry, name string) {
	me.run("Renaming "+e.Name+"...", func(ctx context.Context, a *app.App) error {
		_, err := a.Rename(ctx, e.Path, name)
		return err
	}, func(err error) {
		if err != nil {
			ui.MsgError(me.wnd, "Rename", "Could not rename "+e.Name, err.Error())
		}
		me.refresh()
	})
}

func (me *Explorer) newFolder() {
	if !me.isConnected() || me.busy {
		return
	}
	name, ok := showInput(me.wnd, "New folder", "Folder name (it is created when you upload a file into it)", "New folder", false)
	if !ok || strings.TrimSpace(name) == "" {
		return
	}
	me.run("Creating folder...", func(ctx context.Context, a *app.App) error {
		_, err := a.NewFolder(ctx, me.dir, strings.TrimSpace(name))
		return err
	}, func(err error) {
		if err != nil {
			ui.MsgError(me.wnd, "New folder", "Could not create the folder", err.Error())
		}
		me.refresh()
	})
}

func (me *Explorer) delete() {
	sel := me.selected()
	if len(sel) == 0 || me.busy {
		return
	}
	what := "\"" + sel[0].Name + "\""
	if sel[0].IsDir {
		what = "the folder " + what + " and everything in it"
	}
	if len(sel) > 1 {
		what = fmt.Sprintf("these %d items (folders with everything in them)", len(sel))
	}
	if !ui.MsgOkCancel(me.wnd, "Delete", "Delete from the server?",
		"Permanently delete "+what+"? This cannot be undone.", "Delete") {
		return
	}
	var paths []string
	for _, e := range sel {
		paths = append(paths, e.Path)
	}
	me.run("Deleting...", func(ctx context.Context, a *app.App) error {
		return a.Delete(ctx, paths)
	}, func(err error) {
		if err != nil {
			ui.MsgError(me.wnd, "Delete", "Some items could not be deleted", err.Error())
		}
		me.refresh()
	})
}

func (me *Explorer) upload() {
	if !me.isConnected() || me.busy {
		return
	}
	me.uploadFiles(pickFiles(me.wnd.Hwnd(), "Upload to /"+me.dir), me.dir)
}

// uploadFiles uploads local files and folders into a cloud folder.
func (me *Explorer) uploadFiles(files []string, dir string) {
	if len(files) == 0 {
		return
	}
	slog.Info("upload requested", "files", len(files), "to", dir)
	var summary app.Summary
	me.run("Uploading...", func(ctx context.Context, a *app.App) error {
		summary = a.Upload(ctx, files, dir, me.decide, me.progress)
		return errors.Join(summary.Errors...)
	}, func(err error) {
		me.setStatus("Upload: " + summary.String())
		if err != nil {
			ui.MsgError(me.wnd, "Upload", "Some files could not be uploaded", err.Error())
		}
		me.refresh()
	})
}

func (me *Explorer) download() {
	sel := me.selected()
	if len(sel) == 0 || me.busy {
		return
	}
	start := me.cfg.DownloadFolder
	if start == "" {
		if home, err := os.UserHomeDir(); err == nil {
			start = home
		}
	}
	dst, ok := pickFolder(me.wnd.Hwnd(), "Download to", start)
	if !ok {
		return
	}
	me.cfg.DownloadFolder = dst
	_ = me.cfg.Save(me.cfgDir)

	var paths []string
	for _, e := range sel {
		paths = append(paths, e.Path)
	}
	var summary app.Summary
	me.run("Downloading...", func(ctx context.Context, a *app.App) error {
		summary = a.Download(ctx, paths, dst, me.overwriteLocal, me.progress)
		return errors.Join(summary.Errors...)
	}, func(err error) {
		me.setStatus("Download: " + summary.String())
		if err != nil {
			ui.MsgError(me.wnd, "Download", "Some files could not be downloaded", err.Error())
		}
	})
}

// --- callbacks from worker goroutines ---

func (me *Explorer) progress(msg string) {
	me.wnd.UiThread(func() { me.setStatus(msg) })
}

func (me *Explorer) decide(c *transfer.ConflictError, localPath string) (app.Choice, bool) {
	var choice app.Choice
	var all bool
	me.onUI(func() { choice, all = showConflict(me.wnd, c, localPath) })
	slog.Info("conflict decision", "remote", c.Path, "reason", c.Reason, "local", localPath, "choice", choice, "applyAll", all)
	return choice, all
}

func (me *Explorer) overwriteLocal(localPath string) (bool, bool) {
	var ok bool
	me.onUI(func() {
		ok = ui.MsgOkCancel(me.wnd, "Download", "Replace local file?",
			localPath+" already exists. Replace it with the version from the server?", "Replace")
	})
	slog.Info("overwrite local decision", "local", localPath, "overwrite", ok)
	return ok, false
}

// showTransfers opens the transfer list of the current connection.
func (me *Explorer) showTransfers() {
	me.mu.Lock()
	a, p := me.app, me.profile
	me.mu.Unlock()
	if a == nil {
		return
	}
	showTransfers(me.wnd, "Transfers - "+p.DisplayName(), a.Log().Entries())
}

// --- drag and drop ---

// dropFiles handles files dropped from Windows Explorer: onto a folder in
// the list they go into that folder, anywhere else into the current folder.
func (me *Explorer) dropFiles(files []string, ptMain win.POINT) {
	if !me.isConnected() || len(files) == 0 {
		return
	}
	if me.busy {
		ui.MsgWarn(me.wnd, "Upload", "Busy", "Wait for the current operation to finish, then drop the files again.")
		return
	}
	target := me.dir
	pt := ptMain
	me.wnd.Hwnd().ClientToScreenPt(&pt)
	me.list.Hwnd().ScreenToClientPt(&pt)
	if item, ok := me.list.HitTest(pt); ok {
		if e, ok := item.Data().(remote.Entry); ok && e.IsDir {
			target = e.Path
		}
	}
	me.uploadFiles(files, target)
}

// dragEvents implements dragging items onto a folder in the list (a move on
// the server). The list captures the mouse while dragging.
func (me *Explorer) dragEvents() {
	me.dropIdx = -1
	me.list.On().LvnBeginDrag(func(_ *win.NMLISTVIEW) {
		if me.busy || !me.isConnected() {
			return
		}
		me.dragging = me.selected()
		if len(me.dragging) > 0 {
			me.list.Hwnd().SetCapture()
		}
	})
	sub := me.list.OnSubclass()
	sub.WmMouseMove(func(p ui.WmMouse) {
		if me.dragging == nil {
			return
		}
		idx := -1
		if _, ok := me.dropTarget(p.Pos()); ok {
			item, _ := me.list.HitTest(p.Pos())
			idx = item.Index()
		}
		me.highlight(idx)
	})
	sub.WmLButtonUp(func(p ui.WmMouse) {
		if me.dragging == nil {
			return
		}
		dragged := me.dragging
		target, ok := me.dropTarget(p.Pos())
		me.endDrag()
		if ok {
			me.moveInto(dragged, target)
		}
	})
	sub.WmCaptureChanged(func(_ ui.WmCaptureChanged) { me.endDrag() })
}

// dropTarget returns the folder under a point in list coordinates, if the
// dragged items may be dropped there.
func (me *Explorer) dropTarget(pt win.POINT) (remote.Entry, bool) {
	item, ok := me.list.HitTest(pt)
	if !ok {
		return remote.Entry{}, false
	}
	target, ok := item.Data().(remote.Entry)
	if !ok || !target.IsDir {
		return remote.Entry{}, false
	}
	for _, e := range me.dragging {
		if e.Path == target.Path || strings.HasPrefix(target.Path, e.Path+"/") {
			return remote.Entry{}, false // onto itself or into its own subfolder
		}
	}
	return target, true
}

func (me *Explorer) endDrag() {
	if me.dragging == nil {
		return
	}
	me.dragging = nil // first, so WM_CAPTURECHANGED from the release is a no-op
	me.highlight(-1)
	_ = win.ReleaseCapture()
}

// highlight marks one item as drop target (LVIS_DROPHILITED), -1 for none.
func (me *Explorer) highlight(idx int) {
	if idx == me.dropIdx {
		return
	}
	set := func(i int, on bool) {
		lvi := win.LVITEM{StateMask: co.LVIS_DROPHILITED}
		if on {
			lvi.State = co.LVIS_DROPHILITED
		}
		me.list.Hwnd().SendMessage(co.LVM_SETITEMSTATE, win.WPARAM(i), win.LPARAM(unsafe.Pointer(&lvi)))
	}
	if me.dropIdx >= 0 {
		set(me.dropIdx, false)
	}
	if idx >= 0 {
		set(idx, true)
	}
	me.dropIdx = idx
}

// moveInto moves entries into a folder on the server.
func (me *Explorer) moveInto(entries []remote.Entry, target remote.Entry) {
	slog.Info("move requested", "items", len(entries), "into", target.Path)
	me.run("Moving into "+target.Name+"...", func(ctx context.Context, a *app.App) error {
		var errs []error
		for _, e := range entries {
			if err := a.Move(ctx, e.Path, remote.Join(target.Path, e.Name)); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}, func(err error) {
		if err != nil {
			ui.MsgError(me.wnd, "Move", "Some items could not be moved", err.Error())
		}
		me.refresh()
	})
}
