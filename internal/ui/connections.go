//go:build windows

package ui

import (
	"fmt"
	"log/slog"

	"github.com/mevdschee/ssh-cloud/internal/config"
	"github.com/mevdschee/ssh-cloud/internal/remote"
	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
	"github.com/rodrigocfd/windigo/win"
)

// showConnections is the connection manager: a list of saved connections
// with New, Edit, Delete and Connect. Changes are saved immediately. It
// returns the connection to open, or false when closed without connecting.
func showConnections(parent ui.Parent, cfg *config.Config, cfgDir string) (config.Profile, bool) {
	wnd := newModal(parent, ui.OptsModal().Title("Connections").Size(ui.Dpi(560, 320)))
	list := ui.NewListView(wnd, ui.OptsListView().
		Position(ui.Dpi(12, 12)).
		Size(ui.Dpi(430, 296)).
		CtrlStyle(co.LVS_REPORT|co.LVS_NOSORTHEADER|co.LVS_SHOWSELALWAYS|co.LVS_SINGLESEL).
		CtrlExStyle(co.LVS_EX_FULLROWSELECT|co.LVS_EX_DOUBLEBUFFER).
		Column("Name", ui.DpiX(130)).
		Column("Server", ui.DpiX(150)).
		Column("Remote folder", ui.DpiX(140)))
	drawSelection(list)
	button := func(text string, y int, id uint16, style co.BS) *ui.Button {
		return ui.NewButton(wnd, ui.OptsButton().Text(text).Position(ui.Dpi(452, y)).Width(ui.DpiX(96)).
			CtrlId(id).CtrlStyle(style))
	}
	btnConnect := button("&Connect", 12, uint16(co.ID_OK), co.BS_DEFPUSHBUTTON)
	btnNew := button("&New...", 52, 1101, co.BS_PUSHBUTTON)
	btnEdit := button("&Edit...", 86, 1102, co.BS_PUSHBUTTON)
	btnDelete := button("&Delete", 120, 1103, co.BS_PUSHBUTTON)
	btnClose := button("Close", 282, uint16(co.ID_CANCEL), co.BS_PUSHBUTTON)

	save := func() {
		if err := cfg.Save(cfgDir); err != nil {
			slog.Error("save settings", "err", err)
			ui.MsgError(wnd, "Connections", "Could not save the connections", err.Error())
		}
	}
	selected := func() (config.Profile, bool) {
		items := list.SelectedItems()
		if len(items) != 1 {
			return config.Profile{}, false
		}
		id, _ := items[0].Data().(string)
		return cfg.Profile(id)
	}
	update := func() {
		_, ok := selected()
		btnConnect.Hwnd().EnableWindow(ok)
		btnEdit.Hwnd().EnableWindow(ok)
		btnDelete.Hwnd().EnableWindow(ok)
	}
	fill := func(selectID string) {
		list.DeleteAllItems()
		for _, p := range cfg.Profiles {
			server := fmt.Sprintf("%s@%s", p.User, p.Host)
			if p.Port != 0 && p.Port != 22 {
				server += fmt.Sprintf(":%d", p.Port)
			}
			root := p.Root
			if root == "" {
				root = "(home folder)"
			}
			item := list.AddItem(p.DisplayName(), server, root)
			item.SetData(p.ID)
			if p.ID == selectID {
				item.Select(true).Focus()
			}
		}
		update()
	}

	var chosen config.Profile
	connect := false
	doConnect := func() {
		if p, ok := selected(); ok {
			chosen, connect = p, true
			closeModal(wnd)
		}
	}
	doEdit := func() {
		p, ok := selected()
		if !ok {
			return
		}
		if p, ok = editProfile(wnd, "Edit connection", p); ok {
			cfg.Put(p)
			save()
			fill(p.ID)
		}
	}

	wnd.On().WmCreate(func(_ ui.WmCreate) int {
		last, _ := cfg.Last()
		fill(last.ID)
		if len(cfg.Profiles) > 0 && list.SelectedItemCount() == 0 {
			list.Item(0).Select(true).Focus()
		}
		update()
		wnd.UiThread(func() { list.Focus() })
		return 0
	})
	list.On().LvnItemChanged(func(_ *win.NMLISTVIEW) { update() })
	list.On().LvnItemActivate(func(_ *win.NMITEMACTIVATE) { doConnect() })
	btnConnect.On().BnClicked(doConnect)
	btnEdit.On().BnClicked(doEdit)
	btnNew.On().BnClicked(func() {
		if p, ok := editProfile(wnd, "New connection", config.Profile{Auth: remote.AuthAuto}); ok {
			p = cfg.Put(p)
			save()
			fill(p.ID)
		}
	})
	btnDelete.On().BnClicked(func() {
		p, ok := selected()
		if !ok {
			return
		}
		if ui.MsgOkCancel(wnd, "Delete connection", "Delete \""+p.DisplayName()+"\"?",
			"The connection is removed from this list. Nothing on the server is changed.", "Delete") {
			cfg.Delete(p.ID)
			save()
			fill("")
		}
	})
	btnClose.On().BnClicked(func() { closeModal(wnd) })

	wnd.ShowModal()
	return chosen, connect
}
