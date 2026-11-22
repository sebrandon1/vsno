package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
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
