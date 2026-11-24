package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/ui"
	"github.com/spf13/cobra"
)

// watchLoop draws until the context ends, and stops on a draw error.
func TestWatchLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	draws := 0
	var b strings.Builder
	done := make(chan error, 1)
	go func() {
		done <- watchLoop(cmd, &b, ui.Palette{On: true}, time.Millisecond, func() error {
			draws++
			if draws == 3 {
				cancel()
			}
			return nil
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not stop when the context did")
	}
	if draws < 3 {
		t.Errorf("drew %d times", draws)
	}
	if !strings.Contains(b.String(), "\033[H\033[2J") {
		t.Error("it did not clear the screen on a terminal")
	}

	// A draw error ends it.
	cmd.SetContext(context.Background())
	if err := watchLoop(cmd, &b, ui.Palette{}, time.Millisecond, func() error { return errors.New("boom") }); err == nil {
		t.Error("a draw error was swallowed")
	}
}

// -w with -o json is refused rather than printing a stream of documents.
func TestCheckWatchRefusesJSON(t *testing.T) {
	t.Cleanup(func() { outputFormat = formatTable })
	c := &cobra.Command{Use: "status"}
	c.Flags().BoolP("watch", "w", false, "")
	if err := c.Flags().Set("watch", "true"); err != nil {
		t.Fatal(err)
	}
	outputFormat = formatJSON
	if err := checkWatch(c); err == nil || !strings.Contains(err.Error(), "drop -o json") {
		t.Errorf("err = %v", err)
	}
	outputFormat = formatTable
	if err := checkWatch(c); err != nil {
		t.Errorf("table + watch: %v", err)
	}
}
