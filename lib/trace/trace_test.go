package trace

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

// A line is written when it happens; the count of a run follows when it ends.
func TestCollapser(t *testing.T) {
	var buf bytes.Buffer
	c := NewCollapser(&buf)
	c.Line("a")
	if got, want := buf.String(), "a\n"; got != want {
		t.Errorf("after one line: got %q, want %q", got, want)
	}
	c.Line("a")
	c.Line("a")
	c.Line("b")
	c.Flush()
	c.Flush() // a second flush has nothing to write
	if got, want := buf.String(), "a\na (x3)\nb\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCollapserSingleLine(t *testing.T) {
	var buf bytes.Buffer
	c := NewCollapser(&buf)
	c.Line("one")
	c.Line("one")
	c.Flush()
	if got, want := buf.String(), "one\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCollapserIgnoresEmpty(t *testing.T) {
	var buf bytes.Buffer
	c := NewCollapser(&buf)
	c.Line("")
	c.Line("")
	c.Flush()
	if buf.Len() != 0 {
		t.Errorf("wrote %q for empty lines", buf.String())
	}
}

// The prober traces from one goroutine per node while the command polls; the
// collapser must not lose or interleave a line.
func TestCollapserConcurrent(t *testing.T) {
	var buf bytes.Buffer
	c := NewCollapser(&buf)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				c.Line("x")
			}
		}()
	}
	wg.Wait()
	c.Flush()
	if got, want := buf.String(), "x\nx (x800)\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSink(t *testing.T) {
	var buf bytes.Buffer
	Line("ignored while off")
	if Enabled() {
		t.Fatal("Enabled with no sink")
	}
	Enable(&buf)
	if !Enabled() {
		t.Fatal("not Enabled after Enable")
	}
	Logf("→ %s %d", "get pods", 3)
	Flush()
	Disable()
	if Enabled() {
		t.Fatal("Enabled after Disable")
	}
	if got, want := buf.String(), "→ get pods 3\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if !strings.HasPrefix(buf.String(), "→ ") {
		t.Error("line lost its arrow")
	}
}
