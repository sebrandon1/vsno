package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/registry"
)

// PruneOptions are the user's choices for vsno cache prune.
type PruneOptions struct {
	Namespace string
	// Version names what to prune. Empty prunes the unused releases beyond
	// Keep.
	Version string
	// Keep is how many unused releases to keep, newest used first.
	Keep int
	// Users maps a release image to the vSNOs running it; a cached release in
	// use is never pruned.
	Users    map[string][]string
	DryRun   bool
	Yes      bool
	Confirm  func(question, answer string) bool
	ShowPlan func(*PrunePlan)
	// NoGC skips the registry garbage collection that frees the space.
	NoGC bool
}

// PrunePlan is what a prune removes.
type PrunePlan struct {
	Entries []Entry `json:"entries,omitempty"`
	Bytes   int64   `json:"bytes"`
}

// Empty reports whether there is nothing to prune.
func (p *PrunePlan) Empty() bool { return len(p.Entries) == 0 }

// PruneResult is a finished (or refused) prune.
type PruneResult struct {
	Plan    PrunePlan `json:"plan"`
	Removed []string  `json:"removed,omitempty"`
	// Freed is what the registry's volume lost, measured across the garbage
	// collection.
	Freed int64 `json:"freed,omitempty"`
}

// Prune removes cached releases: the one named, or the unused ones beyond
// Keep. It asks first, then deletes the images from the registry and runs its
// garbage collector, which is what actually frees the space.
func Prune(ctx context.Context, e Env, o PruneOptions) (*PruneResult, error) {
	e.defaults()
	rep, err := List(ctx, e, o.Namespace, o.Users)
	if err != nil {
		return nil, err
	}
	plan, err := selectEntries(rep, o)
	if err != nil {
		return nil, err
	}
	res := &PruneResult{Plan: plan}
	if plan.Empty() || o.DryRun {
		return res, nil
	}
	if !o.Yes && !confirm(o, &plan) {
		return res, errors.New("not confirmed; rerun with --yes to skip the question")
	}
	if err := apply(ctx, e, o, &plan, res); err != nil {
		return res, err
	}
	return res, nil
}

// confirm asks before deleting; it reports whether the user agreed.
func confirm(o PruneOptions, p *PrunePlan) bool {
	if o.ShowPlan != nil {
		o.ShowPlan(p)
	}
	q := fmt.Sprintf("This deletes %d cached release(s); the registry frees their %s when it is garbage collected. Type %q to continue",
		len(p.Entries), bytesText(p.Bytes), "prune")
	return o.Confirm != nil && o.Confirm(q, "prune")
}

// apply deletes the plan's images, forgets them, and collects the garbage.
func apply(ctx context.Context, e Env, o PruneOptions, plan *PrunePlan, res *PruneResult) error {
	reg, err := registry.Load(ctx, e.Kube, o.Namespace)
	if err != nil {
		return err
	}
	before, _, _ := e.Volume(ctx, o.Namespace)
	for _, ent := range plan.Entries {
		if err := deleteEntry(ctx, e, reg, o.Namespace, ent); err != nil {
			return err
		}
		res.Removed = append(res.Removed, ent.Version+": "+ent.Repo)
	}
	if err := update(ctx, e.Kube, o.Namespace, func(m *metadata) {
		for _, ent := range plan.Entries {
			delete(m.Entries, ent.Digest)
		}
	}); err != nil {
		return err
	}
	if o.NoGC {
		return nil
	}
	if err := GarbageCollect(ctx, e, o.Namespace); err != nil {
		return err
	}
	if after, _, err := e.Volume(ctx, o.Namespace); err == nil && before > after {
		res.Freed = before - after
	}
	return nil
}

// selectEntries picks what to remove: the named release, or the unused ones
// beyond Keep.
func selectEntries(rep *Report, o PruneOptions) (PrunePlan, error) {
	var p PrunePlan
	if o.Version != "" {
		for _, ent := range rep.Entries {
			if matches(ent, o.Version) {
				p.Entries = append(p.Entries, ent)
			}
		}
		if len(p.Entries) == 0 {
			return p, fmt.Errorf("no cached release %s in %s (see `vsno cache list`)", o.Version, o.Namespace)
		}
	} else {
		var unused []Entry
		for _, ent := range rep.Entries {
			if !ent.InUse && ent.State != StateMirroring {
				unused = append(unused, ent)
			}
		}
		slices.SortFunc(unused, func(a, b Entry) int { return b.LastUsed.Compare(a.LastUsed) })
		if o.Keep < 0 {
			o.Keep = 0
		}
		if o.Keep < len(unused) {
			p.Entries = unused[o.Keep:]
		}
	}
	for _, ent := range p.Entries {
		if ent.State == StateMirroring {
			return p, fmt.Errorf("%s is being mirrored right now; prune it once that finishes", ent.Version)
		}
		if ent.InUse {
			return p, fmt.Errorf("vSNO %s still runs %s; delete it first", strings.Join(ent.Users, ", "), ent.Version)
		}
		p.Bytes += ent.SizeBytes
	}
	return p, nil
}

// matches reports whether arg names ent: its version, a form the user asked
// for, its digest, or its image.
func matches(ent Entry, arg string) bool {
	if arg == ent.Version || arg == ent.Image || arg == ent.Digest || strings.HasPrefix(ent.Digest, "sha256:"+arg) {
		return true
	}
	return slices.Contains(ent.Requested, arg)
}

// deleteEntry removes one release's images from the registry: its tag, then
// every manifest left in the repository. The tag goes first because deleting
// the manifest it points at makes the tag undeletable.
func deleteEntry(ctx context.Context, e Env, reg registry.Registry, ns string, ent Entry) error {
	ref := ent.Repo + ":" + short(ent.Digest)
	if _, err := reg.Run(ctx, e.Builder, reg.Host, "delete", ref); err != nil {
		// A tag whose image is already gone is the outcome we want; anything
		// else is a real failure.
		out, xerr := reg.Run(ctx, e.Builder, reg.Host, "exists", ref)
		if xerr != nil || strings.TrimSpace(string(out)) != "no" {
			return fmt.Errorf("deleting %s's tag: %w", ent.Repo, err)
		}
	}
	digests, err := e.digests(ctx, ns, ent.Repo)
	if err != nil {
		return err
	}
	if len(digests) == 0 {
		return nil
	}
	if _, err := reg.Run(ctx, e.Builder, append([]string{reg.Host, "delete-manifests", ent.Repo}, digests...)...); err != nil {
		return fmt.Errorf("deleting %s's images: %w", ent.Repo, err)
	}
	return nil
}

// digests is a repository's manifest digests, however Env gets them.
func (e Env) digests(ctx context.Context, ns, repo string) ([]string, error) {
	if e.Digests != nil {
		return e.Digests(ctx, ns, repo)
	}
	return repoDigests(ctx, e, ns, repo)
}

// repoDigests lists a repository's manifest digests, read from the registry's
// own storage: the v2 API cannot list them.
func repoDigests(ctx context.Context, e Env, ns, repo string) ([]string, error) {
	pod, err := registry.Pod(ctx, e.Kube, ns)
	if err != nil {
		return nil, err
	}
	dir := "/var/lib/registry/docker/registry/v2/repositories/" + repo + "/_manifests/revisions/sha256"
	var out, stderr bytes.Buffer
	cmd := []string{"find", dir, "-mindepth", "1", "-maxdepth", "1", "-type", "d", "-exec", "basename", "{}", ";"}
	if err := builder.ExecPod(ctx, e.Config, e.Kube, ns, pod, registry.Service, cmd, builder.IO{Stdout: &out, Stderr: &stderr}); err != nil {
		return nil, fmt.Errorf("listing %s's images: %w%s", repo, err, builder.Tail(stderr.String()))
	}
	var digests []string
	for _, d := range strings.Fields(out.String()) {
		digests = append(digests, "sha256:"+d)
	}
	slices.Sort(digests)
	return digests, nil
}
