package setup

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/manifests"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/state"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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
		if u := r.upgrades[op.Key]; u != nil {
			return r.upgradeOperator(ctx, ps, op, u)
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

// offerUpdates lists the operators with an update available and, unless this is
// a dry run or --upgrade-operators answered, asks about each. A channel named
// with --operator-channel is taken as given: it is applied without asking, and
// a request that can't be honored fails the run. The accepted updates land in
// r.upgrades, for the operator steps to apply.
func (r *runner) offerUpdates(ctx context.Context, rep *precheck.Report) error {
	for _, op := range []operatorSpec{cnv, lvms} {
		requested := r.o.Channels[op.Key]
		var u *cluster.Update
		var err error
		if requested != "" {
			u, err = cluster.RequestedUpdate(ctx, r.Dyn, op.Operator, requested)
			if err != nil {
				return fmt.Errorf("%s: %w", op.Key, err)
			}
		} else {
			u, err = cluster.OperatorUpdate(ctx, r.Dyn, op.Operator, op.channel(r.cfg))
			if err != nil {
				r.res.Warnings = append(r.res.Warnings, "cannot tell whether "+op.Key+" is up to date: "+err.Error())
				continue
			}
		}
		if u == nil {
			continue
		}
		r.res.Updates = append(r.res.Updates, *u)
		q := "Upgrade " + op.Key + " from " + u.Installed + " to " + updateTarget(u)
		if u.Suggested != "" {
			q += " (channel " + u.Channel + " → " + u.Suggested + ")"
		}
		if op.Key == cluster.CNV.Key {
			if vms := r.runningVMs(ctx); len(vms) > 0 {
				m := fmt.Sprintf("%d running VM(s)", len(vms)) + ": " + summarize(vms)
				q += "; this restarts OpenShift Virtualization and affects " + m
				r.res.Warnings = append(r.res.Warnings, "upgrading cnv restarts OpenShift Virtualization and affects "+m)
			}
		}
		switch {
		case r.o.DryRun, r.o.UpgradeOperators, requested != "":
		case r.Confirm == nil || !r.Confirm(q, "y"):
			r.res.Warnings = append(r.res.Warnings,
				op.Key+" update "+u.String()+" not applied; pass --upgrade-operators to upgrade it")
			continue
		}
		r.upgrades[op.Key] = u
	}
	if rep.HubManaged && len(r.upgrades) > 0 {
		r.res.Warnings = append(r.res.Warnings, "an ACM hub manages this cluster: its policies may revert an operator upgrade")
	}
	return nil
}

// updateTarget is what an upgrade installs, for a question.
func updateTarget(u *cluster.Update) string {
	if u.Available == "" {
		return "the pending upgrade"
	}
	return u.Available
}

// runningVMs names the VirtualMachines that a CNV upgrade would restart, as
// ns/name: the ones that keep an instance up, wherever they live.
func (r *runner) runningVMs(ctx context.Context) []string {
	l, err := r.Dyn.Resource(cluster.GVRVirtualMachine).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil // no KubeVirt, or no access: the upgrade's own checks report it
	}
	var out []string
	for i := range l.Items {
		vm := &l.Items[i]
		if !runs(vm) {
			continue
		}
		out = append(out, vm.GetNamespace()+"/"+vm.GetName())
	}
	slices.Sort(out)
	return out
}

// runs reports whether vm has an instance: spec.running, or one of the
// runStrategies that keep a VM up. vsno's own VMs use runStrategy Always.
func runs(vm *unstructured.Unstructured) bool {
	if on, _, _ := unstructured.NestedBool(vm.Object, "spec", "running"); on {
		return true
	}
	if s, _, _ := unstructured.NestedString(vm.Object, "spec", "runStrategy"); s != "Always" && s != "RerunOnFailure" {
		return false
	}
	s, _, _ := unstructured.NestedString(vm.Object, "status", "printableStatus")
	return s != "Stopped"
}

// summarize lists the first five of names and counts the rest.
func summarize(names []string) string {
	if len(names) <= 5 {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:5], ", ") + fmt.Sprintf(" and %d more", len(names)-5)
}

// upgradeOperator applies an accepted update: it moves the Subscription to the
// channel precheck picked and approves an InstallPlan waiting for approval,
// then waits for the new CSV to succeed and, for CNV, for HyperConverged to be
// Available again.
func (r *runner) upgradeOperator(ctx context.Context, ps precheck.ProgressStep, op operatorSpec, u *cluster.Update) (Step, error) {
	step := Step{Name: op.Key, Outcome: Upgraded, Detail: u.String()}
	if r.o.DryRun {
		step.Outcome, step.Detail = Planned, "upgrade "+u.String()
		return step, nil
	}
	sub, err := cluster.Subscription(ctx, r.Dyn, op.Operator)
	if err != nil {
		return step, err
	}
	if sub == nil {
		return step, fmt.Errorf("%s has no Subscription, so OLM has nothing to upgrade", op.Key)
	}
	if u.Suggested != "" {
		ps.Update("moving to channel " + u.Suggested)
		if err := cluster.SetChannel(ctx, r.Dyn, sub, u.Suggested); err != nil {
			return step, err
		}
	}
	if u.Plan != "" {
		ps.Update("approving " + u.Plan)
		if err := cluster.Approve(ctx, r.Dyn, op.Namespace, u.Plan); err != nil {
			return step, err
		}
	}
	if u.SameCSV() {
		// The channel pointed at the CSV that already runs: moving it is the
		// whole upgrade, and no new CSV will appear to wait for.
		r.st.Versions[op.Key] = u.Installed
		step.Detail = u.String()
		return step, nil
	}
	ps.Update("waiting for the new CSV to succeed")
	csv, err := r.waitForUpgrade(ctx, op, u.Installed)
	if csv != "" {
		r.st.Versions[op.Key] = csv
		step.Detail += "; " + csv
	}
	if err != nil {
		return step, err
	}
	if op.Key == cluster.CNV.Key {
		err = r.waitHyperConverged(ctx)
	}
	return step, err
}

// waitForUpgrade waits for the Subscription to settle on a new CSV: OLM has
// moved currentCSV off old, and the CSV it points at is installed and
// Succeeded. A Manual Subscription's next InstallPlan is approved on the way,
// since a channel change makes OLM create a new one.
func (r *runner) waitForUpgrade(ctx context.Context, op operatorSpec, old string) (string, error) {
	var csv string
	err := r.wait(ctx, op.Key+" upgrade", func(ctx context.Context) (bool, error) {
		sub, err := cluster.Subscription(ctx, r.Dyn, op.Operator)
		if err != nil || sub == nil {
			return false, err
		}
		st := cluster.SubscriptionStatusOf(sub)
		if st.Approval == "Manual" {
			if _, err := cluster.ApprovePending(ctx, r.Dyn, op.Operator, st.Installed); err != nil {
				return false, errStuck{err}
			}
		}
		if st.Current == "" || st.Current == old || st.Installed != st.Current {
			return false, nil
		}
		phase, err := cluster.CSVPhase(ctx, r.Dyn, op.Operator, st.Installed)
		if err != nil {
			return false, err
		}
		csv = st.Installed
		return phase == "Succeeded", nil
	})
	return csv, err
}

// waitHyperConverged waits for HyperConverged to report Available after a CNV
// upgrade. Without the object, or its CRD, there is nothing to wait for.
func (r *runner) waitHyperConverged(ctx context.Context) error {
	hc := r.Dyn.Resource(cluster.GVRHyperConverged).Namespace(cnvNS)
	if _, err := hc.Get(ctx, hcoName, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil
		}
		return err
	}
	return r.wait(ctx, "HyperConverged Available", func(ctx context.Context) (bool, error) {
		cur, err := hc.Get(ctx, hcoName, metav1.GetOptions{})
		return err == nil && cluster.Condition(cur, "Available") == "True", err
	})
}

// inNamespace keeps the objects in ns, and the Namespace object ns itself.
func inNamespace(ns string) func(*unstructured.Unstructured) bool {
	return func(o *unstructured.Unstructured) bool {
		return o.GetNamespace() == ns || (o.GetKind() == "Namespace" && o.GetName() == ns)
	}
}

func (r *runner) hyperConverged(ctx context.Context, ps precheck.ProgressStep) (Step, error) {
	step := Step{Name: manifests.StepHyperConverged}
	hc := r.Dyn.Resource(cluster.GVRHyperConverged).Namespace(cnvNS)
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
	note, derr := r.checkDisk(ctx, node, &disk)
	if derr != nil {
		return step, derr
	}
	if err := r.confirmDisk(disk, node); err != nil {
		return step, err
	}
	detail, err := r.applyTemplate(ctx, manifests.LVMCluster, nil)
	if err != nil {
		return step, err
	}
	step.Outcome, step.Detail = r.outcome(), detail+"; "+disk+" on "+node+note
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

// confirmDisk asks the user to type the node name before LVMS erases the disk.
func (r *runner) confirmDisk(disk, node string) error {
	if r.o.Yes || r.o.DryRun {
		return nil
	}
	q := fmt.Sprintf("LVMS will take over %s on %s and erase everything on it. Type the node name to continue", disk, node)
	if r.Confirm == nil || !r.Confirm(q, node) {
		return errors.New("not confirmed; rerun with --yes to approve the disk")
	}
	return nil
}

// checkDisk re-probes the disk setup is about to erase and refuses one it
// cannot prove is blank, unless --force-disk. It returns a note for the step.
func (r *runner) checkDisk(ctx context.Context, node string, disk *string) (string, error) {
	switch {
	case r.o.DryRun:
		return " (not re-probed: dry run)", nil
	case r.VerifyDisk == nil:
		return " (not re-checked: no hardware probe)", nil
	}
	d, err := r.VerifyDisk(ctx, node, *disk)
	if err != nil {
		return "", fmt.Errorf("re-checking %s on %s: %w", *disk, node, err)
	}
	if !d.Empty() {
		why := strings.Join(d.Problems(), ", ")
		if !r.o.ForceDisk {
			return "", fmt.Errorf("%s on %s is not provably empty (%s); LVMS would erase it. Wipe it, choose another --device, or pass --force-disk", *disk, node, why)
		}
		r.res.Warnings = append(r.res.Warnings, fmt.Sprintf("%s on %s is not provably empty (%s); using it anyway (--force-disk)", *disk, node, why))
	}
	// A kernel name is not stable across reboots; the LVMCluster wants by-id.
	if id := d.PreferredByID(); id != "" && !strings.HasPrefix(*disk, "/dev/disk/by-id/") {
		r.res.Warnings = append(r.res.Warnings, fmt.Sprintf("%s is not a stable name; using %s instead", *disk, id))
		*disk = id
		r.cfg.DiskByID = id
	}
	return "", nil
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

// hyperConvergedAndDensity is the HyperConverged step: HyperConverged
// Available, then the density settings asked for.
func (r *runner) hyperConvergedAndDensity(ctx context.Context, ps precheck.ProgressStep) (Step, error) {
	step, err := r.hyperConverged(ctx, ps)
	if err != nil {
		return step, err
	}
	detail, err := r.density(ctx)
	if detail != "" {
		step.Detail += "; " + detail
	}
	return step, err
}

// density applies --memory-overcommit and --ksm to HyperConverged, after
// asking: they change every VM on the cluster. It records the settings it
// replaces, which teardown restores.
func (r *runner) density(ctx context.Context) (string, error) {
	if r.o.MemoryOvercommit == 0 && !r.o.KSM {
		return "", nil
	}
	cur, err := cluster.ReadDensity(ctx, r.Dyn)
	if err != nil {
		return "", err
	}
	want := cur
	if r.o.MemoryOvercommit > 0 {
		want.OvercommitPercent = r.o.MemoryOvercommit
	}
	if r.o.KSM {
		want.KSMNode = ksmWith(cur.KSMNode, r.cfg.Node)
	}
	change := densityChange(cur, want)
	switch {
	case change == "":
		return "density unchanged", nil
	case r.o.DryRun:
		return "would set " + change, nil
	case !r.o.Yes && (r.Confirm == nil || !r.Confirm("This changes every VM on the cluster: "+change+". Type y to continue", "y")):
		return "", errors.New("density not changed: not confirmed; pass --yes to change it without asking")
	}
	prev := cur
	r.st.PreviousDensity = &prev
	if err := cluster.ApplyDensity(ctx, r.Dyn, want); err != nil {
		return "", err
	}
	return "set " + change, nil
}

// ksmWith is the KSM setting that covers node without turning it off where
// it is on: unchanged when it already covers node, every node when it is on
// for another one.
func ksmWith(cur, node string) string {
	switch cur {
	case "":
		return node
	case node, state.KSMEveryNode:
		return cur
	}
	return state.KSMEveryNode
}

// densityChange says what moving from cur to want changes, or "" for nothing.
func densityChange(cur, want state.Density) string {
	var parts []string
	if cur.Percent() != want.Percent() {
		parts = append(parts, fmt.Sprintf("memory overcommit %d%% → %d%%", cur.Percent(), want.Percent()))
	}
	if cur.KSMNode != want.KSMNode {
		parts = append(parts, "KSM "+cur.KSMText()+" → "+want.KSMText())
	}
	return strings.Join(parts, ", ")
}
