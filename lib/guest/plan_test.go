package guest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/cache"
	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/release"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func planSpec() Spec {
	return Spec{Namespace: ns, Name: "seed", Node: node, Cores: 16, MemoryGiB: 32, DiskGiB: 200,
		Release: release.Release{Requested: "4.20", Version: "4.20.46", Image: "quay.io/x:4.20.46"}}
}

func stepNamed(steps []PlanStep, name string) (PlanStep, bool) {
	for _, s := range steps {
		if s.Name == name {
			return s, true
		}
	}
	return PlanStep{}, false
}

func TestPlanFor(t *testing.T) {
	hit := &CacheState{Repo: "ocp/release/abc"}
	for _, c := range []struct {
		name    string
		spec    Spec
		cached  *CacheState
		opts    CreateOptions
		steps   []string
		detail  string // in the "Cache the release" step
		noSteps []string
	}{
		{"install, cache miss", planSpec(), nil, CreateOptions{},
			[]string{"Service and DNS", "Cache the release", "Build the agent ISO", "Create the VM"}, "would mirror", nil},
		{"install, cache hit", planSpec(), hit, CreateOptions{},
			[]string{"Cache the release"}, "already mirrored: ocp/release/abc", nil},
		{"install, no cache", planSpec(), nil, CreateOptions{NoCache: true},
			[]string{"Cache the release"}, "not mirrored (--no-cache)", nil},
		{"install, wait", planSpec(), nil, CreateOptions{Wait: true},
			[]string{"Wait for the install"}, "", nil},
		{"snapshot", Spec{Namespace: ns, Name: "seed", Node: node, Cores: 16, MemoryGiB: 32, DiskGiB: 200,
			Snapshot: "seed", Release: release.Release{Requested: "4.20", Version: "4.20.46"}}, nil, CreateOptions{},
			[]string{"Service and DNS", "Restore the root disk", "Create the VM"}, "",
			[]string{"Cache the release", "Build the agent ISO"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := planFor(c.spec, c.cached, c.opts)
			if p.Name != "seed" || p.Node != node || p.Cores != 16 {
				t.Errorf("plan = %+v", p)
			}
			for _, name := range c.steps {
				if _, ok := stepNamed(p.Steps, name); !ok {
					t.Errorf("missing step %q in %+v", name, p.Steps)
				}
			}
			for _, name := range c.noSteps {
				if _, ok := stepNamed(p.Steps, name); ok {
					t.Errorf("unexpected step %q in %+v", name, p.Steps)
				}
			}
			if c.detail != "" {
				s, _ := stepNamed(p.Steps, "Cache the release")
				if !strings.Contains(s.Detail, c.detail) {
					t.Errorf("cache detail = %q, want %q", s.Detail, c.detail)
				}
			}
		})
	}
}

// lookupCache is a Cache that answers Lookup, as the command's adapter does.
type lookupCache struct {
	entry *cache.Entry
	err   error
}

func (lookupCache) Ensure(context.Context, string, release.Release, func(string)) (*cache.Prepared, error) {
	return nil, nil
}

func (l lookupCache) Lookup(context.Context, string, release.Release) (*cache.Entry, error) {
	return l.entry, l.err
}

func TestPlan(t *testing.T) {
	w := newWorld(t)
	p, err := Plan(context.Background(), w.env(), createOpts("seed", "4.20"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "seed" || p.Release.Version != "4.20.46" || p.Node != node || p.Cores != 16 || p.DiskGiB != 200 {
		t.Errorf("plan = %+v", p)
	}
	// Nothing create would do happened.
	ctx := context.Background()
	if _, err := w.dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(ctx, "seed", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("dry run made the VM: %v", err)
	}
	for _, s := range []string{"iso.sh", "rm.sh"} {
		if calls := w.runner.ran(s); len(calls) != 0 {
			t.Errorf("dry run ran %s: %+v", s, calls)
		}
	}
}

func TestPlanNoCache(t *testing.T) {
	w := newWorld(t)
	o := createOpts("seed", "4.20")
	o.NoCache = true
	p, err := Plan(context.Background(), w.env(), o)
	if err != nil {
		t.Fatal(err)
	}
	if p.Cache != nil || !p.NoCache {
		t.Errorf("no-cache plan = %+v", p)
	}
}

func TestPlanCacheLookup(t *testing.T) {
	w := newWorld(t)
	env := w.env()
	p, err := Plan(context.Background(), env, createOpts("seed", "4.20"))
	if err != nil || p.Cache != nil {
		t.Fatalf("without a Lookup cache: %+v, %v", p, err)
	}

	env.Cache = lookupCache{entry: &cache.Entry{Repo: "ocp/release/abc", Image: "quay.io/x:4.20.46"}}
	p, err = Plan(context.Background(), env, createOpts("seed", "4.20"))
	if err != nil || p.Cache == nil || p.Cache.Repo != "ocp/release/abc" {
		t.Errorf("cache hit = %+v, %v", p, err)
	}

	env.Cache = lookupCache{err: errors.New("registry down")}
	p, err = Plan(context.Background(), env, createOpts("seed", "4.20"))
	if err != nil || p.Cache != nil {
		t.Errorf("unreadable cache must leave the plan silent: %+v, %v", p, err)
	}
}

func TestPlanRejectsExistingVSNO(t *testing.T) {
	w := newWorld(t, vm("seed", "Running"))
	_, err := Plan(context.Background(), w.env(), createOpts("seed", "4.20"))
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("existing vSNO = %v, want already exists", err)
	}
}

func TestPlanFreeName(t *testing.T) {
	w := newWorld(t)
	p, err := Plan(context.Background(), w.env(), createOpts("", "4.20"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateName(p.Name); err != nil || p.Name == "" {
		t.Errorf("free name = %q, %v", p.Name, err)
	}
}
