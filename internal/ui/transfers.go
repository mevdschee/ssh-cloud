//go:build windows

package ui

import (
	"github.com/mevdschee/ssh-cloud/internal/transferlog"
	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
)

// maxShown limits the Transfers window to the most recent entries.
const maxShown = 2000

// showTransfers shows the transfer list of the current connection, newest
// first.
func showTransfers(parent ui.Parent, title string, entries []transferlog.Entry) {
	wnd := ui.NewModal(parent, ui.OptsModal().Title(title).Size(ui.Dpi(900, 460)).
		Style(co.WS_CAPTION|co.WS_SYSMENU|co.WS_CLIPCHILDREN|co.WS_BORDER|co.WS_VISIBLE|co.WS_SIZEBOX))
	list := ui.NewListView(wnd, ui.OptsListView().
		Position(ui.Dpi(12, 12)).
		Size(ui.Dpi(876, 400)).
		Layout(ui.LAY_RESIZE_RESIZE).
		CtrlStyle(co.LVS_REPORT|co.LVS_NOSORTHEADER|co.LVS_SHOWSELALWAYS).
		CtrlExStyle(co.LVS_EX_FULLROWSELECT|co.LVS_EX_DOUBLEBUFFER).
		Column("Time", ui.DpiX(130)).
		Column("Action", ui.DpiX(80)).
		Column("Cloud path", ui.DpiX(240)).
		Column("Local path", ui.DpiX(220)).
		Column("Size", ui.DpiX(80)).
		Column("Result", ui.DpiX(110)))
	closeBtn := ui.NewButton(wnd, ui.OptsButton().Text("Close").Position(ui.Dpi(800, 422)).
		CtrlId(uint16(co.ID_CANCEL)).Layout(ui.LAY_MOVE_MOVE))
	closeBtn.On().BnClicked(func() { closeModal(wnd) })

	wnd.On().WmCreate(func(_ ui.WmCreate) int {
		list.SetRedraw(false)
		shown := 0
		for i := len(entries) - 1; i >= 0 && shown < maxShown; i-- {
			e := entries[i]
			remotePath, size, result := "/"+e.RemotePath, "", string(e.Result)
			if e.Direction == transferlog.Move {
				remotePath = "/" + e.From + "  ->  /" + e.RemotePath
			} else if e.Size > 0 || e.Result == transferlog.Done {
				size = humanSize(e.Size)
			}
			if e.Error != "" {
				result += ": " + e.Error
			}
			list.AddItem(e.Time.Local().Format("2006-01-02 15:04:05"), string(e.Direction), remotePath, e.LocalPath, size, result)
			shown++
		}
		list.SetRedraw(true)
		list.Col(4).SetJustification(co.HDF_RIGHT)
		return 0
	})
	wnd.ShowModal()
}
