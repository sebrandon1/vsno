package cmd

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/seed"
)

// start passes true and stop passes false; the polarity picks the verb shown and
// the states to wait for. A start also stops waiting on making-seed: the guest
// answers nothing then, and runPower says which case it is.
func TestStopRefusal(t *testing.T) {
	err := stopRefusal("seed2", &seed.SeedGen{Message: "Pull recert image"})
	for _, want := range []string{"seed2 is making a seed image (Pull recert image)", "crio and kubelet disabled", "`vsno list` follows it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
	// With the guest's API down, the mark's age is what names the generation.
	err = stopRefusal("seed2", &seed.SeedGen{Age: 4 * time.Minute})
	if !strings.Contains(err.Error(), "(started 4m0s ago)") {
		t.Errorf("err = %v, want the mark's age", err)
	}
}

func TestStartRecovery(t *testing.T) {
	err := startRecovery("seed2")
	for _, want := range []string{"crio and kubelet are disabled", "vsno doctor seed2 --fix",
		"vsno ssh seed2 -- 'sudo systemctl enable --now crio kubelet'"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
}

func TestPowerWait(t *testing.T) {
	verb, want := powerWait(true)
	if verb != "starting" || !slices.Equal(want, []guest.State{guest.Ready, guest.Failed, guest.MakingSeed}) {
		t.Errorf("start = %q %v, want starting and ready/failed/making-seed", verb, want)
	}

	verb, want = powerWait(false)
	if verb != "stopping" || !slices.Equal(want, []guest.State{guest.Stopped}) {
		t.Errorf("stop = %q %v, want stopping and stopped", verb, want)
	}
}
