package teardown

import (
	"context"
	"fmt"
	"slices"

	"github.com/sebrandon1/vsno/lib/apply"
	"github.com/sebrandon1/vsno/lib/manifests"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Actions on Result.Objects.
const (
	// Delete means the object exists and teardown will delete it.
	Delete apply.Action = "delete"
	// Absent means setup would create the object, but it doesn't exist.
	Absent apply.Action = "absent"
	// Removed means teardown deleted it.
	Removed apply.Action = "deleted"
)

// Kept is the Outcome of a host step without --host.
const Kept Outcome = "kept"

// Object is one object of a step and what teardown does to it.
type Object struct {
	apply.Change
	Step string `json:"step"`
}

// Step mirrors one of setup's steps (manifests.Steps), in reverse order.
type Step struct {
	Name    string  `json:"name"`
	Outcome Outcome `json:"outcome"`
	Detail  string  `json:"detail"`
}

// planSteps fills res.Steps and res.Objects: setup's steps in reverse, each
// with the objects setup applies in it (in reverse), plus the targets in the
// step setup doesn't apply (CSVs, a Subscription with another name).
func (p *planner) planSteps(ctx context.Context) error {
	cfg := manifests.NamesConfig(p.o.Namespace)
	for i := len(manifests.Steps) - 1; i >= 0; i-- {
		ms := manifests.Steps[i]
		step := Step{Name: ms.Name}
		if ms.Host && !p.o.Host {
			step.Outcome, step.Detail = Kept, "pass --host to remove"
			p.res.Steps = append(p.res.Steps, step)
			continue
		}
		objs, err := ms.Objects(cfg)
		if err != nil {
			return err
		}
		slices.Reverse(objs)
		ch, err := p.exists(ctx, objs)
		if err != nil {
			return err
		}
		ch = append(p.extraTargets(ms.Name, ch), ch...)
		for _, c := range ch {
			p.res.Objects = append(p.res.Objects, Object{Change: c, Step: ms.Name})
		}
		step.Outcome, step.Detail = Planned, PlanDetail(ch)
		if apply.Count(ch, Delete) == 0 {
			step.Outcome = Gone
		}
		p.res.Steps = append(p.res.Steps, step)
	}
	return nil
}

// exists marks each object Delete or Absent. Without a Planner it assumes
// every object exists.
func (p *planner) exists(ctx context.Context, objs []*unstructured.Unstructured) ([]apply.Change, error) {
	var ch []apply.Change
	if p.Planner != nil {
		var err error
		if ch, err = p.Planner.Plan(ctx, objs); err != nil {
			return nil, err
		}
	} else {
		for _, o := range objs {
			ch = append(ch, apply.Change{Kind: o.GetKind(), Namespace: o.GetNamespace(), Name: o.GetName()})
		}
	}
	for i := range ch {
		if ch[i].Action == apply.Create {
			ch[i].Action = Absent
		} else {
			ch[i].Action = Delete
		}
	}
	return ch, nil
}

// extraTargets are the targets of step that aren't among setup's objects.
func (p *planner) extraTargets(step string, setupObjs []apply.Change) []apply.Change {
	var extra []apply.Change
	for _, t := range p.res.Targets {
		c := apply.Change{Kind: t.Kind, Namespace: t.Namespace, Name: t.Name, Action: Delete}
		if t.step == step && !slices.ContainsFunc(setupObjs, func(o apply.Change) bool { return o.Ref() == c.Ref() }) {
			extra = append(extra, c)
		}
	}
	slices.Reverse(extra) // targets are in deletion order; the step lists in setup order, reversed
	return extra
}

// markDeleted records the steps whose targets were all deleted, and their
// objects. "support" has no targets of its own: deleting the namespace deletes it.
func (r *Result) markDeleted() {
	done := map[string]bool{}
	for _, t := range r.Targets {
		prev, seen := done[t.step]
		done[t.step] = (prev || !seen) && t.Outcome != Planned
	}
	done[manifests.StepSupport] = done[manifests.StepNamespace]
	n := map[string]int{}
	for i, o := range r.Objects {
		if o.Action == Delete && done[o.Step] {
			r.Objects[i].Action = Removed
			n[o.Step]++
		}
	}
	for i, s := range r.Steps {
		if s.Outcome == Planned && done[s.Name] {
			r.Steps[i].Outcome, r.Steps[i].Detail = Deleted, fmt.Sprintf("%d deleted", n[s.Name])
		}
	}
}

// Changes returns the objects' changes, for counting.
func (r *Result) Changes() []apply.Change {
	ch := make([]apply.Change, 0, len(r.Objects))
	for _, o := range r.Objects {
		ch = append(ch, o.Change)
	}
	return ch
}

// PlanDetail is "n to delete, n absent", leaving out zeros.
func PlanDetail(ch []apply.Change) string {
	return apply.Tally(ch, "nothing to delete", apply.Label{Action: Delete, Text: "to delete"}, apply.Label{Action: Absent, Text: "absent"})
}
