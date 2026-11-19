package setup

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/manifests"
	"github.com/sebrandon1/vsno/lib/precheck"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	cnvNS      = cluster.CNVNamespace
	storageNS  = cluster.StorageNamespace
	hcoName    = cluster.HyperConvergedName
	lvmCluster = cluster.LVMClusterName
)

// operatorSpec is an OLM operator setup installs unless its CSV already succeeded.
type operatorSpec struct {
	cluster.Operator
	channel func(manifests.Config) string
}

var (
	cnv  = operatorSpec{cluster.CNV, func(c manifests.Config) string { return c.CNVChannel }}
	lvms = operatorSpec{cluster.LVMS, func(c manifests.Config) string { return c.LVMSChannel }}
)

func (r *runner) operator(op operatorSpec) func(context.Context, precheck.ProgressStep) (Step, error) {
	return func(ctx context.Context, ps precheck.ProgressStep) (Step, error) {
		step := Step{Name: op.Key}
		csv, err := cluster.SucceededCSV(ctx, r.Dyn, op.Operator)
		if err != nil {
			return step, err
		}
		if csv != "" {
			r.st.Versions[op.Key] = csv
			step.Outcome, step.Detail = Reused, csv
			return step, nil
		}
		if op.channel(r.cfg) == "" {
			return step, fmt.Errorf("no update channel for %s; the precheck catalog check found none", strings.TrimSuffix(op.CSVPrefix, "."))
		}
		detail, err := r.applyTemplate(ctx, manifests.Operators, inNamespace(op.Namespace))
		if err != nil {
			return step, err
		}
		step.Outcome, step.Detail = r.outcome(), detail
		if r.o.DryRun {
			return step, nil
		}
		ps.Update("waiting for the CSV to succeed")
		err = r.wait(ctx, op.CSVPrefix+"* CSV", func(ctx context.Context) (bool, error) {
			csv, err = cluster.SucceededCSV(ctx, r.Dyn, op.Operator)
			return csv != "", err
		})
		r.st.Versions[op.Key] = csv
		step.Detail += "; " + csv
		return step, err
	}
}

// inNamespace keeps the objects in ns, and the Namespace object ns itself.
func inNamespace(ns string) func(*unstructured.Unstructured) bool {
	return func(o *unstructured.Unstructured) bool {
		return o.GetNamespace() == ns || (o.GetKind() == "Namespace" && o.GetName() == ns)
	}
}

func (r *runner) hyperConverged(ctx context.Context, ps precheck.ProgressStep) (Step, error) {
	step := Step{Name: manifests.StepHyperConverged}
	hc := r.Dyn.Resource(precheck.GVRHyperConverged).Namespace(cnvNS)
	cur, err := hc.Get(ctx, hcoName, metav1.GetOptions{})
	switch {
	case err == nil:
		avail := cluster.Condition(cur, "Available")
		step.Outcome, step.Detail = Reused, "Available="+avail
		if avail == "True" || r.o.DryRun {
			return step, nil
		}
	case !apierrors.IsNotFound(err) && !r.o.DryRun:
		// In a dry run on a fresh cluster the CRD doesn't exist yet.
		return step, err
	default:
		detail, err := r.applyTemplate(ctx, manifests.HyperConverged, nil)
		if err != nil {
			return step, err
		}
		step.Outcome, step.Detail = r.outcome(), detail
		if r.o.DryRun {
			return step, nil
		}
	}
	ps.Update("waiting for Available")
	return step, r.wait(ctx, "HyperConverged Available", func(ctx context.Context) (bool, error) {
		cur, err := hc.Get(ctx, hcoName, metav1.GetOptions{})
		return err == nil && cluster.Condition(cur, "Available") == "True", err
	})
}

// storage reuses StorageClass lvms-vg-vsno when TopoLVM serves the node,
// otherwise creates an LVMCluster on the chosen disk after confirmation.
func (r *runner) storage(ctx context.Context, ps precheck.ProgressStep) (Step, error) {
	step := Step{Name: manifests.StepStorage}
	node, sc := r.cfg.Node, r.cfg.StorageClass
	_, err := r.Kube.StorageV1().StorageClasses().Get(ctx, sc, metav1.GetOptions{})
	switch {
	case err == nil && precheck.HasTopoLVM(ctx, r.Kube, node):
		r.cfg.DiskByID = "" // the existing LVMCluster owns its disk
		step.Outcome, step.Detail = Reused, "StorageClass "+sc+" on "+node
		return step, nil
	case err == nil:
		return step, fmt.Errorf("StorageClass %s exists but LVMS does not serve node %s; pass --node with a node it serves", sc, node)
	case !apierrors.IsNotFound(err):
		return step, err
	}
	if err := r.checkNoOtherLVMCluster(ctx); err != nil {
		return step, err
	}
	disk := r.cfg.DiskByID
	if disk == "" {
		return step, fmt.Errorf("no disk for LVMS on %s: pass --disk (see `vsno precheck`)", node)
	}
	if !r.o.Yes && !r.o.DryRun {
		q := fmt.Sprintf("LVMS will take over %s on %s and erase everything on it. Type the node name to continue", disk, node)
		if r.Confirm == nil || !r.Confirm(q, node) {
			return step, errors.New("not confirmed; rerun with --yes to approve the disk")
		}
	}
	detail, err := r.applyTemplate(ctx, manifests.LVMCluster, nil)
	if err != nil {
		return step, err
	}
	step.Outcome, step.Detail = r.outcome(), detail+"; "+disk+" on "+node
	if r.o.DryRun {
		return step, nil
	}
	ps.Update("waiting for the LVMCluster to be Ready")
	lc := r.Dyn.Resource(precheck.GVRLVMCluster).Namespace(storageNS)
	return step, r.wait(ctx, "LVMCluster Ready and StorageClass "+sc, func(ctx context.Context) (bool, error) {
		cur, err := lc.Get(ctx, lvmCluster, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if st, _, _ := unstructured.NestedString(cur.Object, "status", "state"); st != "Ready" {
			return false, nil
		}
		_, err = r.Kube.StorageV1().StorageClasses().Get(ctx, sc, metav1.GetOptions{})
		return err == nil, err
	})
}

// checkNoOtherLVMCluster fails if LVMS already has a different LVMCluster:
// LVMS supports only one.
func (r *runner) checkNoOtherLVMCluster(ctx context.Context) error {
	l, err := r.Dyn.Resource(precheck.GVRLVMCluster).Namespace(storageNS).List(ctx, metav1.ListOptions{})
	if err != nil {
		if r.o.DryRun || apierrors.IsNotFound(err) {
			return nil // no CRD yet
		}
		return err
	}
	for _, lc := range l.Items {
		if lc.GetName() != lvmCluster {
			return fmt.Errorf("LVMCluster %s/%s already exists and LVMS supports only one; add a device class named vg-vsno to it, or remove it", storageNS, lc.GetName())
		}
	}
	return nil
}

// errStuck marks a wait error that won't fix itself, so waiting stops early.
type errStuck struct{ error }

// wait polls cond until it is true or r.Timeout passes. Errors from cond are
// treated as "not yet" (e.g. a CRD still installing) unless they are errStuck;
// the last one is reported.
func (r *runner) wait(ctx context.Context, what string, cond func(context.Context) (bool, error)) error {
	var last error
	err := wait.PollUntilContextTimeout(ctx, r.Poll, r.Timeout, true, func(ctx context.Context) (bool, error) {
		ok, err := cond(ctx)
		if errors.As(err, new(errStuck)) {
			return false, err
		}
		last = err
		return ok, nil
	})
	switch {
	case err == nil:
		return nil
	case errors.As(err, new(errStuck)):
		return fmt.Errorf("%s: %w", what, err)
	case last != nil:
		return fmt.Errorf("waiting for %s: %w (last error: %v)", what, err, last)
	default:
		return fmt.Errorf("waiting for %s: %w", what, err)
	}
}
