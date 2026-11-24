package guest

import (
	"context"
	"fmt"

	"github.com/sebrandon1/vsno/lib/cache"
	"github.com/sebrandon1/vsno/lib/manifests"
	"github.com/sebrandon1/vsno/lib/release"
)

// Plan is what `vsno create --dry-run` reports: the choices create resolved
// and the steps it would take. It changes nothing.
type CreatePlan struct {
	Namespace string          `json:"namespace"`
	Name      string          `json:"name"`
	Node      string          `json:"node"`
	Release   release.Release `json:"release"`
	Cores     int             `json:"cores"`
	MemoryGiB int64           `json:"memoryGiB"`
	DiskGiB   int64           `json:"diskGiB"`
	Snapshot  string          `json:"snapshot,omitempty"`
	NoCache   bool            `json:"noCache,omitempty"`
	Cache     *CacheState     `json:"cache,omitempty"`
	Steps     []PlanStep      `json:"steps"`
}

// CacheState is the release already mirrored in the environment's registry.
// Its presence in a Plan means the cache has the release; no field says
// otherwise, so there is none to fall out of step with Repo.
type CacheState struct {
	Repo string `json:"repo"`
}

// PlanStep is one thing create would do.
type PlanStep struct {
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

// cacheLookup is the read-only half of the cache: whether a release is already
// mirrored, without starting a mirror. It is optional, so an Env whose Cache
// only mirrors keeps working.
type cacheLookup interface {
	Lookup(ctx context.Context, ns string, rel release.Release) (*cache.Entry, error)
}

// Plan resolves what Create would do and changes nothing: no builder command,
// no ISO, no VM. It reads what create reads, so a dry run fails for the same
// reasons create would.
func Plan(ctx context.Context, e Env, o CreateOptions) (*CreatePlan, error) {
	e.defaults()
	if o.Name == "" {
		name, err := FreeName(ctx, e, o.Namespace)
		if err != nil {
			return nil, err
		}
		o.Name = name
	}
	spec, _, _, err := prepare(ctx, e, o)
	if err != nil {
		return nil, err
	}
	var cached *CacheState
	if spec.Snapshot == "" && !o.NoCache {
		cached = cachedRelease(ctx, e, spec)
	}
	return planFor(spec, cached, o), nil
}

// cachedRelease asks the environment's cache whether the release is already
// there. An unreadable cache leaves the plan saying nothing rather than failing
// a dry run that is only meant to preview.
func cachedRelease(ctx context.Context, e Env, spec Spec) *CacheState {
	l, ok := e.Cache.(cacheLookup)
	if !ok {
		return nil
	}
	ent, err := l.Lookup(ctx, spec.Namespace, spec.Release)
	if err != nil || ent == nil {
		return nil
	}
	return &CacheState{Repo: ent.Repo}
}

func planFor(spec Spec, cached *CacheState, o CreateOptions) *CreatePlan {
	return &CreatePlan{
		Namespace: spec.Namespace, Name: spec.Name, Node: spec.Node, Release: spec.Release,
		Cores: spec.Cores, MemoryGiB: spec.MemoryGiB, DiskGiB: spec.DiskGiB,
		Snapshot: spec.Snapshot, NoCache: o.NoCache, Cache: cached,
		Steps: planSteps(spec, cached, o),
	}
}

func planSteps(spec Spec, cached *CacheState, o CreateOptions) []PlanStep {
	steps := []PlanStep{{Name: "Service and DNS", Detail: "api." + spec.Name + "." + manifests.DefaultBaseDomain}}
	if spec.Snapshot != "" {
		steps = append(steps, PlanStep{Name: "Restore the root disk",
			Detail: fmt.Sprintf("snapshot %s, %d GiB; no ISO and no mirror", spec.Snapshot, spec.DiskGiB)})
	} else {
		steps = append(steps,
			PlanStep{Name: "Cache the release", Detail: cacheDetail(spec.Release, cached, o.NoCache)},
			PlanStep{Name: "Build the agent ISO", Detail: spec.Release.Requested + " in the builder"})
	}
	steps = append(steps, PlanStep{Name: "Create the VM",
		Detail: fmt.Sprintf("%s on %s, %d vCPU / %d GiB / %d GiB",
			spec.Name, spec.Node, spec.Cores, spec.MemoryGiB, spec.DiskGiB)})
	if o.Wait {
		steps = append(steps, PlanStep{Name: "Wait for the install", Detail: "until the cluster is ready"})
	}
	return steps
}

func cacheDetail(rel release.Release, cached *CacheState, noCache bool) string {
	switch {
	case noCache:
		return "not mirrored (--no-cache): the vSNO pulls from the internet"
	case cached != nil:
		return "already mirrored: " + cached.Repo
	}
	return "would mirror " + rel.Image + " (about 20 GiB)"
}
