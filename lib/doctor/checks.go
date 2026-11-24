package doctor

import (
	"context"
	"fmt"
	"strings"

	"github.com/sebrandon1/vsno/lib/guestctl"
	"github.com/sebrandon1/vsno/lib/ibu"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/seed"
)

// stuckUpgrade finds an ImageBasedUpgrade whose Idle condition is False with a
// reason LCA does not handle: nothing will move it, and `--abort` waits out its
// timeout. ibu.StuckIdle is the one definition of that state, so doctor and
// upgrade cannot disagree; a Preps that is merely running is not one.
func stuckUpgrade(ctx context.Context, a seed.Access, name string) ([]Finding, error) {
	var u ibu.IBU
	found, err := guestctl.Get(ctx, a, &u, "get", "imagebasedupgrades", "upgrade", "-n", seed.LCANamespace, "--ignore-not-found")
	if err != nil || !found {
		return nil, err
	}
	c, stuck := u.StuckIdle()
	if !stuck {
		return nil, nil
	}
	return []Finding{{
		VSNo: name,
		Result: precheck.Result{Group: "doctor", Check: name + ": ImageBasedUpgrade",
			Status: precheck.Fail,
			Detail: fmt.Sprintf("stage %q, Idle condition %q with reason %q, which LCA does not act on", orIdle(string(u.Spec.Stage)), c.Status, c.Reason),
			Hint:   "`vsno upgrade <target> --abort` will not clear this"},
		Fix:    "the status patch this command would apply, or restart the controller",
		Action: UnstickIdle,
	}}, nil
}

// followingSeedGen finds a SeedGenerator LCA is running that no vsno process is
// following: interrupted `seed create` left it, and it still finishes.
func followingSeedGen(ctx context.Context, a seed.Access, name string) ([]Finding, error) {
	var sg struct {
		Status struct {
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	}
	found, err := guestctl.Get(ctx, a, &sg, "get", "seedgenerators", "seedimage", "-n", seed.LCANamespace, "--ignore-not-found")
	if err != nil || !found {
		return nil, err
	}
	running := false
	var msg string
	for _, c := range sg.Status.Conditions {
		if c.Type == "SeedGenCompleted" && c.Status == "True" {
			return nil, nil // done; finish cleanup is all that is left
		}
		if c.Type == "SeedGenInProgress" {
			running = true
			msg = c.Message
		}
	}
	if !running {
		return nil, nil
	}
	return []Finding{{
		VSNo: name,
		Result: precheck.Result{Group: "doctor", Check: name + ": seed generation",
			Status: precheck.Warn,
			Detail: "LCA is generating a seed image (" + msg + "); no vsno process is following it",
			Hint:   "let it finish, then `vsno seed list`; do not run `seed create` again"},
	}}, nil
}

// seedGenServices finds the guest of a half-finished seed generation: LCA
// stopped crio and kubelet and nothing turned them back on, so the vSNO never
// answers after a start.
func seedGenServices(ctx context.Context, a seed.Access, name string) ([]Finding, error) {
	out, err := guestctl.SSHOut(ctx, a, "test", "-d", "/var/lib/lca/ibu-seedgen-orch", "&&", "echo", "orch", ";", "systemctl", "is-enabled", "kubelet", "crio")
	if err != nil {
		return nil, err
	}
	disabled := strings.Contains(out, "disabled")
	if !disabled || !strings.Contains(out, "orch") {
		return nil, nil
	}
	return []Finding{{
		VSNo: name,
		Result: precheck.Result{Group: "doctor", Check: name + ": guest services",
			Status: precheck.Fail,
			Detail: "a seed generation was interrupted: kubelet and crio are disabled, so the vSNO will not answer",
			Hint:   "`vsno ssh " + name + " -- 'sudo systemctl enable --now crio kubelet'`"},
		Fix:    "`vsno ssh " + name + " -- 'sudo systemctl enable --now crio kubelet'`",
		Action: EnableServices,
	}}, nil
}

// orIdle names a stage, the unset one being Idle.
func orIdle(s string) string {
	if s == "" {
		return "Idle"
	}
	return s
}
