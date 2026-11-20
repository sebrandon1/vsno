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

// DeleteOptions are the user's choices for vsno iso delete.
type DeleteOptions struct {
	Namespace string
	// Args name what to delete: a vSNO, an openshift-install binary, or an
	// ISO DataVolume.
	Args     []string
	DryRun   bool
	Yes      bool
	Confirm  func(question, answer string) bool
	ShowPlan func(*Plan)
}

// DeleteResult is a finished (or refused) delete.
type DeleteResult struct {
	Plan    Plan     `json:"plan"`
	Deleted []string `json:"deleted,omitempty"`
	Bytes   int64    `json:"bytes,omitempty"`
}

// Delete removes the ISOs, installers and ISO DataVolumes named in o.Args.
// Anything a vSNO still uses is refused: that is what `vsno iso eject` and
// `vsno delete` are for. It asks "are you sure?" first, unless --yes.
func Delete(ctx context.Context, e Env, o DeleteOptions) (*DeleteResult, error) {
	rep, err := List(ctx, e, o.Namespace)
	if err != nil {
		return nil, err
	}
	plan, err := selectFor(rep, o.Args)
	if err != nil {
		return nil, err
	}
	res := &DeleteResult{Plan: plan}
	if plan.Empty() || o.DryRun {
		return res, nil
	}
	if !o.Yes {
		if o.ShowPlan != nil {
			o.ShowPlan(&plan)
		}
		q := fmt.Sprintf("Are you sure? Deleting %d thing(s) frees %s", countItems(&plan), bytesText(plan.Bytes))
		if o.Confirm == nil || !o.Confirm(q, "y") {
			return res, errors.New("not confirmed; rerun with --yes to skip the question")
		}
	}
	res.Deleted, res.Bytes, err = remove(ctx, e, o.Namespace, &plan)
	return res, err
}

// countItems is how many things a plan removes.
func countItems(p *Plan) int {
	return len(p.VSNOs) + len(p.Installers) + len(p.Disks)
}

// selectFor resolves the arguments against the listing.
func selectFor(rep *Report, args []string) (Plan, error) {
	var p Plan
	seen := map[string]bool{}
	for _, arg := range args {
		if seen[arg] {
			continue
		}
		seen[arg] = true
		n, err := selectOne(rep, arg, &p)
		if err != nil {
			return Plan{}, err
		}
		if n == 0 {
			return Plan{}, fmt.Errorf("nothing named %s in the builder or the ISO DataVolumes (see `vsno iso list`)", arg)
		}
	}
	return p, nil
}

// selectOne adds arg's files to p and returns how many it found. A vSNO
// brings its served ISO, its build directory and its orphaned ISO DataVolume.
func selectOne(rep *Report, arg string, p *Plan) (int, error) {
	n := 0
	var files VSNOFiles
	for _, ent := range rep.Entries {
		switch {
		case (ent.Kind == Served || ent.Kind == Build) && ent.Name == arg:
			if ent.Use == InUse {
				return 0, fmt.Errorf("vSNO %s still uses its install ISO; `vsno iso eject %s` takes it off the VM first", arg, arg)
			}
			files.Name = arg
			files.Size += ent.Size
			n++
		case ent.Kind == Installer && ent.Name == arg:
			if ent.Use == InUse {
				return 0, fmt.Errorf("%s is the installer vSNO %s runs; delete that vSNO first", arg, strings.Join(ent.Users, ", "))
			}
			p.Installers = append(p.Installers, ent)
			p.Bytes += ent.Size
			n++
		}
	}
	if files.Name != "" {
		p.VSNOs = append(p.VSNOs, files)
		p.Bytes += files.Size
	}
	for _, d := range rep.Disks {
		if d.Name != arg && d.VSNO != arg {
			continue
		}
		if slices.ContainsFunc(p.Disks, func(x Disk) bool { return x.Name == d.Name }) {
			continue // named twice: by its vSNO and by itself
		}
		if d.Use == InUse {
			return 0, fmt.Errorf("vSNO %s still has its ISO DataVolume attached; `vsno iso eject %s` removes it safely", d.VSNO, d.VSNO)
		}
		p.Disks = append(p.Disks, d)
		p.Bytes += d.Size
		n++
	}
	return n, nil
}

// remove deletes what p names, and returns what it removed and how much it
// freed. A failure stops there: the rest stays.
func remove(ctx context.Context, e Env, ns string, p *Plan) ([]string, int64, error) {
	var removed []string
	var bytes int64
	for _, v := range p.VSNOs {
		if err := e.Builder.Run(ctx, rmVsno, []string{v.Name}, builder.IO{}); err != nil {
			return removed, bytes, fmt.Errorf("removing %s's files: %w", v.Name, err)
		}
		removed = append(removed, "vSNO "+v.Name)
		bytes += v.Size
	}
	for _, ent := range p.Installers {
		if err := e.Builder.Run(ctx, rmInstaller, []string{ent.Name}, builder.IO{}); err != nil {
			return removed, bytes, fmt.Errorf("removing %s: %w", ent.Name, err)
		}
		removed = append(removed, "installer "+ent.Name)
		bytes += ent.Size
	}
	for _, d := range p.Disks {
		err := e.Dyn.Resource(guest.GVRDataVolume).Namespace(ns).Delete(ctx, d.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return removed, bytes, fmt.Errorf("deleting DataVolume %s: %w", d.Name, err)
		}
		removed = append(removed, "DataVolume "+d.Name)
		bytes += d.Size
	}
	slices.Sort(removed)
	return removed, bytes, nil
}
