package cmd

import (
	"bytes"
	"os"
	"path/filepath"
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

// A settings file gives the options, and a flag given on the command line
// wins over the file's value for that key only.
func TestRunOptions(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "vsno.yaml")
	if err := os.WriteFile(file, []byte("targetName: t1\ndisk: /dev/disk/by-id/a\nfinalize: true\nlca:\n  ref: release-4.21\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := runCmd.Flags()
	t.Cleanup(func() {
		rnFile, rnOpts = "", run.Options{}
		for _, name := range []string{"disk", "seed-name"} {
			_ = f.Set(name, "")
			f.Lookup(name).Changed = false
		}
	})
	rnFile = file
	if err := f.Set("disk", "/dev/disk/by-id/b"); err != nil {
		t.Fatal(err)
	}
	if err := f.Set("seed-name", "s1"); err != nil {
		t.Fatal(err)
	}
	o, err := runOptions(runCmd)
	if err != nil {
		t.Fatal(err)
	}
	if o.TargetName != "t1" || !o.Finalize || o.LCA.Ref != "release-4.21" || o.Disk != "/dev/disk/by-id/b" || o.SeedName != "s1" || o.Namespace != namespace {
		t.Errorf("options = %+v", o)
	}

	if err := os.WriteFile(file, []byte("targetNmae: typo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runOptions(runCmd); err == nil || !strings.Contains(err.Error(), "parsing") {
		t.Errorf("unknown key: %v", err)
	}
}
