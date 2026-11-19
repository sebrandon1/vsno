package cmd

import (
	"os"

	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/ui"
)

// newProgress shows step progress on stderr for table output; JSON output stays clean.
func newProgress() *ui.Progress {
	return ui.New(os.Stderr, outputFormat == formatTable)
}

// precheckProgress adapts ui.Progress to precheck.Progress.
type precheckProgress struct{ p *ui.Progress }

func (a precheckProgress) Start(title string) precheck.ProgressStep { return a.p.Start(title) }
