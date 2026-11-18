package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestBar(t *testing.T) {
	cases := []struct {
		done, total, width int
		want               string
	}{
		{0, 4, 8, "[--------] 0/4"},
		{2, 4, 8, "[####----] 2/4"},
		{4, 4, 8, "[########] 4/4"},
		{9, 4, 8, "[########] 4/4"}, // clamped
		{-1, 4, 8, "[--------] 0/4"},
		{1, 0, 8, ""},
	}
	for _, c := range cases {
		if got := Bar(c.done, c.total, c.width); got != c.want {
			t.Errorf("Bar(%d,%d,%d) = %q, want %q", c.done, c.total, c.width, got, c.want)
		}
	}
}

func TestFormatElapsed(t *testing.T) {
	cases := map[time.Duration]string{
		4 * time.Second:                "4s",
		125 * time.Second:              "2m05s",
		time.Hour + 3*time.Minute + 20: "1h03m",
	}
	for d, want := range cases {
		if got := FormatElapsed(d); got != want {
			t.Errorf("FormatElapsed(%s) = %q, want %q", d, got, want)
		}
	}
}

// fakeClock advances 7s per call so elapsed times are deterministic.
func fakeClock() func() time.Time {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	n := 0
	return func() time.Time {
		n++
		return t0.Add(time.Duration(n-1) * 7 * time.Second)
	}
}

func TestNonTTYPrintsStartAndEnd(t *testing.T) {
	var b bytes.Buffer
	p := New(&b, true) // a bytes.Buffer is never a terminal
	p.now = fakeClock()

	s := p.Start("Probing workers")
	s.SetProgress(1, 2) // no redraws off-terminal
	s.Update("bm-worker ✓")
	s.Done("2 probed")

	want := "→ Probing workers\n✓ Probing workers (7s): 2 probed\n"
	if b.String() != want {
		t.Fatalf("got %q, want %q", b.String(), want)
	}
}

func TestDisabledPrintsNothing(t *testing.T) {
	var b bytes.Buffer
	p := New(&b, false)
	s := p.Start("x")
	s.Fail("boom")
	if b.Len() != 0 {
		t.Fatalf("disabled progress wrote %q", b.String())
	}
}

func TestTTYRedrawsAndClears(t *testing.T) {
	var b bytes.Buffer
	p := New(&b, true)
	p.tty = true // force terminal mode
	p.now = fakeClock()
	p.tick = time.Millisecond

	s := p.Start("Installing")
	s.SetProgress(3, 6)
	s.Update("bootstrapping")
	time.Sleep(10 * time.Millisecond)
	s.Warn("slow")

	out := b.String()
	for _, want := range []string{"\r\033[K", "Installing [##########----------] 3/6 bootstrapping", "! Installing ("} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q in %q", want, out)
		}
	}
	if !strings.HasSuffix(out, ": slow\n") {
		t.Errorf("final line should end the step: %q", out)
	}
}
