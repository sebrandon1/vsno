package doctor

import (
	"bytes"
	"context"
	"fmt"

	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/guestctl"
	"github.com/sebrandon1/vsno/lib/seed"
)

// Action is what --fix may do. Only reversible, single-purpose actions are
// here; everything else a Finding carries is a command for the user.
type Action string

const (
	// EnableServices turns crio and kubelet back on after an interrupted seed
	// generation. It changes no object and deletes nothing.
	EnableServices Action = "enable-services"
	// UnstickIdle points LCA's stuck Idle condition at the reason its controller
	// acts on. The spec is untouched; a status subresource patch only.
	UnstickIdle Action = "unstick-idle"
)

// Fix runs f.Action. A Finding without one is not fixable here.
func Fix(ctx context.Context, a seed.Access, f Finding) error {
	var out, stderr bytes.Buffer
	s := builder.IO{Stdout: &out, Stderr: &stderr}
	switch f.Action {
	case EnableServices:
		if err := a.SSH(ctx, []string{"sudo", "systemctl", "enable", "--now", "crio", "kubelet"}, s); err != nil {
			return fmt.Errorf("enabling crio and kubelet: %w%s", err, builder.Tail(stderr.String()))
		}
		return nil
	case UnstickIdle:
		var u ibu
		found, err := guestctl.Get(ctx, a, &u, "get", "imagebasedupgrades", "upgrade", "-n", seed.LCANamespace, "--ignore-not-found")
		if err != nil || !found {
			return err
		}
		for i, c := range u.Status.Conditions {
			if c.Type == "Idle" && c.Status != "True" && !contains(handledReasons, c.Reason) {
				patch := fmt.Sprintf(`[{"op":"replace","path":"/status/conditions/%d/reason","value":"Aborting"}]`, i)
				if err := a.Oc(ctx, []string{"patch", "imagebasedupgrades", "upgrade", "-n", seed.LCANamespace,
					"--subresource=status", "--type=json", "-p", patch}, s); err != nil {
					return fmt.Errorf("unsticking the Idle condition: %w%s", err, builder.Tail(stderr.String()))
				}
				return nil
			}
		}
		return nil
	}
	return fmt.Errorf("%s has no automatic fix; run the command it names", f.Check)
}
