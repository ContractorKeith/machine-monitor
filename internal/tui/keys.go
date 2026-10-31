package tui

import "charm.land/bubbles/v2/key"

// keyMap is every binding; it also feeds the ? help screen.
type keyMap struct {
	Dashboard, Device, Processes, Next, Help, Quit key.Binding
	Up, Down, PageUp, PageDown, Top, Bottom        key.Binding
	Filter, Sort, Reverse, Clear                   key.Binding
	Left, Right, MoveUp, MoveDown, Rename, Remove  key.Binding // hub mode

	hub bool // hub mode: help lists the device keys
}

func newKeyMap(hub bool) keyMap {
	return keyMap{
		hub:       hub,
		Left:      key.NewBinding(key.WithKeys("left", "h"), key.WithHelp("←/h", "previous device")),
		Right:     key.NewBinding(key.WithKeys("right", "l"), key.WithHelp("→/l", "next device")),
		MoveUp:    key.NewBinding(key.WithKeys("K"), key.WithHelp("K", "move card earlier")),
		MoveDown:  key.NewBinding(key.WithKeys("J"), key.WithHelp("J", "move card later")),
		Rename:    key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "rename device")),
		Remove:    key.NewBinding(key.WithKeys("D"), key.WithHelp("D", "remove device")),
		Dashboard: key.NewBinding(key.WithKeys("1"), key.WithHelp("1", "dashboard")),
		Device:    key.NewBinding(key.WithKeys("2"), key.WithHelp("2", "device")),
		Processes: key.NewBinding(key.WithKeys("3"), key.WithHelp("3", "processes")),
		Next:      key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "next view")),
		Help:      key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:      key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
		Up:        key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:      key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		PageUp:    key.NewBinding(key.WithKeys("pgup"), key.WithHelp("pgup", "page up")),
		PageDown:  key.NewBinding(key.WithKeys("pgdown", "space"), key.WithHelp("pgdn", "page down")),
		Top:       key.NewBinding(key.WithKeys("home", "g"), key.WithHelp("g", "top")),
		Bottom:    key.NewBinding(key.WithKeys("end", "G"), key.WithHelp("G", "bottom")),
		Filter:    key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter processes")),
		Sort:      key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "sort column")),
		Reverse:   key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "reverse sort")),
		Clear:     key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "clear filter / close help")),
	}
}

// ShortHelp implements help.KeyMap.
func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Help, k.Quit}
}

// FullHelp implements help.KeyMap: one column per group.
func (k keyMap) FullHelp() [][]key.Binding {
	groups := [][]key.Binding{
		{k.Dashboard, k.Device, k.Processes, k.Next, k.Help, k.Quit},
		{k.Up, k.Down, k.PageUp, k.PageDown, k.Top, k.Bottom},
	}
	if k.hub { // the process list is local-only, so its keys make room for the device keys
		return append(groups, []key.Binding{k.Left, k.Right, k.MoveUp, k.MoveDown, k.Rename, k.Remove})
	}
	return append(groups, []key.Binding{k.Filter, k.Sort, k.Reverse, k.Clear})
}
