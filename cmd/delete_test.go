package cmd

import (
	"slices"
	"testing"

	"github.com/sebrandon1/vsno/lib/guest"
)

// start passes true and stop passes false; the polarity picks the verb shown and
// the states to wait for.
func TestPowerWait(t *testing.T) {
	verb, want := powerWait(true)
	if verb != "starting" || !slices.Equal(want, []guest.State{guest.Ready, guest.Failed}) {
		t.Errorf("start = %q %v, want starting and ready/failed", verb, want)
	}

	verb, want = powerWait(false)
	if verb != "stopping" || !slices.Equal(want, []guest.State{guest.Stopped}) {
		t.Errorf("stop = %q %v, want stopping and stopped", verb, want)
	}
}
