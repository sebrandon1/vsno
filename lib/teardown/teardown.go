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

	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/state"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	wait      bool // wait until the object is gone before the next target
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
	Forced  bool     `json:"forced,omitempty"`
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
	if !o.Yes {
		answer := o.Namespace
		if o.Host {
			answer = "host"
		}
		if e.ShowPlan != nil {
			e.ShowPlan(res)
		}
		q := fmt.Sprintf("This deletes %d object(s), listed above, and everything in them. Type %q to continue", len(res.Targets), answer)
		if e.Confirm == nil || !e.Confirm(q, answer) {
			return res, errors.New("not confirmed; rerun with --yes to skip the question")
		}
	}
	for i := range res.Targets {
		if err := p.delete(ctx, &res.Targets[i]); err != nil {
			return res, err
		}
	}
	return res, nil
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
		{Kind: "HyperConverged", Namespace: cluster.CNVNamespace, Name: cluster.HyperConvergedName, gvr: precheck.GVRHyperConverged, wait: true},
		{Kind: "Namespace", Name: cluster.OSImagesNamespace, Detail: "boot-source images", gvr: cluster.GVRNamespace, wait: true},
	} {
		if err := p.add(ctx, t); err != nil {
			return err
		}
	}
	if err := p.planOperator(ctx, cluster.CNV); err != nil {
		return err
	}
	lc := Target{Kind: "LVMCluster", Namespace: cluster.StorageNamespace, Name: cluster.LVMClusterName,
		Detail: "releases the disk", gvr: precheck.GVRLVMCluster, wait: true}
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
	if !cluster.ManagedByVsno(u.GetLabels()) {
		// A state ConfigMap, even an unreadable one, means vsno set it up.
		_, found, err := state.Load(ctx, p.Kube, ns)
		if !found && err != nil {
			return err
		}
		if !found {
			p.block("namespace %s was not created by vsno", ns)
		}
	}
	vms, err := p.list(ctx, cluster.GVRVirtualMachine, ns)
	if err != nil {
		return err
	}
	pvcs, err := p.Kube.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing PVCs in %s: %w", ns, err)
	}
	p.res.Targets = append(p.res.Targets, Target{Kind: "Namespace", Name: ns, gvr: cluster.GVRNamespace, wait: true,
		Detail: fmt.Sprintf("%d VM(s)%s, %d PVC(s)", len(vms), names(vms), len(pvcs.Items))})
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
			p.res.Targets = append(p.res.Targets, Target{Kind: "Subscription", Namespace: op.Namespace, Name: s.GetName(), gvr: cluster.GVRSubscription})
		}
	}
	csvs, err := cluster.CSVs(ctx, p.Dyn, op)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("listing %s CSVs: %w", op.Key, err)
	}
	for _, c := range csvs {
		p.res.Targets = append(p.res.Targets, Target{Kind: "ClusterServiceVersion", Namespace: op.Namespace, Name: c.GetName(), gvr: precheck.GVRCSV, wait: true})
	}
	return p.add(ctx, Target{Kind: "Namespace", Name: op.Namespace, gvr: cluster.GVRNamespace})
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
