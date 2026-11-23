package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/sebrandon1/vsno/lib/trace"
)

// -o wide is accepted by every command but only a few have a wide view; the
// rest must say so instead of quietly printing the table.
func TestCheckWide(t *testing.T) {
	old := outputFormat
	t.Cleanup(func() { outputFormat = old })

	for _, c := range []struct {
		args    []string
		format  string
		wantErr bool
	}{
		{[]string{"list"}, formatWide, false},
		{[]string{"iso"}, formatWide, false},
		{[]string{"iso", "list"}, formatWide, false},
		{[]string{"status"}, formatWide, true},
		{[]string{"cache", "list"}, formatWide, true},
		{[]string{"status"}, formatTable, false},
		{[]string{"status"}, formatJSON, false},
	} {
		var cmd *cobra.Command
		var err error
		if cmd, _, err = rootCmd.Find(c.args); err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		outputFormat = c.format
		err = checkWide(cmd)
		switch {
		case c.wantErr && err == nil:
			t.Errorf("%v -o %s: want an error", c.args, c.format)
		case c.wantErr && (!strings.Contains(err.Error(), "no wide output") || !strings.Contains(err.Error(), cmd.CommandPath())):
			t.Errorf("%v -o %s: error = %v, want it to name the command", c.args, c.format, err)
		case !c.wantErr && err != nil:
			t.Errorf("%v -o %s: %v, want nil", c.args, c.format, err)
		}
	}
}

// -v used to print the version. It is verbose now, and cobra registers
// --version without a shorthand because -v is taken.
func TestVerboseFlag(t *testing.T) {
	f := rootCmd.PersistentFlags().Lookup("verbose")
	if f == nil {
		t.Fatal("no --verbose flag")
	}
	if f.Shorthand != "v" {
		t.Errorf("--verbose shorthand = %q, want v", f.Shorthand)
	}

	old := rootCmd.Version
	rootCmd.Version = "test"
	t.Cleanup(func() { rootCmd.Version = old })
	rootCmd.InitDefaultVersionFlag()
	v := rootCmd.Flags().Lookup("version")
	if v == nil {
		t.Fatal("no --version flag")
	}
	if v.Shorthand != "" {
		t.Errorf("--version kept shorthand %q, want none", v.Shorthand)
	}
}

func TestVerboseEnablesTrace(t *testing.T) {
	oldVerbose, oldFormat := verbose, outputFormat
	t.Cleanup(func() {
		verbose, outputFormat = oldVerbose, oldFormat
		rootCmd.SetErr(nil)
		trace.Disable()
	})

	var buf bytes.Buffer
	rootCmd.SetErr(&buf)
	verbose, outputFormat = true, formatTable
	if err := rootCmd.PersistentPreRunE(rootCmd, nil); err != nil {
		t.Fatalf("PersistentPreRunE: %v", err)
	}
	if !trace.Enabled() {
		t.Fatal("--verbose did not turn the trace on")
	}
	trace.Line("→ get pods -n vsno")
	trace.Flush()
	if !strings.Contains(buf.String(), "→ get pods -n vsno") {
		t.Errorf("trace = %q, want the line on stderr", buf.String())
	}

	verbose = false
	if err := rootCmd.PersistentPreRunE(rootCmd, nil); err != nil {
		t.Fatalf("PersistentPreRunE: %v", err)
	}
	if trace.Enabled() {
		t.Error("the trace stayed on without --verbose")
	}
}

// Commands that hand over a stream or only say they are done have nothing to
// put in -o json, so they refuse it rather than printing human text anyway.
func TestCheckJSON(t *testing.T) {
	old := outputFormat
	t.Cleanup(func() { outputFormat = old })

	for _, c := range []struct {
		args    []string
		format  string
		wantErr bool
	}{
		{[]string{"oc"}, formatJSON, true},
		{[]string{"ssh"}, formatJSON, true},
		{[]string{"kubeconfig"}, formatJSON, true},
		{[]string{"screenshot"}, formatJSON, true},
		{[]string{"iso", "eject"}, formatJSON, true},
		{[]string{"cache", "gc"}, formatJSON, false},
		{[]string{"list"}, formatJSON, false},
		{[]string{"version"}, formatJSON, false},
		{[]string{"oc"}, formatTable, false},
	} {
		var cmd *cobra.Command
		var err error
		if cmd, _, err = rootCmd.Find(c.args); err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		outputFormat = c.format
		err = checkJSON(cmd)
		switch {
		case c.wantErr && err == nil:
			t.Errorf("%v -o %s: want an error", c.args, c.format)
		case c.wantErr && !strings.Contains(err.Error(), cmd.CommandPath()):
			t.Errorf("%v -o %s: error = %v, want it to name the command", c.args, c.format, err)
		case !c.wantErr && err != nil:
			t.Errorf("%v -o %s: %v, want nil", c.args, c.format, err)
		}
	}
}
