package seed

import (
	"context"

	"github.com/sebrandon1/vsno/lib/guestctl"
	"github.com/sebrandon1/vsno/lib/precheck"
)

// OADPNamespace is where OADP runs on the vSNO.
const OADPNamespace = "openshift-adp"

// OADP is the OpenShift API for Data Protection, which LCA backs a cluster up
// with before an upgrade and restores it from after. LCA's prerequisites have
// the operator installed on the seed as well as the target (lifecycle-agent's
// docs/backuprestore-with-oadp.md); only its DataProtectionApplication is
// target-only, and `vsno prepare` adds that.
var OADP = guestctl.Operator{Name: "OADP", Package: "redhat-oadp-operator", CSVPrefix: "oadp-operator.", Namespace: OADPNamespace}

// PrepOptions are what to ready on a vSNO, and what to leave alone.
type PrepOptions struct {
	// Prefix starts each step's title, naming the vSNO when the command
	// readies several ("target: ").
	Prefix string
	// Registry is the environment's registry, whose CA the vSNO trusts so it
	// can push its seed image there.
	Registry Registry
	// NoTrust leaves trust alone: the seed image goes to another registry.
	NoTrust bool
	// NoLCA leaves the Lifecycle Agent alone: the caller installs it its own
	// way (vsno prepare --lca-ref).
	NoLCA bool
}

// Ready is what readying a vSNO left running.
type Ready struct {
	// LCA is the CSV it runs, or the image of an LCA deployed from a ref.
	LCA string
	// OADP is the CSV it runs.
	OADP string
	// Trusted is whether it trusts the environment's registry.
	Trusted bool
}

// EnsureReady readies vSNO a for an Image Based Upgrade in either direction:
// the Lifecycle Agent from its own catalog, OADP, and trust in the
// environment's registry. It is the one definition of what that takes —
// `seed precheck --install`, `seed create` and `vsno prepare` all run it — so
// the commands cannot disagree about what a vSNO needs.
func EnsureReady(ctx context.Context, e Env, a Access, o PrepOptions) (Ready, error) {
	var r Ready
	if !o.NoLCA {
		if err := guestctl.Step(e.Progress, o.Prefix+"the Lifecycle Agent", func(ps precheck.ProgressStep) (string, error) {
			var err error
			r.LCA, err = ensureLCA(ctx, e, a, ps)
			return r.LCA, err
		}); err != nil {
			return r, err
		}
	}
	if err := guestctl.Step(e.Progress, o.Prefix+OADP.Name, func(ps precheck.ProgressStep) (string, error) {
		var err error
		r.OADP, err = OADP.Ensure(ctx, a, e.Clock(), operatorWait, ps)
		return r.OADP, err
	}); err != nil {
		return r, err
	}
	if o.NoTrust {
		return r, nil
	}
	err := guestctl.Step(e.Progress, o.Prefix+"trusting the environment registry", func(ps precheck.ProgressStep) (string, error) {
		return o.Registry.Host, EnsureTrust(ctx, e, a, o.Registry, ps)
	})
	r.Trusted = err == nil
	return r, err
}
