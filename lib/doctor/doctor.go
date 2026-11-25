// Package doctor looks for the states that make a vSNO or an environment look
// broken without saying why: an ImageBasedUpgrade stuck between stages, a seed
// generation nothing is following, a guest whose services a half-finished
// generation left off, a Released volume still holding a disk, and a registry
// too full for the next seed. It reads; Fixes only run under --fix, and only
// the two that are safe to run unattended.
package doctor

import (
	"context"

	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/seed"
)

// Finding is one state worth knowing about, with the action that clears it.
type Finding struct {
	precheck.Result
	// VSNo is the vSNO it is about, "" for the environment's own checks.
	VSNo string `json:"vsno,omitempty"`
	// Fix is what clears it, as a command the user can run.
	Fix string `json:"fix,omitempty"`
	// Action is what `--fix` may do about it. Empty means the Fix is only ever
	// a command for the user.
	Action Action `json:"-"`
}

// Env is what the checks read: the seed environment has the cluster, oc/ssh to
// a vSNO, and the registry's volume.
type Env struct {
	seed.Env
	// Guests lists the vSNOs to check; nil uses guest.List.
	Guests func(ctx context.Context, ns string) ([]guest.Guest, error)
}

func (e Env) guests(ctx context.Context, ns string) ([]guest.Guest, error) {
	if e.Guests != nil {
		return e.Guests(ctx, ns)
	}
	return guest.List(ctx, e.Env.Env, ns)
}

// Run checks every vSNO it can name, and then the environment. names empty
// means every vSNO in the namespace.
func Run(ctx context.Context, e Env, ns string, names []string) ([]Finding, error) {
	// Access and the registry reader come from Defaults when the caller did
	// not supply them: a nil Access is a panic waiting for the first check.
	e.Defaults(ns)
	all, err := e.guests(ctx, ns)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, g := range all {
		if len(names) > 0 && !contains(names, g.Name) {
			continue
		}
		// A stopped vSNO answers nothing by design; its checks would only say
		// "couldn't be read".
		if g.State == guest.Stopped || g.State == guest.Deleting {
			continue
		}
		a := e.Access(g.Name)
		for _, check := range []func(context.Context, seed.Access, string) ([]Finding, error){
			stuckUpgrade, followingSeedGen, seedGenServices,
		} {
			f, err := check(ctx, a, g.Name)
			if err != nil {
				out = append(out, unreadable(g.Name, err))
				continue
			}
			out = append(out, f...)
		}
	}
	if len(names) == 0 {
		out = append(out, Environment(ctx, e, ns)...)
	}
	return out, nil
}

// Environment reports the environment's own checks rather than its vSNOs':
// Released volumes still holding a disk, and a registry too full for the next
// seed. It only reads, and reconcile reuses it so the two agree.
func Environment(ctx context.Context, e Env, ns string) []Finding {
	e.Defaults(ns)
	var out []Finding
	leaks, err := volumeLeaks(ctx, e, ns)
	if err != nil {
		out = append(out, unreadable("the environment", err))
	}
	out = append(out, leaks...)
	return append(out, registryRoom(ctx, e, ns)...)
}

func contains(all []string, s string) bool {
	for _, a := range all {
		if a == s {
			return true
		}
	}
	return false
}

func unreadable(what string, err error) Finding {
	return Finding{Result: precheck.Result{Group: "doctor", Check: what, Status: precheck.Warn,
		Detail: "couldn't be read: " + err.Error()}}
}
