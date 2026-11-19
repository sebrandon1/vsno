package ui

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestPalette(t *testing.T) {
	off, on := Palette{}, Palette{On: true}
	if got := off.Red("x"); got != "x" {
		t.Errorf("off: %q", got)
	}
	if got := on.Red(""); got != "" {
		t.Errorf("empty text stays empty: %q", got)
	}
	for _, c := range []struct {
		got, want string
	}{
		{on.Bold("x"), "\033[1mx\033[0m"},
		{on.Dim("x"), "\033[2mx\033[0m"},
		{on.Red("x"), "\033[31mx\033[0m"},
		{on.Green("x"), "\033[32mx\033[0m"},
		{on.Yellow("x"), "\033[33mx\033[0m"},
		{on.Cyan("x"), "\033[36mx\033[0m"},
		{on.Icon("✓"), on.Green("✓")},
		{on.Icon("!"), on.Yellow("!")},
		{on.Icon("✗"), on.Red("✗")},
		{on.Icon("i"), on.Cyan("i")},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

func TestVisibleWidth(t *testing.T) {
	on := Palette{On: true}
	s := on.Bold("✓ ok") + " " + on.Red("✗")
	if got := VisibleWidth(s); got != 6 {
		t.Errorf("VisibleWidth = %d, want 6", got)
	}
	if got := StripANSI(s); got != "✓ ok ✗" {
		t.Errorf("StripANSI = %q", got)
	}
}

func TestTableAlignsColoredCells(t *testing.T) {
	render := func(p Palette) string {
		var tb Table
		tb.Row(p.Green("✓"), "cpu", "104 threads")
		tb.Row(p.Yellow("!"), "virtualization", "nested")
		tb.Row("", "", p.Yellow("→")+" hint") // shorter first cell, still aligned
		tb.Row("i", "x")                      // fewer cells: the last one is not padded
		var b bytes.Buffer
		tb.Write(&b)
		return b.String()
	}
	want := "✓  cpu             104 threads\n" +
		"!  virtualization  nested\n" +
		"                   → hint\n" +
		"i  x\n"
	if got := render(Palette{}); got != want {
		t.Errorf("plain:\n%s\nwant:\n%s", got, want)
	}
	if got := StripANSI(render(Palette{On: true})); got != want {
		t.Errorf("colored, stripped:\n%s\nwant:\n%s", got, want)
	}
}

func TestColorEnabled(t *testing.T) {
	if ColorEnabled(&bytes.Buffer{}) {
		t.Error("a buffer is not a terminal")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm")
	if ColorEnabled(w) {
		t.Error("a pipe is not a terminal")
	}
	for _, env := range [][2]string{{"NO_COLOR", "1"}, {"TERM", "dumb"}} {
		t.Setenv(env[0], env[1])
		if ColorEnabled(os.Stdout) {
			t.Errorf("%s=%s must turn color off", env[0], env[1])
		}
	}
}

func TestProgressColorsOnlyWhenEnabled(t *testing.T) {
	var b bytes.Buffer
	p := New(&b, true)
	p.Start("x").Done("")
	if strings.Contains(b.String(), "\033[") {
		t.Errorf("a buffer gets no color: %q", b.String())
	}
	b.Reset()
	p.pal = Palette{On: true}
	p.Start("x").Done("")
	if !strings.Contains(b.String(), Palette{On: true}.Green("✓")) {
		t.Errorf("want a green ✓: %q", b.String())
	}
}
