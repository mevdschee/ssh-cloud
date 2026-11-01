//go:build windows

package ui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/mevdschee/ssh-cloud/internal/app"
	"github.com/mevdschee/ssh-cloud/internal/config"
	"github.com/mevdschee/ssh-cloud/internal/remote"
	"github.com/mevdschee/ssh-cloud/internal/transfer"
	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
)

func closeModal(wnd *ui.Modal) { wnd.Hwnd().SendMessage(co.WM_CLOSE, 0, 0) }

// focusOnOpen puts the keyboard focus in an edit when the dialog opens, with
// its text selected so typing replaces it.
func focusOnOpen(wnd *ui.Modal, edit *ui.Edit, init ...func()) {
	wnd.On().WmCreate(func(_ ui.WmCreate) int {
		for _, fn := range init {
			fn()
		}
		wnd.UiThread(func() {
			edit.Focus()
			edit.SetSelection(0, -1)
		})
		return 0
	})
}

func label(parent ui.Parent, text string, x, y, cx int) *ui.Static {
	return ui.NewStatic(parent, ui.OptsStatic().Text(text).Position(ui.Dpi(x, y+3)).Size(ui.Dpi(cx, 20)))
}

func okCancel(wnd *ui.Modal, okText string, x, y int) (*ui.Button, *ui.Button) {
	ok := ui.NewButton(wnd, ui.OptsButton().Text(okText).Position(ui.Dpi(x, y)).
		CtrlId(uint16(co.ID_OK)).CtrlStyle(co.BS_DEFPUSHBUTTON))
	cancel := ui.NewButton(wnd, ui.OptsButton().Text("Cancel").Position(ui.Dpi(x+96, y)).
		CtrlId(uint16(co.ID_CANCEL)))
	cancel.On().BnClicked(func() { closeModal(wnd) })
	return ok, cancel
}

// editProfile shows the form for one connection. It returns false on cancel.
func editProfile(parent ui.Parent, title string, p config.Profile) (config.Profile, bool) {
	wnd := ui.NewModal(parent, ui.OptsModal().Title(title).Size(ui.Dpi(460, 268)))

	row := func(i int) int { return 14 + i*32 }
	label(wnd, "Name", 14, row(0), 110)
	name := ui.NewEdit(wnd, ui.OptsEdit().Text(p.Name).Position(ui.Dpi(130, row(0))).Width(ui.DpiX(310)))
	label(wnd, "Host", 14, row(1), 110)
	host := ui.NewEdit(wnd, ui.OptsEdit().Text(p.Host).Position(ui.Dpi(130, row(1))).Width(ui.DpiX(240)))
	port := strconv.Itoa(p.Port)
	if p.Port == 0 {
		port = "22"
	}
	portEdit := ui.NewEdit(wnd, ui.OptsEdit().Text(port).Position(ui.Dpi(380, row(1))).Width(ui.DpiX(60)).
		CtrlStyle(co.ES_AUTOHSCROLL|co.ES_NUMBER))
	label(wnd, "Username", 14, row(2), 110)
	user := ui.NewEdit(wnd, ui.OptsEdit().Text(p.User).Position(ui.Dpi(130, row(2))).Width(ui.DpiX(310)))
	label(wnd, "Remote folder", 14, row(3), 110)
	root := ui.NewEdit(wnd, ui.OptsEdit().Text(p.Root).Position(ui.Dpi(130, row(3))).Width(ui.DpiX(310)))
	label(wnd, "Authentication", 14, row(4), 110)
	methods := []string{remote.AuthAuto, remote.AuthKeyFile, remote.AuthPageant, remote.AuthPassword}
	selected := 0
	for i, m := range methods {
		if m == p.Auth {
			selected = i
		}
	}
	auth := ui.NewComboBox(wnd, ui.OptsComboBox().Position(ui.Dpi(130, row(4))).Width(ui.DpiX(310)).
		Texts("Automatic (agent, Pageant, default keys, password)", "Key file", "Pageant", "Password").
		Select(selected))
	label(wnd, "Key file", 14, row(5), 110)
	key := ui.NewEdit(wnd, ui.OptsEdit().Text(p.KeyFile).Position(ui.Dpi(130, row(5))).Width(ui.DpiX(240)))
	browse := ui.NewButton(wnd, ui.OptsButton().Text("&Browse...").Position(ui.Dpi(378, row(5)-1)).Width(ui.DpiX(64)))
	ok, _ := okCancel(wnd, "&Save", 252, row(6)+4)

	updateKey := func() {
		isKeyFile := methods[max(auth.SelectedIndex(), 0)] == remote.AuthKeyFile
		key.Hwnd().EnableWindow(isKeyFile)
		browse.Hwnd().EnableWindow(isKeyFile)
	}
	auth.On().CbnSelChange(updateKey)
	focusOnOpen(wnd, name, updateKey)

	browse.On().BnClicked(func() {
		if f, chosen := pickKeyFile(wnd.Hwnd()); chosen {
			key.SetText(f)
		}
	})

	var result config.Profile
	accepted := false
	ok.On().BnClicked(func() {
		portNum, err := strconv.Atoi(strings.TrimSpace(portEdit.Text()))
		if err != nil || portNum <= 0 || portNum > 65535 {
			ui.MsgError(wnd, "Connect", "Invalid port", "Enter a port number between 1 and 65535.")
			return
		}
		result = config.Profile{
			ID:      p.ID,
			Name:    strings.TrimSpace(name.Text()),
			Host:    strings.TrimSpace(host.Text()),
			Port:    portNum,
			User:    strings.TrimSpace(user.Text()),
			Root:    strings.TrimSpace(root.Text()),
			Auth:    methods[max(auth.SelectedIndex(), 0)],
			KeyFile: strings.TrimSpace(key.Text()),
		}
		if result.Host == "" || result.User == "" {
			ui.MsgError(wnd, "Connect", "Missing fields", "Host and username are required.")
			return
		}
		if result.Auth == remote.AuthKeyFile && result.KeyFile == "" {
			ui.MsgError(wnd, "Connect", "No key file", "Choose a private key file, or another authentication method.")
			return
		}
		if result.Name == "" {
			result.Name = result.User + "@" + result.Host
		}
		accepted = true
		closeModal(wnd)
	})

	wnd.ShowModal()
	return result, accepted
}

// showConflict asks what to do with an upload that collides with a newer or
// unknown version on the server.
func showConflict(parent ui.Parent, c *transfer.ConflictError, localPath string) (app.Choice, bool) {
	wnd := ui.NewModal(parent, ui.OptsModal().Title("File changed on the server").Size(ui.Dpi(470, 210)))

	var detail string
	switch c.Reason {
	case transfer.NeverTransferred:
		detail = "You have not transferred this file before, and a file with this name already exists on the server."
	case transfer.ChangedOutside:
		detail = "The file was changed outside SSH-Cloud, so its version is unknown."
	case transfer.UnknownVersion:
		detail = "The version on the server is unknown."
	case transfer.CreatedMeanwhile:
		detail = "Someone else created this file while you were uploading."
	case transfer.OtherLocalFile:
		detail = "A file with this name already exists on the server."
		if c.Last != nil && c.Last.LocalPath != "" && !c.Last.DragOut {
			detail += "\nYou last transferred it with " + c.Last.LocalPath + ", not with " + localPath + "."
		} else {
			detail += "\nYou are uploading " + localPath + "."
		}
	case transfer.MissingMeanwhile:
		detail = "The file disappeared from the server during your upload: it was deleted, moved, or someone else is replacing it right now."
	default:
		detail = "Someone changed this file on the server since your last transfer."
	}
	server := fmt.Sprintf("On the server: %s", humanSize(c.ServerSize))
	if c.Last != nil {
		server += fmt.Sprintf(". Your last transfer: %s, %s", c.Last.Time.Local().Format("2006-01-02 15:04"), humanSize(c.Last.Size))
	}
	text := fmt.Sprintf("%s\n\n%s\n%s.", c.Path, detail, server)
	ui.NewStatic(wnd, ui.OptsStatic().Text(text).Position(ui.Dpi(14, 14)).Size(ui.Dpi(442, 100)))
	applyAll := ui.NewCheckBox(wnd, ui.OptsCheckBox().Text("Do this for all remaining conflicts").
		Position(ui.Dpi(14, 124)).Size(ui.Dpi(300, 20)))

	choice, all := app.Cancel, false
	button := func(text string, x int, ch app.Choice, id uint16, style co.BS) {
		b := ui.NewButton(wnd, ui.OptsButton().Text(text).Position(ui.Dpi(x, 166)).Width(ui.DpiX(104)).CtrlId(id).CtrlStyle(style))
		b.On().BnClicked(func() {
			choice, all = ch, applyAll.IsChecked()
			closeModal(wnd)
		})
	}
	button("&Replace", 14, app.Replace, 1001, co.BS_PUSHBUTTON)
	button("&Keep both", 126, app.KeepBoth, 1002, co.BS_PUSHBUTTON)
	button("&Skip", 238, app.Skip, uint16(co.ID_OK), co.BS_DEFPUSHBUTTON)
	button("Cancel all", 350, app.Cancel, uint16(co.ID_CANCEL), co.BS_PUSHBUTTON)

	wnd.ShowModal()
	return choice, all
}

// showInput asks for one line of text. Password hides the input.
func showInput(parent ui.Parent, title, prompt, value string, password bool) (string, bool) {
	wnd := ui.NewModal(parent, ui.OptsModal().Title(title).Size(ui.Dpi(400, 120)))
	label(wnd, prompt, 14, 12, 370)
	style := co.ES_AUTOHSCROLL
	if password {
		style |= co.ES_PASSWORD
	}
	edit := ui.NewEdit(wnd, ui.OptsEdit().Text(value).Position(ui.Dpi(14, 40)).Width(ui.DpiX(372)).CtrlStyle(style))
	ok, _ := okCancel(wnd, "OK", 194, 80)
	focusOnOpen(wnd, edit)
	result, accepted := "", false
	ok.On().BnClicked(func() {
		result, accepted = edit.Text(), true
		closeModal(wnd)
	})
	wnd.ShowModal()
	return result, accepted
}

func humanSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d bytes", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
	return fmt.Sprintf("%.1f GB", float64(n)/(1024*1024*1024))
}
