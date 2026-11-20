package isos

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/guest"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PruneOptions are the user's choices for vsno isos prune.
type PruneOptions struct {
	Namespace string
	DryRun    bool
	Yes       bool
	// Keep is how many unused openshift-install binaries to keep, newest first.
	Keep     int
	Confirm  func(question, answer string) bool
	ShowPlan func(*Plan)
}

// Plan is what a prune removes.
type Plan struct {
	// VSNOs are the names whose served ISO and build directory are orphaned.
	VSNOs []string `json:"vsnos,omitempty"`
	// Installers are the unused openshift-install binaries.
	Installers []Entry `json:"installers,omitempty"`
	// Disks are the orphaned ISO DataVolumes.
	Disks []Disk `json:"disks,omitempty"`
	// Bytes is what removing all of it frees.
	Bytes int64 `json:"bytes"`
}

// Empty reports whether there is nothing to prune.
func (p *Plan) Empty() bool {
	return len(p.VSNOs) == 0 && len(p.Installers) == 0 && len(p.Disks) == 0
}

// Result is a finished (or refused) prune.
type Result struct {
	Plan    Plan     `json:"plan"`
	Removed []string `json:"removed,omitempty"`
	Bytes   int64    `json:"bytes,omitempty"`
}

// Prune removes what no vSNO uses: the served ISOs and build directories of
// vSNOs that no longer exist, the openshift-install binaries no vSNO runs
// (keeping the newest Keep), and their orphaned ISO DataVolumes. It plans,
// asks, then deletes.
func Prune(ctx context.Context, e Env, o PruneOptions) (*Result, error) {
	rep, err := List(ctx, e, o.Namespace)
	if err != nil {
		return nil, err
	}
	res := &Result{Plan: planFor(rep, o.Keep)}
	if res.Plan.Empty() || o.DryRun {
		return res, nil
	}
	if !o.Yes {
		if o.ShowPlan != nil {
			o.ShowPlan(&res.Plan)
		}
		q := fmt.Sprintf("This deletes %d orphaned ISO(s), %d unused installer(s) and %d ISO DataVolume(s), freeing %s. Type %q to continue",
			len(res.Plan.VSNOs), len(res.Plan.Installers), len(res.Plan.Disks), bytesText(res.Plan.Bytes), answer)
		if o.Confirm == nil || !o.Confirm(q, answer) {
			return res, errors.New("not confirmed; rerun with --yes to skip the question")
		}
	}
	for _, name := range res.Plan.VSNOs {
		if err := e.Builder.Run(ctx, rmVsno, []string{name}, builder.IO{}); err != nil {
			return res, fmt.Errorf("removing %s's files: %w", name, err)
		}
		res.removed("vSNO "+name, res.Plan.Bytes)
	}
	for _, ent := range res.Plan.Installers {
		if err := e.Builder.Run(ctx, rmInstaller, []string{ent.Name}, builder.IO{}); err != nil {
			return res, fmt.Errorf("removing %s: %w", ent.Name, err)
		}
		res.removed("installer "+ent.Name, ent.Size)
	}
	for _, d := range res.Plan.Disks {
		err := e.Dyn.Resource(guest.GVRDataVolume).Namespace(o.Namespace).Delete(ctx, d.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return res, fmt.Errorf("deleting DataVolume %s: %w", d.Name, err)
		}
		res.removed("DataVolume "+d.Name, d.Size)
	}
	return res, nil
}

// answer is what the user types to confirm.
const answer = "prune"

func (r *Result) removed(what string, size int64) {
	r.Removed = append(r.Removed, what)
	r.Bytes += size
}

// planFor picks what to remove. An orphaned vSNO's served ISO and build
// directory count once: they go together.
func planFor(rep *Report, keep int) Plan {
	var p Plan
	seen := map[string]bool{}
	var installers []Entry
	for _, ent := range rep.Entries {
		switch {
		case ent.Kind == Served || ent.Kind == Build:
			if ent.Use == Orphaned && !seen[ent.Name] {
				seen[ent.Name] = true
				p.VSNOs = append(p.VSNOs, ent.Name)
				p.Bytes += ent.Size
				continue
			}
			if ent.Use == Orphaned {
				p.Bytes += ent.Size // the second half of a pair
			}
		case ent.Kind == Installer && ent.Use == Unused:
			installers = append(installers, ent)
		}
	}
	// Newest first, so --keep keeps the most recently used.
	sortByNewest(installers)
	if keep < 0 {
		keep = 0
	}
	if keep < len(installers) {
		p.Installers = installers[keep:]
		for _, ent := range p.Installers {
			p.Bytes += ent.Size
		}
	}
	for _, d := range rep.Disks {
		if d.Use == Orphaned {
			p.Disks = append(p.Disks, d)
			p.Bytes += d.Size
		}
	}
	return p
}

// sortByNewest orders installers by modification time, newest first.
func sortByNewest(es []Entry) {
	slices.SortFunc(es, func(a, b Entry) int {
		switch {
		case a.Modified.After(b.Modified):
			return -1
		case b.Modified.After(a.Modified):
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
}

// rmVsno removes one vSNO's files from the builder, as guest.Delete does.
const rmVsno = `rm -rf "/work/guests/$1" "/work/www/$1.iso" "/work/www/$1.iso.tmp"`

// rmInstaller removes one cached openshift-install binary and its .ref.
const rmInstaller = `rm -f "/work/bin/$1" "/work/bin/$1.ref"`

// bytesText renders a size in GiB or MiB.
func bytesText(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGi", float64(n)/(1<<30))
	case n > 0:
		return fmt.Sprintf("%dMi", (n+(1<<20)-1)>>20)
	}
	return "nothing"
}
