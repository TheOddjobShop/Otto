package tui

import "charm.land/lipgloss/v2"

// Palette. Deliberately narrow — amber for Otto and anything active, cool grays
// for chrome, and one red for errors. The lightbulb already supplies all the
// color the screen needs; competing with it makes the UI look busy.
var (
	amber    = lipgloss.Color("#f59e0b")
	dimGray  = lipgloss.Color("#6b7280")
	softGray = lipgloss.Color("#9ca3af")
	errRed   = lipgloss.Color("#ef4444")
)

var (
	helpStyle = lipgloss.NewStyle().Foreground(dimGray)

	statusStyle = lipgloss.NewStyle().Foreground(softGray)

	errorStyle = lipgloss.NewStyle().Foreground(errRed)

	userLabelStyle = lipgloss.NewStyle().Foreground(softGray).Bold(true)
	userStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#e5e7eb"))

	ottoLabelStyle = lipgloss.NewStyle().Foreground(amber).Bold(true)
	ottoStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#fef3c7"))
)
