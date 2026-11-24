package cmd

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/guestctl/guestctltest"
	"github.com/sebrandon1/vsno/lib/seed"
	"github.com/spf13/cobra"
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

// A start that ends without the guest answering either says why, or names the
// seed generation it found.
func TestStartOutcome(t *testing.T) {
	const probe = "test -d /var/lib/lca/ibu-seedgen-orch && echo orch ; systemctl is-enabled kubelet crio"
	broken := &guestctltest.Fake{SSHReplies: map[string][]guestctltest.Reply{probe: {{Out: "orch\ndisabled\ndisabled\n"}}}}
	healthy := &guestctltest.Fake{SSHReplies: map[string][]guestctltest.Reply{probe: {{Out: "orch\nenabled\nenabled\n"}}}}
	timeout := errors.New("timed out")
	for _, c := range []struct {
		name string
		fa   *guestctltest.Fake
		st   guest.State
		err  error
		want string // "" = the error comes back unchanged (nil here)
		note bool
	}{
		{"failed", healthy, guest.Failed, nil, "failed to start", false},
		{"making-seed, interrupted", broken, guest.MakingSeed, nil, "crio and kubelet are disabled", false},
		{"making-seed, LCA generating", healthy, guest.MakingSeed, nil, "", true},
		{"timed out, interrupted", broken, guest.Unknown, timeout, "crio and kubelet are disabled", false},
		{"timed out, not interrupted", healthy, guest.Unknown, timeout, "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var stderr strings.Builder
			cmd := &cobra.Command{}
			cmd.SetErr(&stderr)
			se := seed.Env{Access: func(string) seed.Access { return c.fa }}
			err := startOutcome(cmd, se, "seed2", c.st, c.err)
			switch {
			case c.want == "" && c.err == nil && err != nil:
				t.Fatalf("err = %v, want none", err)
			case c.want == "" && c.err != nil && err != c.err:
				t.Fatalf("err = %v, want the wait's own", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if got := stderr.String() != ""; got != c.note {
				t.Errorf("note = %q, want one = %v", stderr.String(), c.note)
			}
		})
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
