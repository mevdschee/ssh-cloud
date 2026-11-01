//go:build windows

package ui

// Tabs: one per open connection, as buttons in the grey band at the top
// (like the tabs in Windows 11 Explorer's title bar). Each tab is a session
// with its own connection, folder, history and listing. The window shows one
// session at a time: switching saves the window's state into the old session
// and loads the new one.

import (
	"log/slog"

	"github.com/mevdschee/ssh-cloud/internal/app"
	"github.com/mevdschee/ssh-cloud/internal/config"
	"github.com/mevdschee/ssh-cloud/internal/remote"
)

// session is one tab.
type session struct {
	profile       config.Profile
	app           *app.App
	canCopy       bool
	dir           string
	back, forward []string
	entries       []remote.Entry
}

// Tab context menu ids.
const (
	cmdTabClose uint16 = 2101 + iota
	cmdTabCloseOthers
	cmdTabNew
)

// rebuildTabs shows the sessions in the tab strip.
func (me *Explorer) rebuildTabs() {
	names := make([]string, len(me.sessions))
	for i, s := range me.sessions {
		names[i] = s.profile.DisplayName()
	}
	me.tabs.set(names, me.active)
	me.updateTabButtons()
}

// updateTabButtons disables tab switching while an operation runs, so its
// result cannot end up in another tab.
func (me *Explorer) updateTabButtons() {
	me.tabs.setEnabled(!me.busy)
}

// saveActive stores the window's state in the active session.
func (me *Explorer) saveActive() {
	if me.active < 0 || me.active >= len(me.sessions) {
		return
	}
	s := me.sessions[me.active]
	me.mu.Lock()
	s.app, s.profile = me.app, me.profile
	me.mu.Unlock()
	s.canCopy, s.dir, s.back, s.forward, s.entries = me.canCopy, me.dir, me.back, me.forward, me.entries
}

// activate shows session i: its folder from cache at once, then refreshed.
func (me *Explorer) activate(i int) {
	me.saveActive()
	me.active = i
	s := me.sessions[i]
	me.mu.Lock()
	me.app, me.profile = s.app, s.profile
	me.mu.Unlock()
	me.canCopy, me.dir, me.back, me.forward = s.canCopy, s.dir, s.back, s.forward
	me.wnd.Hwnd().SetWindowText("SSH-Cloud - " + s.profile.DisplayName())
	me.rebuildTabs()
	me.resetTree(s.profile.DisplayName())
	me.show(s.entries)
	me.syncTree(me.dir, s.entries)
	slog.Info("tab activated", "tab", i, "profile", s.profile.DisplayName())
	me.navigate(me.dir, false)
}

func (me *Explorer) switchTab(i int) {
	if me.busy || i < 0 || i >= len(me.sessions) {
		return
	}
	if i != me.active {
		me.activate(i)
	}
}

func (me *Explorer) nextTab(delta int) {
	if n := len(me.sessions); n > 1 {
		me.switchTab((me.active + delta + n) % n)
	}
}

// openSession adds a connected session as a new tab, or replaces the active
// tab's connection.
func (me *Explorer) openSession(p config.Profile, a *app.App, canCopy, newTab bool) {
	s := &session{profile: p, app: a, canCopy: canCopy}
	if newTab || me.active < 0 {
		me.sessions = append(me.sessions, s)
		me.activate(len(me.sessions) - 1)
		return
	}
	old := me.sessions[me.active]
	me.sessions[me.active] = s
	if old.app != nil && old.app != a {
		old.app.Close()
	}
	me.mu.Lock()
	me.app = nil // already closed; activate loads the new session
	me.mu.Unlock()
	i := me.active
	me.active = -1 // nothing to save from the replaced session
	me.activate(i)
}

// closeTab closes a tab and its connection.
func (me *Explorer) closeTab(i int) {
	if me.busy || i < 0 || i >= len(me.sessions) {
		return
	}
	me.saveActive()
	s := me.sessions[i]
	slog.Info("tab closed", "tab", i, "profile", s.profile.DisplayName())
	if s.app != nil {
		s.app.Close()
	}
	if me.clipApp == s.app {
		me.clip, me.clipApp = nil, nil
	}
	me.sessions = append(me.sessions[:i], me.sessions[i+1:]...)
	switch {
	case len(me.sessions) == 0:
		me.active = -1
		me.mu.Lock()
		me.app, me.profile = nil, config.Profile{}
		me.mu.Unlock()
		me.dir, me.back, me.forward, me.entries = "", nil, nil, nil
		me.tree.DeleteAllItems()
		me.list.DeleteAllItems()
		me.address.SetText("")
		me.wnd.Hwnd().SetWindowText("SSH-Cloud")
		me.setStatus("Not connected")
		me.rebuildTabs()
		me.updateButtons()
	case i == me.active:
		me.active = -1
		me.activate(min(i, len(me.sessions)-1))
	default:
		if i < me.active {
			me.active--
		}
		me.rebuildTabs()
	}
}

func (me *Explorer) closeAllSessions() {
	for _, s := range me.sessions {
		if s.app != nil {
			s.app.Close()
		}
	}
	me.mu.Lock()
	if me.app != nil && me.active < 0 {
		me.app.Close()
	}
	me.mu.Unlock()
}

// tabEvents connects the tab strip: click selects, the close button and a
// middle-click close, + opens a new tab, right-click shows a menu.
func (me *Explorer) tabEvents() {
	me.tabs.onSelect = me.switchTab
	me.tabs.onClose = me.closeTab
	me.tabs.onNew = func() {
		if len(me.sessions) < maxTabs {
			me.promptConnect(true)
		}
	}
	me.tabs.onMenu = func(i int) {
		isTab := i >= 0 && i < len(me.sessions)
		items := []menuItem{
			{id: cmdTabClose, text: "&Close tab\tCtrl+W", enabled: isTab && !me.busy},
			{id: cmdTabCloseOthers, text: "Close &other tabs", enabled: isTab && !me.busy && len(me.sessions) > 1},
			{},
			{id: cmdTabNew, text: "&New tab\tCtrl+T", enabled: !me.busy && len(me.sessions) < maxTabs},
		}
		switch popupMenu(me.wnd.Hwnd(), items) {
		case cmdTabClose:
			me.closeTab(i)
		case cmdTabCloseOthers:
			keep := me.sessions[i]
			for j := len(me.sessions) - 1; j >= 0; j-- {
				if me.sessions[j] != keep {
					me.closeTab(j)
				}
			}
		case cmdTabNew:
			me.promptConnect(true)
		}
	}
}
