package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		for _, name := range []string{"device", "disk", "seed-name"} {
			_ = f.Set(name, "")
			f.Lookup(name).Changed = false
		}
	})
	rnFile = file
	// --device is the flag now; the file still calls the setting "disk".
	if err := f.Set("device", "/dev/disk/by-id/b"); err != nil {
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
	// A typo is refused with the key it probably meant, not "unknown field".
	_, err = runOptions(runCmd)
	if !hasAll(err, `"targetNmae" is not a setting`, `did you mean "targetName"?`) {
		t.Errorf("unknown key: %v", err)
	}
}

// --device was --disk; a file's "disk" setting is still overridden by either
// spelling, because the precedence is keyed on the flag that was given.
func TestRunSetupTimeout(t *testing.T) {
	f := runCmd.Flags()
	t.Cleanup(func() {
		rnTimeout = 20 * time.Minute
		_ = f.Set("timeout", "20m")
	})
	if err := f.Set("timeout", "45m"); err != nil {
		t.Fatal(err)
	}
	if rnTimeout != 45*time.Minute {
		t.Errorf("timeout = %s, want 45m", rnTimeout)
	}
	if d := f.Lookup("timeout").DefValue; d != "20m0s" {
		t.Errorf("default = %q, want 20m0s", d)
	}
}

func TestRunDeviceFromTheOldFlag(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "vsno.yaml")
	if err := os.WriteFile(file, []byte("disk: /dev/disk/by-id/a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := runCmd.Flags()
	t.Cleanup(func() {
		rnFile, rnOpts = "", run.Options{}
		for _, name := range []string{"device", "disk"} {
			_ = f.Set(name, "")
			f.Lookup(name).Changed = false
		}
	})
	rnFile = file
	if err := f.Set("disk", "/dev/disk/by-id/old"); err != nil {
		t.Fatal(err)
	}
	o, err := runOptions(runCmd)
	if err != nil || o.Disk != "/dev/disk/by-id/old" {
		t.Errorf("options = %+v, err = %v", o, err)
	}
}

// hasAll reports whether err holds every substring.
func hasAll(err error, subs ...string) bool {
	if err == nil {
		return false
	}
	for _, s := range subs {
		if !strings.Contains(err.Error(), s) {
			return false
		}
	}
	return true
}

// The file takes --memory as memory, and --device as device, since those are
// what the help calls them.
func TestRunFileAliases(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "vsno.yaml")
	b := []byte("memory: 20\ndevice: /dev/disk/by-id/a\n")
	if err := os.WriteFile(file, b, 0o600); err != nil {
		t.Fatal(err)
	}
	var o run.Options
	if err := decodeRunFile(file, b, &o); err != nil {
		t.Fatal(err)
	}
	if o.MemoryGiB != 20 || o.Disk != "/dev/disk/by-id/a" {
		t.Errorf("options = %+v", o)
	}
	// The JSON name still wins if both are given.
	var both run.Options
	b2 := []byte("memory: 20\nmemoryGiB: 30\n")
	if err := decodeRunFile(file, b2, &both); err != nil {
		t.Fatal(err)
	}
	if both.MemoryGiB != 30 {
		t.Errorf("both spellings: %+v", both)
	}
}
