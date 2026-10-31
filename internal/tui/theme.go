package tui

import (
	"charm.land/lipgloss/v2"
)

// lipglossNone is the unstyled style.
var lipglossNone = lipgloss.NewStyle()

// Temperature bands, as upstream's dashboard: normal below 70 °C, warm below 85, hot above.
const (
	tempWarm = 70.0
	tempHot  = 85.0
)

// theme holds every style the views use. The default uses the 16 ANSI colours so it follows
// the terminal's palette; the monochrome theme (NO_COLOR) uses only text attributes.
type theme struct {
	title, dim, label, tabOn, tabOff, selected, err lipgloss.Style
	normal, warm, hot                               lipgloss.Style
	barFill, barEmpty                               lipgloss.Style
	sparkStyle                                      lipgloss.Style
	border                                          lipgloss.Border
	borderStyle                                     lipgloss.Style

	// Glyphs, swapped for ASCII by the fallback.
	fill, empty rune
	ascii       bool
}

func newTheme(noColor, ascii bool) theme {
	t := theme{
		title:    lipgloss.NewStyle().Bold(true),
		label:    lipgloss.NewStyle().Bold(true),
		tabOn:    lipgloss.NewStyle().Reverse(true).Bold(true),
		tabOff:   lipgloss.NewStyle(),
		selected: lipgloss.NewStyle().Reverse(true),
		border:   lipgloss.RoundedBorder(),
		fill:     '█',
		empty:    '░',
		ascii:    ascii,
	}
	if ascii {
		t.border = lipgloss.ASCIIBorder()
		t.fill, t.empty = '#', '.'
	}
	if noColor {
		t.dim = lipgloss.NewStyle().Faint(true)
		t.err = lipgloss.NewStyle().Bold(true)
		t.warm = lipgloss.NewStyle().Bold(true)
		t.hot = lipgloss.NewStyle().Bold(true).Underline(true)
		t.barEmpty = lipgloss.NewStyle().Faint(true)
		return t
	}
	t.dim = lipgloss.NewStyle().Foreground(lipgloss.BrightBlack)
	t.err = lipgloss.NewStyle().Foreground(lipgloss.Red)
	t.normal = lipgloss.NewStyle().Foreground(lipgloss.Green)
	t.warm = lipgloss.NewStyle().Foreground(lipgloss.Yellow)
	t.hot = lipgloss.NewStyle().Foreground(lipgloss.BrightRed).Bold(true)
	t.title = t.title.Foreground(lipgloss.Cyan)
	t.label = t.label.Foreground(lipgloss.Cyan)
	t.barFill = lipgloss.NewStyle().Foreground(lipgloss.Cyan)
	t.barEmpty = lipgloss.NewStyle().Foreground(lipgloss.BrightBlack)
	t.sparkStyle = lipgloss.NewStyle().Foreground(lipgloss.Cyan)
	t.borderStyle = lipgloss.NewStyle().Foreground(lipgloss.BrightBlack)
	return t
}

// tempStyle is the style for a temperature in °C.
func (t theme) tempStyle(c float64) lipgloss.Style {
	switch {
	case c >= tempHot:
		return t.hot
	case c >= tempWarm:
		return t.warm
	default:
		return t.normal
	}
}

// loadStyle colours a bar by load: warm from 75 %, hot from 90 %.
func (t theme) loadStyle(pct float64) lipgloss.Style {
	switch {
	case pct >= 90:
		return t.hot
	case pct >= 75:
		return t.warm
	default:
		return t.barFill
	}
}
