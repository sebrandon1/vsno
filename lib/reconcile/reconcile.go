// Package reconcile is the level-triggered sweep behind `vsno reconcile`: it
// settles bookkeeping that finished while nothing was watching, and reports or
// removes what a create that died left behind. Every action derives from what
// the cluster holds now, so a rerun is a no-op, and nothing changes unless
// Options.Prune says so.
package reconcile

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/cache"
	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/doctor"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/manifests"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/seed"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Action is what the sweep did, or would do, about an item.
type Action string

const (
	Observe Action = "report" // found, not changed
	Settle  Action = "settle" // recorded the end of a finished mirror
	Delete  Action = "delete" // removed an abandoned create's parts
)

// The group and kinds a report's rows carry. They are also the words a future
// controller would turn into condition types.
const (
	groupName     = "reconcile"
	kindMirror    = "cache mirror"
	kindAbandoned = "abandoned vSNO"
	kindDNS       = "DNS entry"
	kindSeedMark  = "seed mark"
	kindEnv       = "environment"
	checkLease    = "reconcile lease"
)

// Item is one thing the sweep found, shaped like a precheck result so a future
// controller can turn it into a condition without translating it.
type Item struct {
	precheck.Result
	Kind   string `json:"kind"`
	Name   string `json:"name,omitempty"`
	Action Action `json:"action"`
	Done   bool   `json:"done,omitempty"`
	// Error is set when a read failed or a --prune action did. It is what makes
	// the command exit non-zero: a finding alone does not.
	Error string `json:"error,omitempty"`
}

// Report is what `vsno reconcile` shows.
type Report struct {
	Namespace string `json:"namespace"`
	Pruned    bool   `json:"pruned"`
	Items     []Item `json:"items"`
}

// Failed reports whether the sweep could not read something or could not do
// what --prune planned.
func (r *Report) Failed() bool {
	return slices.ContainsFunc(r.Items, func(it Item) bool { return it.Error != "" })
}

// Options are the user's choices for a sweep.
type Options struct {
	Namespace string
	// Names restricts the vSNO-level items; empty means every one.
	Names []string
	// Prune performs the mutations. Without it the sweep only reports.
	Prune bool
	// Grace is how long a Service may wait for its VM before it counts as
	// abandoned; 0 uses guest.AbandonGrace().
	Grace time.Duration
	// Timeout bounds the whole sweep; 0 means five minutes.
	Timeout time.Duration
}

// Env is what the sweep reads and changes.
type Env struct {
	seed.Env // the cluster, the builder and oc/ssh to a vSNO
	Cache    cache.Env
}

// Sweep runs the checks in order and reports what it found.
func Sweep(ctx context.Context, e Env, o Options) (*Report, error) {
	if o.Grace == 0 {
		o.Grace = guest.AbandonGrace()
	}
	if o.Timeout == 0 {
		o.Timeout = 5 * time.Minute
	}
	// A sweep runs from cron: it must never draw a spinner or ask anything.
	e.Progress = nil
	e.Defaults(o.Namespace)

	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()

	if _, err := e.Kube.CoreV1().Namespaces().Get(ctx, o.Namespace, metav1.GetOptions{}); err != nil {
		return nil, fmt.Errorf("reading namespace %s: %w", o.Namespace, err)
	}

	rep := &Report{Namespace: o.Namespace, Pruned: o.Prune}
	if o.Prune {
		// Only a sweep that changes something takes the lock: a report-only run
		// must not write to the cluster, not even a Lease.
		release, blocked, warning := acquire(ctx, e.Kube, o.Namespace, e.Now())
		if release != nil {
			defer release()
		}
		if blocked != nil {
			rep.Items = []Item{*blocked}
			return rep, nil
		}
		if warning != nil {
			rep.Items = append(rep.Items, *warning)
		}
	}
	for _, step := range []func(context.Context, Env, Options) []Item{
		settleMirrors,
		abandonedServices,
		orphanDNS,
		staleSeedMarks,
		environment,
	} {
		rep.Items = append(rep.Items, step(ctx, e, o)...)
	}
	if rep.Items == nil {
		rep.Items = []Item{}
	}
	return rep, nil
}

// settleMirrors records the end of mirrors whose Jobs finished while no
// command was watching. `cache list` does the same; this is the unattended
// version.
func settleMirrors(ctx context.Context, e Env, o Options) []Item {
	pending, err := cache.Pending(ctx, e.Kube, o.Namespace)
	if err != nil {
		return []Item{readFailed(kindMirror, "reading the cache's records", err)}
	}
	if len(pending) == 0 {
		return nil
	}
	if !o.Prune {
		out := make([]Item, 0, len(pending))
		for _, ent := range pending {
			out = append(out, mirrorItem(ent, false, false, ""))
		}
		return out
	}
	rerr := cache.Reconcile(ctx, e.Cache, o.Namespace)
	after, err := cache.Pending(ctx, e.Kube, o.Namespace)
	if err != nil {
		return []Item{readFailed(kindMirror, "reading the cache's records", err)}
	}
	left := map[string]bool{}
	for _, ent := range after {
		left[ent.Digest] = true
	}
	out := make([]Item, 0, len(pending))
	for _, ent := range pending {
		switch {
		case !left[ent.Digest]:
			out = append(out, mirrorItem(ent, true, true, ""))
		case rerr != nil:
			out = append(out, mirrorItem(ent, true, false, rerr.Error()))
		default:
			out = append(out, mirrorItem(ent, true, false, ""))
		}
	}
	return out
}

func mirrorItem(ent cache.Entry, prune, done bool, errMsg string) Item {
	name := ent.Version
	if name == "" {
		name = ent.Digest
	}
	it := Item{
		Kind: kindMirror, Name: name, Action: Observe, Done: done,
		Result: precheck.Result{Group: groupName, Check: kindMirror, Status: precheck.Info,
			Detail: "a mirror is still running",
			Hint:   "`vsno cache list` settles a finished mirror too"},
	}
	if prune {
		it.Action = Settle
		it.Detail = "still running"
		if done {
			it.Detail = "settled: the mirror finished while nothing was watching"
		}
	}
	if errMsg != "" {
		it.Error = errMsg
		it.Status, it.Detail = precheck.Warn, "the mirror could not be settled: "+errMsg
	}
	return it
}

// abandonedServices removes what a create that died after making its Service
// left: no VM, and older than any ISO build.
func abandonedServices(ctx context.Context, e Env, o Options) []Item {
	names, err := guest.AbandonedServices(ctx, e.Env.Env, o.Namespace, o.Grace, o.Names)
	if err != nil {
		return []Item{readFailed(kindAbandoned, "listing abandoned vSNOs", err)}
	}
	if len(names) == 0 {
		return nil
	}
	ready, why := builderReady(ctx, e, o.Namespace)
	out := make([]Item, 0, len(names))
	for _, name := range names {
		it := Item{
			Kind: kindAbandoned, Name: name, Action: Observe,
			Result: precheck.Result{Group: groupName, Check: kindAbandoned, Status: precheck.Warn,
				Detail: "a Service with no VM, older than " + o.Grace.String(),
				Hint:   "`vsno reconcile --prune` removes it, with its DNS entry and builder files"},
		}
		if o.Prune {
			it.Action = Delete
			switch {
			case !ready:
				it.Detail += "; not removed: " + why
			default:
				ge := e.Env.Env
				ge.Progress = precheck.NoProgress()
				// The default 90m wait is for an install; a Service has nothing
				// to wait for.
				ge.Timeout = 2 * time.Minute
				if _, derr := guest.Delete(ctx, ge, guest.DeleteOptions{Namespace: o.Namespace, Names: []string{name}, Yes: true}); derr != nil {
					it.Error, it.Detail = derr.Error(), "the delete failed: "+derr.Error()
				} else {
					it.Done, it.Status, it.Detail = true, precheck.Info, "removed"
				}
			}
		}
		out = append(out, it)
	}
	return out
}

// orphanDNS removes dnsmasq entries for names that are neither a VM nor a
// Service. create writes the Service before the DNS entry, so an entry with no
// Service cannot belong to a create that is still running.
func orphanDNS(ctx context.Context, e Env, o Options) []Item {
	cm, err := e.Kube.CoreV1().ConfigMaps(o.Namespace).Get(ctx, manifests.GuestsConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return []Item{readFailed(kindDNS, "reading the DNS entries", err)}
	}
	live, err := liveNames(ctx, e, o.Namespace)
	if err != nil {
		return []Item{readFailed(kindDNS, "listing what still answers", err)}
	}
	names := orphanEntries(cm.Data, live, o.Names)
	out := make([]Item, 0, len(names))
	for _, name := range names {
		out = append(out, dnsItem(ctx, e, o, name))
	}
	return out
}

// liveNames is every name that still has a VM or a Service.
func liveNames(ctx context.Context, e Env, ns string) (map[string]bool, error) {
	live := map[string]bool{}
	vms, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("listing vSNOs: %w", err)
	}
	if vms != nil {
		for i := range vms.Items {
			live[vms.Items[i].GetName()] = true
		}
	}
	svcs, err := e.Kube.CoreV1().Services(ns).List(ctx, metav1.ListOptions{LabelSelector: guest.LabelName})
	if err != nil {
		return nil, fmt.Errorf("listing vSNO Services: %w", err)
	}
	for i := range svcs.Items {
		live[svcs.Items[i].Labels[guest.LabelName]] = true
	}
	return live, nil
}

// orphanEntries picks the dnsmasq keys whose name nothing answers to.
func orphanEntries(data map[string]string, live map[string]bool, want []string) []string {
	var names []string
	for key := range data {
		name, ok := strings.CutSuffix(key, ".conf")
		if !ok || name == "" || live[name] {
			continue
		}
		if len(want) > 0 && !slices.Contains(want, name) {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func dnsItem(ctx context.Context, e Env, o Options, name string) Item {
	it := Item{
		Kind: kindDNS, Name: name + ".conf", Action: Observe,
		Result: precheck.Result{Group: groupName, Check: kindDNS, Status: precheck.Warn,
			Detail: "no VM and no Service answers to it",
			Hint:   "`vsno reconcile --prune` removes it"},
	}
	if o.Prune {
		it.Action = Delete
		if derr := guest.SetDNS(ctx, e.Env.Env, o.Namespace, name, false); derr != nil {
			it.Error, it.Detail = derr.Error(), "the delete failed: "+derr.Error()
		} else {
			it.Done, it.Status, it.Detail = true, precheck.Info, "removed"
		}
	}
	return it
}

// staleSeedMarks clears the vsno.io/seeding mark of a generation that ended.
// The mark is what makes `list` say "making-seed" while the guest's API is
// down, so it may only go once the Lifecycle Agent says the generation ended.
func staleSeedMarks(ctx context.Context, e Env, o Options) []Item {
	vms, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(o.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return []Item{readFailed(kindSeedMark, "listing vSNOs", err)}
	}
	if vms == nil {
		return nil
	}
	marked := markedVSNOs(vms, o.Names)
	if len(marked) == 0 {
		return nil
	}
	ready, why := builderReady(ctx, e, o.Namespace)
	out := make([]Item, 0, len(marked))
	for _, name := range marked {
		it := Item{
			Kind: kindSeedMark, Name: name, Action: Observe,
			Result: precheck.Result{Group: groupName, Check: kindSeedMark, Status: precheck.Info,
				Detail: "marked as generating a seed image",
				Hint:   "`vsno reconcile --prune` clears it once the Lifecycle Agent says the generation ended"},
		}
		if o.Prune {
			it.Action = Settle
			if !ready {
				it.Detail += "; not checked: " + why
			} else if cleared, cerr := seed.ClearEndedMark(ctx, e.Env, o.Namespace, name); cerr != nil {
				it.Error, it.Detail = cerr.Error(), "could not be checked: "+cerr.Error()
			} else if cleared {
				it.Done, it.Detail = true, "cleared: the generation ended"
			} else {
				it.Detail = "kept: the generation is still running, or could not be read"
			}
		}
		out = append(out, it)
	}
	return out
}

// markedVSNOs names the vSNOs still carrying a seeding mark.
func markedVSNOs(vms *unstructured.UnstructuredList, want []string) []string {
	var out []string
	for i := range vms.Items {
		name := vms.Items[i].GetName()
		if vms.Items[i].GetAnnotations()[guest.AnnSeeding] == "" {
			continue
		}
		if len(want) > 0 && !slices.Contains(want, name) {
			continue
		}
		out = append(out, name)
	}
	return out
}

// environment reports the environment's own health. It only reads.
func environment(ctx context.Context, e Env, o Options) []Item {
	if len(o.Names) > 0 {
		return nil // the checks are the environment's, not one vSNO's
	}
	findings := doctor.Environment(ctx, doctor.Env{Env: e.Env}, o.Namespace)
	out := make([]Item, 0, len(findings))
	for _, f := range findings {
		out = append(out, Item{Result: f.Result, Kind: kindEnv, Action: Observe})
	}
	return out
}

func builderReady(ctx context.Context, e Env, ns string) (bool, string) {
	if _, err := builder.ReadyPod(ctx, e.Kube, ns); err != nil {
		return false, "the builder pod is not ready"
	}
	return true, ""
}

func readFailed(kind, what string, err error) Item {
	return Item{
		Kind: kind, Action: Observe, Error: err.Error(),
		Result: precheck.Result{Group: groupName, Check: what, Status: precheck.Warn,
			Detail: "couldn't be read: " + err.Error()},
	}
}
