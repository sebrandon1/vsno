// Package teardown removes a vSNO environment: by default its namespace (VMs,
// support services, credentials), and with Host also what setup installed on
// the host cluster (HyperConverged, the LVMCluster and both operators).
package teardown

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sebrandon1/vsno/lib/apply"
	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/manifests"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/state"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// Env is the cluster access and UI teardown needs.
type Env struct {
	Kube kubernetes.Interface
	Dyn  dynamic.Interface
	// Confirm asks the user to type answer to approve question. Nil never approves.
	Confirm func(question, answer string) bool
	// ShowPlan, if set, is called with the planned targets right before Confirm.
	ShowPlan func(*Result)
	// Planner says which of setup's objects exist (apply.SSA). Nil assumes all do.
	Planner  apply.Planner
	Progress precheck.Progress // nil = silent
	Poll     time.Duration     // 0 = 5s
	Timeout  time.Duration     // limit for each wait; 0 = 10m
}

// Options are the user's choices.
type Options struct {
	Namespace string
	Host      bool // also remove the host components
	Force     bool // ignore the safety checks
	Yes       bool // don't ask
	DryRun    bool // list what would be deleted
}

// Outcome is what happened to a target.
type Outcome string

const (
	Planned Outcome = "planned" // not deleted (yet)
	Deleted Outcome = "deleted"
	Gone    Outcome = "gone" // already deleted by an earlier target
)

// Target is an object teardown deletes.
type Target struct {
	Kind      string  `json:"kind"`
	Namespace string  `json:"namespace,omitempty"`
	Name      string  `json:"name"`
	Detail    string  `json:"detail,omitempty"`
	Outcome   Outcome `json:"outcome,omitempty"`
	gvr       schema.GroupVersionResource
	wait      bool   // wait until the object is gone before the next target
	step      string // the setup step (manifests.Steps) it belongs to
}

// Ref is "Kind namespace/name", or "Kind name" for cluster-scoped objects.
func (t Target) Ref() string {
	if t.Namespace == "" {
		return t.Kind + " " + t.Name
	}
	return t.Kind + " " + t.Namespace + "/" + t.Name
}

// Result is the teardown report.
type Result struct {
	Namespace string `json:"namespace"`
	Host      bool   `json:"host"`
	// Blockers are why teardown refused (or would refuse without Force).
	Blockers []string `json:"blockers,omitempty"`
	// Forced is true when Force overrode the Blockers.
	Forced bool `json:"forced,omitempty"`
	// RestoreDensity is the HyperConverged density setup replaced, which a
	// teardown without Host puts back; with Host, HyperConverged goes.
	RestoreDensity *state.Density `json:"restoreDensity,omitempty"`
	// Steps mirror setup's steps in reverse; Objects are their objects.
	Steps   []Step   `json:"steps"`
	Objects []Object `json:"objects"`
	// Unmanaged are VMs and PVCs in the namespace that setup didn't create;
	// deleting the namespace deletes them too.
	Unmanaged []string `json:"unmanaged,omitempty"`
	// Allocated is storage the environment still holds after a teardown that
	// otherwise finished: volumes whose claim is gone but whose disk isn't.
	Allocated []AllocatedPV `json:"allocated,omitempty"`
	// Targets are what teardown deletes itself, in order.
	Targets []Target `json:"targets"`
}

// Run plans the teardown, checks it is safe, confirms, and deletes the targets
// in order. On error the Result shows how far it got.
func Run(ctx context.Context, e Env, o Options) (*Result, error) {
	e.defaults()
	res := &Result{Namespace: o.Namespace, Host: o.Host}
	p := &planner{Env: e, o: o, res: res}
	if err := p.plan(ctx); err != nil {
		return res, err
	}
	if err := p.planSteps(ctx); err != nil {
		return res, err
	}
	if !o.Host {
		if st, _, err := state.Load(ctx, e.Kube, o.Namespace); err == nil {
			res.RestoreDensity = st.PreviousDensity
		}
	}
	for i := range res.Targets {
		res.Targets[i].Outcome = Planned // until deleted
	}
	if len(res.Blockers) > 0 {
		if !o.Force {
			return res, fmt.Errorf("refusing to tear down (%d problem(s)): %s; pass --force to override", len(res.Blockers), strings.Join(res.Blockers, "; "))
		}
		res.Forced = true
	}
	if len(res.Targets) == 0 || o.DryRun {
		return res, nil
	}
	if err := p.confirm(res); err != nil {
		return res, err
	}
	return res, p.execute(ctx, res)
}

// confirm shows the plan and asks, unless Yes.
func (p *planner) confirm(res *Result) error {
	if p.o.Yes {
		return nil
	}
	answer := p.o.Namespace
	if p.o.Host {
		answer = "host"
	}
	if p.ShowPlan != nil {
		p.ShowPlan(res)
	}
	q := fmt.Sprintf("This deletes %d object(s), listed above, and everything else in the namespace. Type %q to continue",
		apply.Count(res.Changes(), apply.Delete), answer)
	if p.Confirm == nil || !p.Confirm(q, answer) {
		return errors.New("not confirmed; rerun with --yes to skip the question")
	}
	return nil
}

// execute puts HyperConverged's density back, then deletes the targets in order.
func (p *planner) execute(ctx context.Context, res *Result) error {
	defer res.markDeleted()
	// Before the namespace goes: its state is where the old settings are.
	if res.RestoreDensity != nil {
		if err := cluster.ApplyDensity(ctx, p.Dyn, *res.RestoreDensity); err != nil {
			return err
		}
	}
	for i := range res.Targets {
		if err := p.delete(ctx, &res.Targets[i]); err != nil {
			return err
		}
	}
	return p.awaitStorageReleased(ctx, res)
}

// volumeGroup is the LVM volume group LVMS made for the environment; its
// LogicalVolumes are where a stuck PV's capacity actually sits.
const volumeGroup = "vg-vsno"

// AllocatedPV is a PersistentVolume the environment still holds: its claim is
// gone, but the volume — and the capacity behind it — is not.
type AllocatedPV struct {
	Name  string            `json:"name"`
	Claim string            `json:"claim"` // namespace/name it was made for
	Phase string            `json:"phase"`
	Size  resource.Quantity `json:"size"`
}

// allocated lists the PersistentVolumes made for claims in the environment's
// namespace. Deleting the namespace leaves them Released rather than deleted:
// the PVC is gone, the PV stays, and nothing else in vsno looks at them. The
// boot-source images belong to another namespace and are not counted.
func (p *planner) allocated(ctx context.Context) ([]AllocatedPV, error) {
	return Allocated(ctx, p.Kube, p.o.Namespace)
}

// Allocated lists the PersistentVolumes made for claims in ns. Deleting the
// namespace leaves them Released rather than deleted: the PVC is gone, the PV
// stays, and nothing else in vsno looks at them. Exported so `vsno doctor` can
// report the leak without a teardown.
func Allocated(ctx context.Context, kube kubernetes.Interface, ns string) ([]AllocatedPV, error) {
	pvs, err := kube.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing PersistentVolumes: %w", err)
	}
	var out []AllocatedPV
	for _, pv := range pvs.Items {
		ref := pv.Spec.ClaimRef
		if ref == nil || ref.Namespace != ns {
			continue
		}
		out = append(out, AllocatedPV{Name: pv.Name, Claim: ref.Namespace + "/" + ref.Name,
			Phase: string(pv.Status.Phase), Size: pv.Spec.Capacity[corev1.ResourceStorage]})
	}
	return out, nil
}

// awaitStorageReleased waits for the volumes the environment's claims left
// behind to go, and fails naming them when they don't: a teardown that only
// looks at the namespace can succeed while the disk is still allocated.
func (p *planner) awaitStorageReleased(ctx context.Context, res *Result) error {
	ps := p.Progress.Start("Checking the storage was released")
	var last []AllocatedPV
	err := wait.PollUntilContextTimeout(ctx, p.Poll, p.Timeout, true, func(ctx context.Context) (bool, error) {
		var err error
		last, err = p.allocated(ctx)
		return len(last) == 0, err
	})
	switch {
	case err == nil:
		ps.Done("nothing is still allocated")
		return nil
	case !wait.Interrupted(err):
		ps.Fail(err.Error())
		return fmt.Errorf("checking the storage teardown released: %w", err)
	}
	res.Allocated = last
	failure := allocatedError(last)
	ps.Fail(failure.Error())
	return failure
}

// allocatedError says what is still allocated and how to find the holder.
func allocatedError(pvs []AllocatedPV) error {
	var total resource.Quantity
	for _, pv := range pvs {
		total.Add(pv.Size)
	}
	first := pvs[0]
	more := ""
	if len(pvs) > 1 {
		more = fmt.Sprintf(" (and %d more, listed in --output json)", len(pvs)-1)
	}
	// The volume's name carries the PVC's UUID, which is what the
	// LogicalVolume and the device are named after.
	uuid := strings.TrimPrefix(first.Name, "pvc-")
	return fmt.Errorf("teardown finished but %s is still allocated: PV %s%s (%s, %s, was %s). "+
		"The volume's LogicalVolume is stuck deleting, usually because the device is still mounted or held open on the node:\n\n"+
		"  oc debug node/<node> -- chroot /host sh -c 'grep %s /proc/mounts; lsof /dev/%s/%s'\n\n"+
		"Release the holder and vg-manager removes the volume within a minute",
		total.String(), first.Name, more, first.Phase, first.Size.String(), first.Claim, uuid, volumeGroup, uuid)
}

func (e *Env) defaults() {
	if e.Progress == nil {
		e.Progress = precheck.NoProgress()
	}
	if e.Poll == 0 {
		e.Poll = 5 * time.Second
	}
	if e.Timeout == 0 {
		e.Timeout = 10 * time.Minute
	}
}

type planner struct {
	Env
	o   Options
	res *Result
}

func (p *planner) block(format string, a ...any) {
	p.res.Blockers = append(p.res.Blockers, fmt.Sprintf(format, a...))
}

// add appends a target if the object exists.
func (p *planner) add(ctx context.Context, t Target) error {
	_, err := p.Dyn.Resource(t.gvr).Namespace(t.Namespace).Get(ctx, t.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", t.Ref(), err)
	}
	p.res.Targets = append(p.res.Targets, t)
	return nil
}

func (p *planner) plan(ctx context.Context) error {
	if err := p.planNamespace(ctx); err != nil {
		return err
	}
	if !p.o.Host {
		return nil
	}
	if err := p.checkHostUnused(ctx); err != nil {
		return err
	}
	for _, t := range []Target{
		{Kind: "HyperConverged", Namespace: cluster.CNVNamespace, Name: cluster.HyperConvergedName, gvr: cluster.GVRHyperConverged, wait: true, step: manifests.StepHyperConverged},
		{Kind: "Namespace", Name: cluster.OSImagesNamespace, Detail: "boot-source images", gvr: cluster.GVRNamespace, wait: true, step: manifests.StepHyperConverged},
	} {
		if err := p.add(ctx, t); err != nil {
			return err
		}
	}
	if err := p.planOperator(ctx, cluster.CNV); err != nil {
		return err
	}
	lc := Target{Kind: "LVMCluster", Namespace: cluster.StorageNamespace, Name: cluster.LVMClusterName,
		Detail: "releases the disk", gvr: precheck.GVRLVMCluster, wait: true, step: manifests.StepStorage}
	if err := p.add(ctx, lc); err != nil {
		return err
	}
	return p.planOperator(ctx, cluster.LVMS)
}

// planNamespace adds the environment namespace, if vsno created it.
func (p *planner) planNamespace(ctx context.Context) error {
	ns := p.o.Namespace
	u, err := p.Dyn.Resource(cluster.GVRNamespace).Get(ctx, ns, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading namespace %s: %w", ns, err)
	}
	created, err := cluster.CreatedByVsno(ctx, p.Kube, ns, u.GetLabels())
	if err != nil {
		return err
	}
	if !created {
		p.block("namespace %s was not created by vsno", ns)
	}
	vms, err := p.list(ctx, cluster.GVRVirtualMachine, ns)
	if err != nil {
		return err
	}
	pvcs, err := p.Kube.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing PVCs in %s: %w", ns, err)
	}
	p.res.Targets = append(p.res.Targets, Target{Kind: "Namespace", Name: ns, gvr: cluster.GVRNamespace, wait: true, step: manifests.StepNamespace,
		Detail: fmt.Sprintf("%d VM(s)%s, %d PVC(s)", len(vms), names(vms), len(pvcs.Items))})
	ours := setupPVCs(ns)
	for _, vm := range vms {
		if name := vm.GetLabels()[guest.LabelName]; name != "" {
			// A vSNO: its disks go with it, so name it once.
			p.res.Unmanaged = append(p.res.Unmanaged, "vSNO "+name)
			ours[name+"-root"], ours[name+"-iso"] = true, true
			continue
		}
		p.res.Unmanaged = append(p.res.Unmanaged, "VM "+vm.GetName())
	}
	for _, pvc := range pvcs.Items {
		if !ours[pvc.Name] {
			p.res.Unmanaged = append(p.res.Unmanaged, "PVC "+pvc.Name)
		}
	}
	return nil
}

// checkHostUnused blocks when something besides this environment still uses
// OpenShift Virtualization or LVMS.
func (p *planner) checkHostUnused(ctx context.Context) error {
	for _, check := range []func(context.Context) error{p.checkVMs, p.checkPVCs, p.checkLVMClusters} {
		if err := check(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (p *planner) checkVMs(ctx context.Context) error {
	vms, err := p.list(ctx, cluster.GVRVirtualMachine, "")
	if err != nil {
		return err
	}
	var others []string
	for _, vm := range vms {
		if vm.GetNamespace() != p.o.Namespace {
			others = append(others, vm.GetNamespace()+"/"+vm.GetName())
		}
	}
	if len(others) > 0 {
		p.block("VMs outside %s still exist: %s", p.o.Namespace, strings.Join(others, ", "))
	}
	return nil
}

// checkPVCs blocks on PVCs elsewhere that use a TopoLVM StorageClass. The
// boot-source images don't count: teardown deletes them first.
func (p *planner) checkPVCs(ctx context.Context) error {
	scs, err := p.Kube.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing StorageClasses: %w", err)
	}
	var lvms []string
	for _, sc := range scs.Items {
		if sc.Provisioner == cluster.TopoLVM {
			lvms = append(lvms, sc.Name)
		}
	}
	pvcs, err := p.Kube.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing PVCs: %w", err)
	}
	var others []string
	for _, pvc := range pvcs.Items {
		sc := pvc.Spec.StorageClassName
		if sc == nil || !slices.Contains(lvms, *sc) || pvc.Namespace == p.o.Namespace || pvc.Namespace == cluster.OSImagesNamespace {
			continue
		}
		others = append(others, pvc.Namespace+"/"+pvc.Name)
	}
	if len(others) > 0 {
		p.block("%d PVC(s) outside %s use LVMS storage: %s", len(others), p.o.Namespace, short(others))
	}
	return nil
}

func (p *planner) checkLVMClusters(ctx context.Context) error {
	lcs, err := p.list(ctx, precheck.GVRLVMCluster, cluster.StorageNamespace)
	if err != nil {
		return err
	}
	for _, lc := range lcs {
		if lc.GetName() != cluster.LVMClusterName {
			p.block("LVMCluster %s/%s was not created by vsno", lc.GetNamespace(), lc.GetName())
		}
	}
	return nil
}

// planOperator adds op's Subscriptions and CSVs, then its namespace.
func (p *planner) planOperator(ctx context.Context, op cluster.Operator) error {
	subs, err := p.list(ctx, cluster.GVRSubscription, op.Namespace)
	if err != nil {
		return err
	}
	for _, s := range subs {
		if pkg, _, _ := unstructured.NestedString(s.Object, "spec", "name"); pkg == op.Package {
			p.res.Targets = append(p.res.Targets, Target{Kind: "Subscription", Namespace: op.Namespace, Name: s.GetName(), gvr: cluster.GVRSubscription, step: op.Key})
		}
	}
	csvs, err := cluster.CSVs(ctx, p.Dyn, op)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("listing %s CSVs: %w", op.Key, err)
	}
	for _, c := range csvs {
		p.res.Targets = append(p.res.Targets, Target{Kind: "ClusterServiceVersion", Namespace: op.Namespace, Name: c.GetName(), gvr: cluster.GVRCSV, wait: true, step: op.Key})
	}
	return p.add(ctx, Target{Kind: "Namespace", Name: op.Namespace, gvr: cluster.GVRNamespace, step: op.Key})
}

// list returns the objects of gvr in ns ("" = all); a missing CRD means none.
func (p *planner) list(ctx context.Context, gvr schema.GroupVersionResource, ns string) ([]unstructured.Unstructured, error) {
	l, err := p.Dyn.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", gvr.Resource, err)
	}
	return l.Items, nil
}

// delete deletes t and, if t.wait, waits until it is gone.
func (p *planner) delete(ctx context.Context, t *Target) error {
	ps := p.Progress.Start("Deleting " + t.Ref())
	ri := p.Dyn.Resource(t.gvr).Namespace(t.Namespace)
	err := ri.Delete(ctx, t.Name, metav1.DeleteOptions{})
	switch {
	case apierrors.IsNotFound(err):
		t.Outcome = Gone
		ps.Done("already gone")
		return nil
	case err != nil:
		ps.Fail(err.Error())
		return fmt.Errorf("deleting %s: %w", t.Ref(), err)
	}
	t.Outcome = Deleted
	if t.wait {
		ps.Update("waiting until it is gone")
		err = wait.PollUntilContextTimeout(ctx, p.Poll, p.Timeout, true, func(ctx context.Context) (bool, error) {
			_, err := ri.Get(ctx, t.Name, metav1.GetOptions{})
			return apierrors.IsNotFound(err), nil
		})
		if err != nil {
			ps.Fail(err.Error())
			return fmt.Errorf("waiting for %s to be deleted: %w", t.Ref(), err)
		}
	}
	ps.Done("deleted")
	return nil
}

func names(objs []unstructured.Unstructured) string {
	if len(objs) == 0 {
		return ""
	}
	n := make([]string, 0, len(objs))
	for _, o := range objs {
		n = append(n, o.GetName())
	}
	return " (" + short(n) + ")"
}

// short lists up to 5 items.
func short(items []string) string {
	if len(items) <= 5 {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:5], ", ") + fmt.Sprintf(", and %d more", len(items)-5)
}

// setupPVCs are the names of the PVCs setup creates in ns.
func setupPVCs(ns string) map[string]bool {
	names := map[string]bool{}
	for _, st := range manifests.Steps {
		objs, err := st.Objects(manifests.NamesConfig(ns))
		if err != nil {
			continue // the templates are tested to render
		}
		for _, o := range objs {
			if o.GetKind() == "PersistentVolumeClaim" {
				names[o.GetName()] = true
			}
		}
	}
	return names
}
