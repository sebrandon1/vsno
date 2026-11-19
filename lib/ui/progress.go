// Package ui renders step progress for long-running vsno operations: a spinner
// with elapsed time, an optional progress bar, and a final ✓/!/✗ line.
//
// On a terminal it redraws one line per step. Elsewhere (CI, pipes, files) it
// prints one line when a step starts and one when it ends, with no redraws.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Progress writes step progress to w.
type Progress struct {
	w       io.Writer
	tty     bool
	enabled bool
	now     func() time.Time
	tick    time.Duration
	// cols returns the terminal width, or 0 if unknown.
	cols func() int
	pal  Palette
}

// New returns a Progress writing to w. Redraws are used only when w is a terminal.
// A disabled Progress (enabled=false) prints nothing, e.g. for -o json.
func New(w io.Writer, enabled bool) *Progress {
	p := &Progress{w: w, enabled: enabled, now: time.Now, tick: 100 * time.Millisecond, cols: func() int { return 0 },
		pal: Palette{On: enabled && ColorEnabled(w)}}
	if f, ok := w.(*os.File); ok {
		fd := int(f.Fd()) // #nosec G115 -- fd fits in int
		p.tty = term.IsTerminal(fd)
		p.cols = func() int {
			c, _, err := term.GetSize(fd)
			if err != nil {
				return 0
			}
			return c
		}
	}
	return p
}

// Step is one running operation.
type Step struct {
	p      *Progress
	title  string
	start  time.Time
	mu     sync.Mutex
	detail string
	done   int
	total  int
	stop   chan struct{}
	ended  sync.WaitGroup
	frame  int
}

// Start begins a step. Always call Done, Warn or Fail on it.
func (p *Progress) Start(title string) *Step {
	s := &Step{p: p, title: title, start: p.now(), stop: make(chan struct{})}
	if !p.enabled {
		return s
	}
	if !p.tty {
		fmt.Fprintf(p.w, "→ %s\n", title)
		return s
	}
	s.ended.Add(1)
	go s.spin()
	return s
}

func (s *Step) spin() {
	defer s.ended.Done()
	t := time.NewTicker(s.p.tick)
	defer t.Stop()
	for {
		s.draw()
		select {
		case <-s.stop:
			return
		case <-t.C:
		}
	}
}

func (s *Step) draw() {
	s.mu.Lock()
	defer s.mu.Unlock()
	head := fmt.Sprintf("%s %s", s.p.pal.Cyan(spinnerFrames[s.frame%len(spinnerFrames)]), s.title)
	s.frame++
	if s.total > 0 {
		head += " " + Bar(s.done, s.total, 20)
	}
	tail := fmt.Sprintf(" (%s)", FormatElapsed(s.p.now().Sub(s.start)))
	fmt.Fprintf(s.p.w, "\r\033[K%s", fitLine(head, s.detail, tail, s.p.cols()-1))
}

// Update sets the detail text shown after the title, e.g. "bm-worker ✓".
func (s *Step) Update(detail string) {
	s.mu.Lock()
	s.detail = detail
	s.mu.Unlock()
}

// SetProgress shows a bar for done/total. total <= 0 hides it.
func (s *Step) SetProgress(done, total int) {
	s.mu.Lock()
	s.done, s.total = done, total
	s.mu.Unlock()
}

// Done ends the step successfully.
func (s *Step) Done(msg string) { s.finish("✓", msg) }

// Warn ends the step with a warning.
func (s *Step) Warn(msg string) { s.finish("!", msg) }

// Fail ends the step with a failure.
func (s *Step) Fail(msg string) { s.finish("✗", msg) }

func (s *Step) finish(icon, msg string) {
	if !s.p.enabled {
		return
	}
	if s.p.tty {
		close(s.stop)
		s.ended.Wait()
		fmt.Fprint(s.p.w, "\r\033[K")
	}
	line := fmt.Sprintf("%s %s (%s)", s.p.pal.Icon(icon), s.title, FormatElapsed(s.p.now().Sub(s.start)))
	if msg != "" {
		line += ": " + msg
	}
	fmt.Fprintln(s.p.w, line)
}

// fitLine joins head, detail and tail within width characters. A line wider
// than the terminal wraps, and "\r" then returns only to the start of its last
// row, so every redraw would leave a copy behind. The detail is shortened
// first, so the elapsed time in tail stays visible.
func fitLine(head, detail, tail string, width int) string {
	line := head + tail
	if detail != "" {
		line = head + " " + detail + tail
	}
	if width < 1 || VisibleWidth(line) <= width {
		return line
	}
	if room := width - VisibleWidth(head) - VisibleWidth(tail) - 1; detail != "" && room >= 2 {
		return head + " " + Truncate(detail, room) + tail
	}
	return Truncate(StripANSI(head+tail), width) // too narrow even without the detail
}

// Truncate shortens s to at most width characters, ending in "…". A width
// below 1 leaves s unchanged.
func Truncate(s string, width int) string {
	r := []rune(s)
	if width < 1 || len(r) <= width {
		return s
	}
	return string(r[:width-1]) + "…"
}

// Bar renders a fixed-width progress bar like "[########------------] 2/5".
func Bar(done, total, width int) string {
	if total <= 0 || width <= 0 {
		return ""
	}
	if done < 0 {
		done = 0
	}
	if done > total {
		done = total
	}
	filled := done * width / total
	return fmt.Sprintf("[%s%s] %d/%d", strings.Repeat("#", filled), strings.Repeat("-", width-filled), done, total)
}

// FormatElapsed renders a duration as "4s", "2m05s" or "1h03m".
func FormatElapsed(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
