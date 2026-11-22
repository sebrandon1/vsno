// Package trace prints what vsno is doing underneath: the cluster API calls it
// makes, named as kubectl would name them, and the commands it runs in the
// builder pod.
//
// It writes to stderr, never stdout, so -o json stays cleanly pipeable, and it
// carries no color codes so piped stderr is readable too. Nothing here reads a
// request body, a header, stdin, or an auth file: only the method, the path, and
// three query keys are ever rendered.
package trace

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// sink is the process-wide destination, nil while tracing is off. Libraries
// build clients and runners deep inside a command with no writer to thread
// through, and one process runs one command against one stderr.
var sink atomic.Pointer[Collapser]

// Enable starts tracing to w, replacing any previous destination.
func Enable(w io.Writer) { sink.Store(NewCollapser(w)) }

// Disable stops tracing, writing whatever line is pending.
func Disable() {
	if c := sink.Swap(nil); c != nil {
		c.Flush()
	}
}

// Enabled reports whether tracing is on.
func Enabled() bool { return sink.Load() != nil }

// Logf traces one formatted line.
func Logf(format string, a ...any) { Line(fmt.Sprintf(format, a...)) }

// Line traces one line. Repeated identical lines are counted rather than
// repeated, and an empty line is dropped.
func Line(s string) {
	if s == "" {
		return
	}
	if c := sink.Load(); c != nil {
		c.Line(s)
	}
}

// Flush writes the pending line, if any. A command calls it before handing the
// terminal to something long-running, and Execute defers it so the last count
// is never lost.
func Flush() {
	if c := sink.Load(); c != nil {
		c.Flush()
	}
}

// Collapser writes lines to a writer as they happen, and counts a run of
// identical ones. When the run ends it writes the line again with the count, so
// a wait says when it started looking and how many times it looked, rather than
// printing every request.
type Collapser struct {
	mu   sync.Mutex
	w    io.Writer
	line string
	n    int
}

// NewCollapser returns a Collapser writing to w.
func NewCollapser(w io.Writer) *Collapser { return &Collapser{w: w} }

// Line writes s at once, or counts it if it repeats the line before it.
func (c *Collapser) Line(s string) {
	if s == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if s == c.line {
		c.n++
		return
	}
	c.summaryLocked()
	fmt.Fprintln(c.w, s)
	c.line, c.n = s, 1
}

// Flush writes the count of a run that has ended. It is safe to call
// concurrently with Line.
func (c *Collapser) Flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.summaryLocked()
}

// summaryLocked writes the count of a run worth counting. Two reads of the same
// thing is not news, and the line itself has already been written; a wait that
// looked a hundred times is.
func (c *Collapser) summaryLocked() {
	if c.n > 2 {
		fmt.Fprintf(c.w, "%s (x%d)\n", c.line, c.n)
	}
	c.line, c.n = "", 0
}
