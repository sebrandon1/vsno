package ui

import (
	"bytes"
	"os"
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

func TestTruncate(t *testing.T) {
	for _, c := range []struct {
		in    string
		width int
		want  string
	}{
		{"short", 10, "short"},
		{"exactly10!", 10, "exactly10!"},
		{"⠋ Probing worker-1", 10, "⠋ Probing…"},
		{"anything", 0, "anything"},
		{"anything", -1, "anything"},
		{"ab", 1, "…"},
	} {
		if got := Truncate(c.in, c.width); got != c.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", c.in, c.width, got, c.want)
		}
	}
}

func TestTTYRedrawFitsTerminal(t *testing.T) {
	var b bytes.Buffer
	p := New(&b, true)
	p.tty = true
	p.now = fakeClock()
	p.tick = time.Millisecond
	p.cols = func() int { return 40 }

	s := p.Start("Probing 2 worker(s)")
	s.Update("a-very-long-node-name.example.com ✓")
	time.Sleep(10 * time.Millisecond)
	s.Done("2 probed")

	for _, redraw := range strings.Split(b.String(), "\r\033[K")[1:] {
		if strings.HasSuffix(redraw, "\n") {
			continue // the final line is printed once, so it may wrap
		}
		if n := len([]rune(redraw)); n > 39 {
			t.Errorf("redraw is %d characters on a 40-column terminal: %q", n, redraw)
		}
		if !strings.Contains(redraw, "…") || !strings.HasSuffix(redraw, "s)") {
			t.Errorf("want the node name shortened and the elapsed time kept: %q", redraw)
		}
	}
}

func TestFitLine(t *testing.T) {
	for _, c := range []struct {
		name, head, detail, tail string
		width                    int
		want                     string
	}{
		{"fits", "⠋ Probe", "node-1 ✓", " (3s)", 40, "⠋ Probe node-1 ✓ (3s)"},
		{"unknown width", "⠋ Probe", "node-1 ✓", " (3s)", 0, "⠋ Probe node-1 ✓ (3s)"},
		{"detail shortened", "⠋ Probe", "a-long-node-name ✓", " (3s)", 20, "⠋ Probe a-long… (3s)"},
		{"no room for detail", "⠋ Probing workers", "node-1", " (3s)", 20, "⠋ Probing workers (…"},
		{"no detail", "⠋ Probing many many workers", "", " (3s)", 20, "⠋ Probing many many…"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := fitLine(c.head, c.detail, c.tail, c.width)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
			if c.width > 0 && len([]rune(got)) > c.width {
				t.Errorf("%q is wider than %d", got, c.width)
			}
		})
	}
}

func TestNewOnAFileThatIsNotATerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	p := New(w, true)
	if p.tty || p.cols() != 0 {
		t.Errorf("a pipe is not a terminal: tty=%v cols=%d", p.tty, p.cols())
	}
}
