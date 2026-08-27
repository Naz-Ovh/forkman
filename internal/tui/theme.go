package tui

import "charm.land/lipgloss/v2"

// Theme holds every style the TUI uses. When Plain is true all styles are
// bare lipgloss styles with no colors or attributes, so rendered views are
// pure text (used for NO_COLOR and for golden tests).
type Theme struct {
	Title    lipgloss.Style
	Header   lipgloss.Style
	Muted    lipgloss.Style
	OK       lipgloss.Style
	Fail     lipgloss.Style
	Warn     lipgloss.Style
	Info     lipgloss.Style
	Cursor   lipgloss.Style
	Selected lipgloss.Style
	Match    lipgloss.Style
	Bar      lipgloss.Style
	Plain    bool
}

// NewTheme builds the style set. plain=true yields attribute-free styles.
func NewTheme(plain bool) Theme {
	if plain {
		s := lipgloss.NewStyle()
		return Theme{
			Title: s, Header: s, Muted: s, OK: s, Fail: s,
			Warn: s, Info: s, Cursor: s, Selected: s, Match: s, Bar: s,
			Plain: true,
		}
	}
	return Theme{
		Title:    lipgloss.NewStyle().Bold(true),
		Header:   lipgloss.NewStyle().Foreground(lipgloss.Color("#7d56f4")).Bold(true),
		Muted:    lipgloss.NewStyle().Faint(true),
		OK:       lipgloss.NewStyle().Foreground(lipgloss.Color("#3fb950")),
		Fail:     lipgloss.NewStyle().Foreground(lipgloss.Color("#f85149")),
		Warn:     lipgloss.NewStyle().Foreground(lipgloss.Color("#d29922")),
		Info:     lipgloss.NewStyle().Foreground(lipgloss.Color("#58a6ff")),
		Cursor:   lipgloss.NewStyle().Foreground(lipgloss.Color("#7d56f4")).Bold(true),
		Selected: lipgloss.NewStyle().Foreground(lipgloss.Color("#3fb950")).Bold(true),
		Match:    lipgloss.NewStyle().Foreground(lipgloss.Color("#d29922")).Bold(true),
		Bar:      lipgloss.NewStyle().Foreground(lipgloss.Color("#7d56f4")),
	}
}

// PlainFromEnv reports whether plain (color-free) rendering is requested:
// NO_COLOR non-empty enables it unless FORCE_COLOR is also non-empty.
func PlainFromEnv(getenv func(string) string) bool {
	if getenv == nil {
		return false
	}
	if getenv("FORCE_COLOR") != "" {
		return false
	}
	return getenv("NO_COLOR") != ""
}
