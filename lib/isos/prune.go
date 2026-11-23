package isos

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// PruneOptions are the user's choices for vsno isos prune.
type PruneOptions struct {
	Namespace string
	DryRun    bool
	Yes       bool
	// Keep is how many unused openshift-install binaries to keep, newest first.
	Keep int
	// Confirm asks question and approves answer, which may be "y|prune": the
	// first is what the prompt asks for, the rest are what it used to.
	Confirm  func(question, answer string) bool
	ShowPlan func(*Plan)
}

// Plan is what a prune removes.
type Plan struct {
	// VSNOs are the served ISOs and build directories whose vSNO is gone.
	VSNOs []VSNOFiles `json:"vsnos,omitempty"`
	// Installers are the unused openshift-install binaries.
	Installers []Entry `json:"installers,omitempty"`
	// Disks are the orphaned ISO DataVolumes.
	Disks []Disk `json:"disks,omitempty"`
	// Bytes is what removing all of it frees.
	Bytes int64 `json:"bytes"`
}

// VSNOFiles is one orphaned vSNO's files in the builder.
type VSNOFiles struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
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
		if o.Confirm == nil || !o.Confirm(q, answer+"|"+legacyAnswer) {
			return res, errors.New("not confirmed; rerun with --yes to skip the question")
		}
	}
	res.Removed, res.Bytes, err = remove(ctx, e, o.Namespace, &res.Plan)
	return res, err
}

// answer is what the user types to confirm: y, or the word the prompt used
// before the confirmations were brought together.
const answer = "y"
const legacyAnswer = "prune"

// planFor picks what to remove. An orphaned vSNO's served ISO and build
// directory count once: they go together.
func planFor(rep *Report, keep int) Plan {
	var p Plan
	at := map[string]int{} // a vSNO's entry in p.VSNOs
	var installers []Entry
	for _, ent := range rep.Entries {
		switch {
		case ent.Kind == Served || ent.Kind == Build:
			if ent.Use != Orphaned {
				continue
			}
			i, ok := at[ent.Name]
			if !ok {
				i = len(p.VSNOs)
				at[ent.Name] = i
				p.VSNOs = append(p.VSNOs, VSNOFiles{Name: ent.Name})
			}
			p.VSNOs[i].Size += ent.Size // its served ISO and build directory
			p.Bytes += ent.Size
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

// rmInstaller removes one cached openshift-install binary, its .ref and the
// matching per-release oc.
const rmInstaller = `rm -f "/work/bin/$1" "/work/bin/$1.ref" "/work/bin/oc-${1#openshift-install-}"`

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
