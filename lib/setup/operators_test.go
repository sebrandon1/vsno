package setup

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/state"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

// cnvNewCSV is the CSV CNV's channel offers in the upgrade tests.
const cnvNewCSV = "kubevirt-hyperconverged-operator.v1.1.0"

// cnvSub builds CNV's Subscription, running cnvCSV until OLM moves current to
// current.
func cnvSub(channel, approval, current string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec":   map[string]any{"name": cluster.CNV.Package, "channel": channel},
		"status": map[string]any{"installedCSV": cnvCSV, "currentCSV": current, "state": "AtLatestKnown"},
	}}
	if approval != "" {
		_ = unstructured.SetNestedField(u.Object, approval, "spec", "installPlanApproval")
	}
	u.SetAPIVersion("operators.coreos.com/v1alpha1")
	u.SetKind("Subscription")
	u.SetNamespace(cluster.CNV.Namespace)
	u.SetName("hco-operatorhub")
	return u
}

// manifestObj builds pkg's PackageManifest, whose channel1 head is csv.
func manifestObj(pkg, csv string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"defaultChannel": channel1,
			"channels": []any{map[string]any{"name": channel1, "currentCSV": csv}}},
	}}
	u.SetAPIVersion("packages.operators.coreos.com/v1")
	u.SetKind("PackageManifest")
	u.SetNamespace(cluster.MarketplaceNamespace)
	u.SetName(pkg)
	return u
}

// manifestOn builds pkg's PackageManifest with channel as its only channel.
func manifestOn(pkg, channel, csv string) *unstructured.Unstructured {
	u := manifestObj(pkg, csv)
	_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{"name": channel, "currentCSV": csv}}, "status", "channels")
	_ = unstructured.SetNestedField(u.Object, channel, "status", "defaultChannel")
	return u
}

// installPlanObj builds an InstallPlan that would install csv.
func installPlanObj(ns, name, csv string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec":   map[string]any{"approved": false, "clusterServiceVersionNames": []any{csv}},
		"status": map[string]any{"phase": "RequiresApproval"},
	}}
	u.SetAPIVersion("operators.coreos.com/v1alpha1")
	u.SetKind("InstallPlan")
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

func (s *sim) create(t *testing.T, gvr schema.GroupVersionResource, objs ...*unstructured.Unstructured) {
	t.Helper()
	for _, o := range objs {
		if err := s.dyn.Tracker().Create(gvr, o, o.GetNamespace()); err != nil {
			t.Fatal(err)
		}
	}
}

// outdatedCNV makes CNV look installed but behind its channel: it runs cnvCSV,
// and the catalog offers cnvNewCSV on the same channel.
func (s *sim) outdatedCNV(t *testing.T) {
	t.Helper()
	s.create(t, cluster.GVRSubscription, cnvSub(channel1, "", cnvCSV))
	s.create(t, cluster.GVRPackageManifest, manifestObj(cluster.CNV.Package, cnvNewCSV))
}

// finishUpgrade makes OLM finish op's upgrade: the Subscription runs new and
// its CSV has Succeeded.
func (s *sim) finishUpgrade(t *testing.T, op cluster.Operator, new string) {
	t.Helper()
	ctx := context.Background()
	l, err := s.dyn.Resource(cluster.GVRSubscription).Namespace(op.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil || len(l.Items) == 0 {
		t.Fatalf("no Subscription to upgrade: %v", err)
	}
	u := l.Items[0].DeepCopy()
	_ = unstructured.SetNestedField(u.Object, new, "status", "installedCSV")
	_ = unstructured.SetNestedField(u.Object, new, "status", "currentCSV")
	_ = unstructured.SetNestedField(u.Object, "AtLatestKnown", "status", "state")
	if err := s.dyn.Tracker().Update(cluster.GVRSubscription, u, op.Namespace); err != nil {
		t.Fatal(err)
	}
	_ = s.dyn.Tracker().Create(cluster.GVRCSV, csv(op.Namespace, new), op.Namespace)
}

// progressWhen runs fn when a step reports detail. setup reports
// "waiting for the new CSV to succeed" once it has asked OLM to upgrade, which
// is when a real OLM starts work.
type progressWhen struct {
	detail string
	fn     func()
}

func (p *progressWhen) Start(string) precheck.ProgressStep { return p }
func (p *progressWhen) Update(detail string) {
	if detail == p.detail {
		p.fn()
	}
}
func (p *progressWhen) SetProgress(int, int) {}
func (p *progressWhen) Done(string)          {}
func (p *progressWhen) Warn(string)          {}
func (p *progressWhen) Fail(string)          {}

// upgradeOnWait has the sim finish CNV's upgrade when setup waits for it, as
// OLM would.
func (s *sim) upgradeOnWait(t *testing.T, e *Env) {
	e.Progress = &progressWhen{detail: "waiting for the new CSV to succeed", fn: func() {
		s.finishUpgrade(t, cluster.CNV, cnvNewCSV)
	}}
}

func stepOf(res *Result, name string) Step {
	for _, s := range res.Steps {
		if s.Name == name {
			return s
		}
	}
	return Step{}
}

func hasWarning(res *Result, want string) bool {
	for _, w := range res.Warnings {
		if strings.Contains(w, want) {
			return true
		}
	}
	return false
}

func TestRunUpgradesOperator(t *testing.T) {
	s := newSim()
	s.existingHost(t)
	s.outdatedCNV(t)
	e := s.env(report())
	s.upgradeOnWait(t, &e)
	o := opts()
	o.UpgradeOperators = true

	res, err := Run(context.Background(), e, o)
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomes(res)["cnv"]; got != Upgraded {
		t.Fatalf("cnv = %q, want upgraded (%+v)", got, res.Steps)
	}
	step := stepOf(res, "cnv")
	if !strings.Contains(step.Detail, cnvCSV+" → "+cnvNewCSV) || !strings.Contains(step.Detail, cnvNewCSV) {
		t.Errorf("detail = %q", step.Detail)
	}
	if len(res.Updates) != 1 || res.Updates[0].Operator != cluster.CNV.Key || res.Updates[0].Installed != cnvCSV {
		t.Errorf("updates = %+v", res.Updates)
	}
	if got := outcomes(res)["lvms"]; got != Reused {
		t.Errorf("lvms = %q: it had no update", got)
	}
	st, _, err := state.Load(context.Background(), s.kube, ns)
	if err != nil {
		t.Fatal(err)
	}
	if st.Versions["cnv"] != cnvNewCSV {
		t.Errorf("recorded cnv = %q, want %q", st.Versions["cnv"], cnvNewCSV)
	}
}

func TestRunOperatorUpgradePrompt(t *testing.T) {
	cases := []struct {
		name    string
		confirm func(q, a string) bool
		yes     bool
		upgrade bool
		want    Outcome
	}{
		{"typed y", func(_, a string) bool { return a == "y" }, false, false, Upgraded},
		{"declined", func(string, string) bool { return false }, false, false, Reused},
		{"no terminal", nil, false, false, Reused},
		{"--yes alone", nil, true, false, Reused},
		{"--upgrade-operators", nil, false, true, Upgraded},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newSim()
			s.existingHost(t)
			s.outdatedCNV(t)
			e := s.env(report())
			s.upgradeOnWait(t, &e)
			e.Confirm = c.confirm
			o := opts()
			o.Yes, o.UpgradeOperators = c.yes, c.upgrade

			res, err := Run(context.Background(), e, o)
			if err != nil {
				t.Fatal(err)
			}
			if got := outcomes(res)["cnv"]; got != c.want {
				t.Errorf("cnv = %q, want %q", got, c.want)
			}
			if applied := c.want == Upgraded; applied == hasWarning(res, "not applied") {
				t.Errorf("applied=%v, warnings = %v", applied, res.Warnings)
			}
		})
	}
}

func TestRunApprovesManualInstallPlan(t *testing.T) {
	ctx := context.Background()
	s := newSim()
	s.existingHost(t)
	// The catalog doesn't say what comes next; only the InstallPlan is newer.
	s.create(t, cluster.GVRSubscription,
		cnvSub(channel1, "Manual", cnvNewCSV))
	s.create(t, cluster.GVRInstallPlan, installPlanObj(cluster.CNV.Namespace, "install-abc", cnvNewCSV))
	e := s.env(report())
	s.upgradeOnWait(t, &e)
	o := opts()
	o.UpgradeOperators = true

	res, err := Run(ctx, e, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Updates) != 1 || res.Updates[0].Plan != "install-abc" {
		t.Errorf("updates = %+v", res.Updates)
	}
	if got := outcomes(res)["cnv"]; got != Upgraded {
		t.Errorf("cnv = %q", got)
	}
	plan, err := s.dyn.Resource(cluster.GVRInstallPlan).Namespace(cluster.CNV.Namespace).Get(ctx, "install-abc", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if approved, _, _ := unstructured.NestedBool(plan.Object, "spec", "approved"); !approved {
		t.Error("InstallPlan was not approved")
	}
}

func TestRunOperatorUpgradePatchesChannel(t *testing.T) {
	ctx := context.Background()
	s := newSim()
	s.existingHost(t)
	s.create(t, cluster.GVRSubscription,
		cnvSub("stable-4.21", "", cnvCSV))
	s.create(t, cluster.GVRPackageManifest, manifestOn(cluster.CNV.Package, "stable-4.22", cnvNewCSV))
	rep := report()
	rep.Suggested.CNVChannel = "stable-4.22"
	e := s.env(rep)
	s.upgradeOnWait(t, &e)
	o := opts()
	o.UpgradeOperators = true

	res, err := Run(ctx, e, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Updates) != 1 || res.Updates[0].Suggested != "stable-4.22" || res.Updates[0].Channel != "stable-4.21" {
		t.Fatalf("updates = %+v", res.Updates)
	}
	sub, err := s.dyn.Resource(cluster.GVRSubscription).Namespace(cluster.CNV.Namespace).Get(ctx, "hco-operatorhub", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ch, _, _ := unstructured.NestedString(sub.Object, "spec", "channel"); ch != "stable-4.22" {
		t.Errorf("channel = %q, want stable-4.22", ch)
	}
	if got := outcomes(res)["cnv"]; got != Upgraded {
		t.Errorf("cnv = %q", got)
	}
}

func TestRunCNVUpgradeNamesRunningVMs(t *testing.T) {
	s := newSim()
	s.existingHost(t)
	s.outdatedCNV(t)
	vm := &unstructured.Unstructured{Object: map[string]any{
		"spec":   map[string]any{"running": true},
		"status": map[string]any{"printableStatus": "Running"},
	}}
	vm.SetAPIVersion("kubevirt.io/v1")
	vm.SetKind("VirtualMachine")
	vm.SetNamespace("vsno-dev")
	vm.SetName("seed")
	// vsno's own VMs use a runStrategy instead of spec.running; a stopped one
	// is left out.
	strategy := func(name, status string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{
			"spec":   map[string]any{"runStrategy": "Always"},
			"status": map[string]any{"printableStatus": status},
		}}
		u.SetAPIVersion("kubevirt.io/v1")
		u.SetKind("VirtualMachine")
		u.SetNamespace("vsno-dev")
		u.SetName(name)
		return u
	}
	s.create(t, cluster.GVRVirtualMachine, vm, strategy("target", "Running"), strategy("asleep", "Stopped"))

	var asked string
	e := s.env(report())
	e.Confirm = func(q, _ string) bool { asked = q; return false }

	res, err := Run(context.Background(), e, opts())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(asked, "vsno-dev/seed, vsno-dev/target") || !strings.Contains(asked, "cnv") {
		t.Errorf("question = %q", asked)
	}
	if !hasWarning(res, "affects 2 running VM(s): vsno-dev/seed, vsno-dev/target") {
		t.Errorf("warnings = %v", res.Warnings)
	}
	if got := outcomes(res)["cnv"]; got != Reused {
		t.Errorf("declined upgrade: cnv = %q", got)
	}
}

func TestRunDryRunPlansOperatorUpgrade(t *testing.T) {
	ctx := context.Background()
	s := newSim()
	s.existingHost(t)
	s.outdatedCNV(t)
	e := s.env(report())
	e.Confirm = func(string, string) bool { t.Error("a dry run must not ask"); return false }
	o := opts()
	o.DryRun = true

	res, err := Run(ctx, e, o)
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomes(res)["cnv"]; got != Planned {
		t.Fatalf("cnv = %q", got)
	}
	if d := stepOf(res, "cnv").Detail; d != "upgrade "+cnvCSV+" → "+cnvNewCSV {
		t.Errorf("detail = %q", d)
	}
	sub, err := s.dyn.Resource(cluster.GVRSubscription).Namespace(cluster.CNV.Namespace).Get(ctx, "hco-operatorhub", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if installed, _, _ := unstructured.NestedString(sub.Object, "status", "installedCSV"); installed != cnvCSV {
		t.Errorf("a dry run changed the Subscription: installed %q", installed)
	}
}

func TestRunWarnsAboutHubManagedUpgrade(t *testing.T) {
	s := newSim()
	s.existingHost(t)
	s.outdatedCNV(t)
	rep := report()
	rep.HubManaged = true
	e := s.env(rep)
	s.upgradeOnWait(t, &e)
	o := opts()
	o.UpgradeOperators = true

	res, err := Run(context.Background(), e, o)
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(res, "ACM hub") {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

func TestRunLeavesCurrentOperatorsAlone(t *testing.T) {
	s := newSim()
	s.existingHost(t)
	// Both Subscriptions are already at their channel's head.
	s.create(t, cluster.GVRSubscription, cnvSub(channel1, "", cnvCSV))
	s.create(t, cluster.GVRPackageManifest, manifestObj(cluster.CNV.Package, cnvCSV))
	e := s.env(report())
	e.Confirm = func(string, string) bool { t.Error("nothing to ask"); return false }

	res, err := Run(context.Background(), e, opts())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Updates) != 0 || len(res.Warnings) != 0 {
		t.Errorf("updates = %+v, warnings = %v", res.Updates, res.Warnings)
	}
	if got := outcomes(res)["cnv"]; got != Reused {
		t.Errorf("cnv = %q", got)
	}
}

func TestRunWarnsWhenTheCatalogFails(t *testing.T) {
	s := newSim()
	s.existingHost(t)
	s.dyn.PrependReactor("list", "subscriptions", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("api down")
	})
	res, err := Run(context.Background(), s.env(report()), opts())
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(res, "cannot tell whether cnv is up to date") || !hasWarning(res, "cannot tell whether lvms is up to date") {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

func TestSummarize(t *testing.T) {
	if got := summarize([]string{"vsno/a", "vsno/b"}); got != "vsno/a, vsno/b" {
		t.Errorf("summarize = %q", got)
	}
	got := summarize([]string{"1", "2", "3", "4", "5", "6"})
	if got != "1, 2, 3, 4, 5 and 1 more" {
		t.Errorf("summarize = %q", got)
	}
}

func TestRunOperatorUpgradeMovesOnlyTheChannel(t *testing.T) {
	ctx := context.Background()
	s := newSim()
	s.existingHost(t)
	// Both channels point at the CSV that runs, so only the channel moves and
	// there is no new CSV to wait for.
	s.create(t, cluster.GVRSubscription, cnvSub("stable-4.21", "", cnvCSV))
	s.create(t, cluster.GVRPackageManifest, manifestOn(cluster.CNV.Package, "stable-4.22", cnvCSV))
	rep := report()
	rep.Suggested.CNVChannel = "stable-4.22"
	e := s.env(rep)
	o := opts()
	o.UpgradeOperators = true

	res, err := Run(ctx, e, o)
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomes(res)["cnv"]; got != Upgraded {
		t.Fatalf("cnv = %q", got)
	}
	if d := stepOf(res, "cnv").Detail; d != "channel stable-4.21 → stable-4.22" {
		t.Errorf("detail = %q", d)
	}
	sub, err := s.dyn.Resource(cluster.GVRSubscription).Namespace(cluster.CNV.Namespace).Get(ctx, "hco-operatorhub", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ch, _, _ := unstructured.NestedString(sub.Object, "spec", "channel"); ch != "stable-4.22" {
		t.Errorf("channel = %q, want stable-4.22", ch)
	}
}

func TestRunRequestedChannel(t *testing.T) {
	ctx := context.Background()
	s := newSim()
	s.existingHost(t)
	// CNV runs stable's CSV; candidate has a newer one.
	s.create(t, cluster.GVRSubscription, cnvSub(channel1, "", cnvCSV))
	s.create(t, cluster.GVRPackageManifest, manifestOn(cluster.CNV.Package, "candidate", cnvNewCSV))
	e := s.env(report())
	s.upgradeOnWait(t, &e)
	e.Confirm = func(string, string) bool { t.Error("--operator-channel must not ask"); return false }
	o := opts()
	o.Channels = map[string]string{cluster.CNV.Key: "candidate"}

	res, err := Run(ctx, e, o)
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomes(res)["cnv"]; got != Upgraded {
		t.Fatalf("cnv = %q", got)
	}
	sub, err := s.dyn.Resource(cluster.GVRSubscription).Namespace(cluster.CNV.Namespace).Get(ctx, "hco-operatorhub", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ch, _, _ := unstructured.NestedString(sub.Object, "spec", "channel"); ch != "candidate" {
		t.Errorf("channel = %q, want candidate", ch)
	}
}

func TestRunRequestedChannelThatCantBeHonoured(t *testing.T) {
	s := newSim()
	s.existingHost(t)
	s.create(t, cluster.GVRSubscription, cnvSub(channel1, "", cnvCSV))
	s.create(t, cluster.GVRPackageManifest, manifestObj(cluster.CNV.Package, cnvCSV))
	o := opts()
	o.Channels = map[string]string{cluster.CNV.Key: "beta"}

	_, err := Run(context.Background(), s.env(report()), o)
	if err == nil || !strings.Contains(err.Error(), "no beta channel") {
		t.Fatalf("err = %v", err)
	}
}
