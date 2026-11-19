package ui

import (
	"io"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// Palette adds ANSI colors to text when On. The zero value prints plain text.
type Palette struct{ On bool }

func (p Palette) paint(code, s string) string {
	if !p.On || s == "" {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

// Bold, Dim, Red, Green, Yellow and Cyan style s.
func (p Palette) Bold(s string) string   { return p.paint("1", s) }
func (p Palette) Dim(s string) string    { return p.paint("2", s) }
func (p Palette) Red(s string) string    { return p.paint("31", s) }
func (p Palette) Green(s string) string  { return p.paint("32", s) }
func (p Palette) Yellow(s string) string { return p.paint("33", s) }
func (p Palette) Cyan(s string) string   { return p.paint("36", s) }

// Icon colors a status icon: ✓ green, ! yellow, ✗ red, anything else dim.
func (p Palette) Icon(icon string) string {
	switch icon {
	case "✓":
		return p.Green(icon)
	case "!":
		return p.Yellow(icon)
	case "✗":
		return p.Red(icon)
	default:
		return p.Cyan(icon)
	}
}

// ColorEnabled reports whether w is a terminal that should get colors: not
// when NO_COLOR is set (https://no-color.org) or TERM is "dumb".
func ColorEnabled(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	return term.IsTerminal(int(f.Fd())) // #nosec G115 -- fd fits in int
}

var ansi = regexp.MustCompile("\033\\[[0-9;]*m")

// StripANSI removes color codes.
func StripANSI(s string) string { return ansi.ReplaceAllString(s, "") }

// VisibleWidth is the number of characters s shows, ignoring color codes.
func VisibleWidth(s string) int { return utf8.RuneCountInString(StripANSI(s)) }

// Table lines up columns like text/tabwriter with two spaces between them,
// but measures cells by their visible width, so color codes don't shift the
// columns. As with tabwriter, a row's last cell is not padded and does not
// widen its column.
type Table struct {
	rows [][]string
}

// Row adds a row.
func (t *Table) Row(cells ...string) { t.rows = append(t.rows, cells) }

// Write prints the rows to w.
func (t *Table) Write(w io.Writer) {
	var widths []int
	for _, r := range t.rows {
		for i := 0; i < len(r)-1; i++ {
			if i == len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], VisibleWidth(r[i]))
		}
	}
	var b strings.Builder
	for _, r := range t.rows {
		var line strings.Builder
		for i, c := range r {
			line.WriteString(c)
			if i < len(r)-1 {
				line.WriteString(strings.Repeat(" ", widths[i]-VisibleWidth(c)+2))
			}
		}
		// An empty last cell would leave the padding before it.
		b.WriteString(strings.TrimRight(line.String(), " "))
		b.WriteByte('\n')
	}
	_, _ = io.WriteString(w, b.String())
}
