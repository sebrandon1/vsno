package cmd

import "testing"

// newProgress is the precheck progress sink: it must work with color on and off
// (JSON output turns it off), without touching the terminal state.
func TestProgressStart(t *testing.T) {
	for _, format := range []string{formatTable, formatJSON} {
		old := outputFormat
		outputFormat = format
		p := newProgress()
		step := p.Start("doing it")
		step.Update("half way")
		step.Done("did it")
		// precheckProgress is what precheck drives; it must answer the same way.
		adapter := precheckProgress{p: p}.Start("prechecking")
		adapter.Update("half way")
		adapter.Done("prechecked")
		outputFormat = old
	}
}
