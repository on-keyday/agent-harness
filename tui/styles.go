package tui

import "github.com/charmbracelet/lipgloss"

// Color palette. Tweak freely once the user has run the binary.
var (
	colorBorder  = lipgloss.Color("241") // gray
	colorFocused = lipgloss.Color("69")  // accent (focus highlight)
	colorOK      = lipgloss.Color("42")  // green
	colorWarn    = lipgloss.Color("214") // amber
	colorErr     = lipgloss.Color("196") // red
	colorMuted   = lipgloss.Color("245") // dim
)

// PanelStyle is the unfocused panel border.
var PanelStyle = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(colorBorder)

// PanelStyleFocused is the focused panel border.
var PanelStyleFocused = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(colorFocused)

// HeaderStyle styles the top status line.
var HeaderStyle = lipgloss.NewStyle().Bold(true)

// FooterStyle styles the bottom hotkey hint line.
var FooterStyle = lipgloss.NewStyle().Foreground(colorMuted)

// ErrorStyle marks error text in cmdresult.
var ErrorStyle = lipgloss.NewStyle().Foreground(colorErr)

// OKStyle marks success / connected indicators.
var OKStyle = lipgloss.NewStyle().Foreground(colorOK)

// WarnStyle marks warnings.
var WarnStyle = lipgloss.NewStyle().Foreground(colorWarn)
