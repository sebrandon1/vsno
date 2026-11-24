package cmd

import (
	"fmt"
	"io"
	"time"

	"github.com/sebrandon1/vsno/lib/ui"
	"github.com/spf13/cobra"
)

// watchLoop redraws until the command's context ends: it clears the screen
// before each draw on a terminal, then waits for the interval. Every command
// with -w/--watch goes through it, so they behave the same.
func watchLoop(cmd *cobra.Command, w io.Writer, pal ui.Palette, every time.Duration, draw func() error) error {
	for {
		if pal.On {
			fmt.Fprint(w, "\033[H\033[2J") // clear the screen between refreshes
		}
		if err := draw(); err != nil {
			return err
		}
		select {
		case <-cmd.Context().Done():
			return nil
		case <-time.After(every):
		}
	}
}

// checkWatch refuses -w with -o json: a watch prints the report again and
// again, and a stream of JSON documents is not what a consumer wants.
func checkWatch(cmd *cobra.Command) error {
	if outputFormat != formatJSON || !cmd.Flags().Changed("watch") {
		return nil
	}
	return fmt.Errorf("%s: --watch redraws the report; drop -o json", cmd.CommandPath())
}
