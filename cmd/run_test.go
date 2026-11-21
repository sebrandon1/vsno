package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/run"
)

// Each stage shows whether it ran, failed, or an earlier run finished it.
func TestRenderRun(t *testing.T) {
	res := &run.Result{
		Plan:    run.Plan{Stages: []run.Stage{run.StageSetup, run.StageVSNOs, run.StageSeed}, Done: []run.Stage{run.StageSetup}},
		Timings: map[run.Stage]string{run.StageSetup: "26s", run.StageVSNOs: "1h0m5s"},
		Failed:  run.StageVSNOs,
	}
	var b bytes.Buffer
	renderRun(&b, res, plain)
	out := b.String()
	for _, want := range []string{"✓  setup  26s (resumed)", "✗  vsnos  1h0m5s failed", "=  seed   not run"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
