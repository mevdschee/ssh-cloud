//go:build windows

package ui

import (
	"strings"

	"github.com/mevdschee/ssh-cloud/internal/logging"
	"github.com/mevdschee/ssh-cloud/internal/remote"
	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
	"github.com/rodrigocfd/windigo/win"
)

// The folder tree is loaded lazily: a folder node gets a placeholder child
// (so it shows an expand button) until it is expanded for the first time.
// Node data is the folder's cloud path; placeholders have no data.

const tveExpand = 0x0002 // TVE_EXPAND in NMTREEVIEW.action

func (me *Explorer) resetTree(name string) {
	me.tree.DeleteAllItems()
	root := me.tree.AddRoot(name)
	root.SetData("")
	setTreeImage(me.tree.Hwnd(), root.Htreeitem(), shellTypeOf("", true).icon)
	root.AddChild("...") // placeholder
	root.Expand(true)
}

func isPlaceholder(item ui.TreeViewItem) bool {
	_, ok := item.Data().(string)
	return !ok
}

func hasPlaceholder(item ui.TreeViewItem) bool {
	children := item.Children()
	return len(children) == 1 && isPlaceholder(children[0])
}

func (me *Explorer) treeEvents() {
	me.tree.On().TvnItemExpanding(func(p *win.NMTREEVIEW) bool {
		item := me.tree.Item(p.ItemNew.HItem)
		if p.Action&tveExpand != 0 && hasPlaceholder(item) {
			me.loadTreeChildren(item)
		}
		return false // allow
	})
	me.tree.On().TvnSelChanged(func(p *win.NMTREEVIEW) {
		if me.treeSync {
			return
		}
		item := me.tree.Item(p.ItemNew.HItem)
		if dir, ok := item.Data().(string); ok && dir != me.dir {
			me.navigate(dir, true)
		}
	})
}

// loadTreeChildren lists a folder in the background and replaces the
// placeholder with its subfolders.
func (me *Explorer) loadTreeChildren(item ui.TreeViewItem) {
	me.mu.Lock()
	a := me.app
	me.mu.Unlock()
	dir, _ := item.Data().(string)
	if a == nil {
		return
	}
	go func() {
		defer logging.Recover("tree")
		entries, err := a.List(me.ctx, dir)
		me.wnd.UiThread(func() {
			if err != nil {
				if children := item.Children(); len(children) == 1 && isPlaceholder(children[0]) {
					children[0].SetText("(could not load)")
				}
				return
			}
			me.setTreeChildren(item, me.visible(entries))
		})
	}()
}

// setTreeChildren makes a node's children match the folders in entries. It
// only rebuilds when the set of folder names changed, so expanded subfolders
// stay expanded on a normal refresh.
func (me *Explorer) setTreeChildren(item ui.TreeViewItem, entries []remote.Entry) {
	var folders []remote.Entry
	for _, e := range entries {
		if e.IsDir {
			folders = append(folders, e)
		}
	}
	children := item.Children()
	if !hasPlaceholder(item) && len(children) == len(folders) {
		same := true
		for i, c := range children {
			if p, _ := c.Data().(string); p != folders[i].Path {
				same = false
				break
			}
		}
		if same {
			return
		}
	}
	for _, c := range children {
		c.Delete()
	}
	icon := shellTypeOf("", true).icon
	for _, f := range folders {
		child := item.AddChild(f.Name)
		child.SetData(f.Path)
		setTreeImage(me.tree.Hwnd(), child.Htreeitem(), icon)
		child.AddChild("...") // placeholder until expanded
	}
}

// syncTree updates and selects the node of the folder shown in the list, if
// the tree has loaded the way down to it.
func (me *Explorer) syncTree(dir string, entries []remote.Entry) {
	roots := me.tree.Roots()
	if len(roots) == 0 {
		return
	}
	node := roots[0]
	if dir != "" {
		for _, part := range strings.Split(dir, "/") {
			want := remote.Join(nodePath(node), part)
			found := false
			for _, c := range node.Children() {
				if p, ok := c.Data().(string); ok && p == want {
					node, found = c, true
					break
				}
			}
			if !found {
				return // not loaded that far; leave the tree alone
			}
		}
	}
	if !hasPlaceholder(node) || node.IsExpanded() {
		me.setTreeChildren(node, entries)
	}
	me.treeSync = true
	me.tree.Hwnd().SendMessage(co.TVM_SELECTITEM, win.WPARAM(co.TVGN_CARET), win.LPARAM(node.Htreeitem()))
	node.EnsureVisible()
	me.treeSync = false
}

func nodePath(item ui.TreeViewItem) string {
	p, _ := item.Data().(string)
	return p
}
