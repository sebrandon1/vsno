package cluster

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

const (
	csvV1 = "kubevirt-hyperconverged-operator.v1"
	csvV2 = "kubevirt-hyperconverged-operator.v2"
	csvV3 = "kubevirt-hyperconverged-operator.v3"
)

// subscription builds a CNV Subscription.
func subscription(channel, approval, installed, current, state string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec":   map[string]any{"name": CNV.Package, "channel": channel},
		"status": map[string]any{"installedCSV": installed, "currentCSV": current, "state": state},
	}}
	if approval != "" {
		_ = unstructured.SetNestedField(u.Object, approval, "spec", "installPlanApproval")
	}
	u.SetAPIVersion("operators.coreos.com/v1alpha1")
	u.SetKind("Subscription")
	u.SetNamespace(CNV.Namespace)
	u.SetName("hco-operatorhub")
	return u
}

// manifest builds CNV's PackageManifest, whose channels map to their currentCSV.
func manifest(channels map[string]string) *unstructured.Unstructured {
	chs := make([]any, 0, len(channels))
	for _, n := range slices.Sorted(maps.Keys(channels)) {
		chs = append(chs, map[string]any{"name": n, "currentCSV": channels[n]})
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"defaultChannel": "stable", "channels": chs},
	}}
	u.SetAPIVersion("packages.operators.coreos.com/v1")
	u.SetKind("PackageManifest")
	u.SetNamespace(MarketplaceNamespace)
	u.SetName(CNV.Package)
	return u
}

// installPlan builds an InstallPlan that would install csvs.
func installPlan(name string, approved bool, phase string, csvs ...string) *unstructured.Unstructured {
	names := make([]any, 0, len(csvs))
	for _, c := range csvs {
		names = append(names, c)
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec":   map[string]any{"approved": approved, "clusterServiceVersionNames": names},
		"status": map[string]any{"phase": phase},
	}}
	u.SetAPIVersion("operators.coreos.com/v1alpha1")
	u.SetKind("InstallPlan")
	u.SetNamespace(CNV.Namespace)
	u.SetName(name)
	return u
}

func TestOperatorUpdate(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		objs []runtime.Object
		// suggested is the channel precheck picked.
		suggested string
		want      string // the update's String(), or "" for none
		wantSug   string
		wantPlan  string
	}{
		{
			name: "up to date",
			objs: []runtime.Object{csv(CNV.Namespace, csvV1, "Succeeded"),
				subscription("stable", "", csvV1, csvV1, "AtLatestKnown"), manifest(map[string]string{"stable": csvV1})},
		},
		{
			name: "newer CSV in the channel",
			objs: []runtime.Object{csv(CNV.Namespace, csvV1, "Succeeded"),
				subscription("stable", "", csvV1, csvV1, "UpgradeAvailable"), manifest(map[string]string{"stable": csvV2})},
			want: csvV1 + " → " + csvV2,
		},
		{
			name: "upgrade pending, catalog matches",
			objs: []runtime.Object{csv(CNV.Namespace, csvV1, "Succeeded"),
				subscription("stable", "", csvV1, csvV1, "UpgradePending"), manifest(map[string]string{"stable": csvV1})},
			want: csvV1 + " → " + csvV1,
		},
		{
			name: "manual subscription with an InstallPlan to approve",
			objs: []runtime.Object{csv(CNV.Namespace, csvV1, "Succeeded"),
				subscription("stable", "Manual", csvV1, csvV2, "UpgradePending"),
				manifest(map[string]string{"stable": csvV1}),
				installPlan("install-abc", false, "RequiresApproval", csvV2)},
			want: csvV1 + " → " + csvV1, wantPlan: "install-abc",
		},
		{
			name: "manual subscription with its plan already approved",
			objs: []runtime.Object{csv(CNV.Namespace, csvV1, "Succeeded"),
				subscription("stable", "Manual", csvV1, csvV1, "AtLatestKnown"),
				installPlan("install-abc", true, "Complete", csvV1)},
			want: "",
		},
		{
			name: "channel behind the cluster",
			objs: []runtime.Object{csv(CNV.Namespace, csvV1, "Succeeded"),
				subscription("stable-4.21", "", csvV1, csvV1, "AtLatestKnown"),
				manifest(map[string]string{"stable-4.21": csvV1, "stable-4.22": csvV3})},
			suggested: "stable-4.22",
			want:      csvV1 + " → " + csvV3, wantSug: "stable-4.22",
		},
		{
			name: "no subscription",
			objs: []runtime.Object{csv(CNV.Namespace, csvV1, "Succeeded")},
		},
		{
			name: "no package manifest",
			objs: []runtime.Object{csv(CNV.Namespace, csvV1, "Succeeded"),
				subscription("stable", "", csvV1, csvV1, "AtLatestKnown")},
		},
		{
			name: "channel the catalog doesn't list",
			objs: []runtime.Object{csv(CNV.Namespace, csvV1, "Succeeded"),
				subscription("beta", "", csvV1, csvV1, "AtLatestKnown"),
				manifest(map[string]string{"stable": csvV2})},
		},
		{
			name: "not installed",
			objs: []runtime.Object{subscription("stable", "", "", "", "")},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, err := OperatorUpdate(ctx, fakeDyn(c.objs...), CNV, c.suggested)
			if err != nil {
				t.Fatal(err)
			}
			if c.want == "" {
				if u != nil {
					t.Fatalf("update = %+v, want none", u)
				}
				return
			}
			if u == nil {
				t.Fatalf("update = nil, want %q", c.want)
			}
			if got := u.String(); got != c.want {
				t.Errorf("String() = %q, want %q", got, c.want)
			}
			if u.Suggested != c.wantSug || u.Plan != c.wantPlan || u.Operator != CNV.Key || u.Approval == "" {
				t.Errorf("update = %+v", u)
			}
		})
	}
}

func TestOperatorUpdateErrors(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct{ name, resource string }{
		{"listing subscriptions", "subscriptions"},
		{"listing installplans", "installplans"},
		{"reading the catalog", "packagemanifests"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dyn := fakeDyn(csv(CNV.Namespace, csvV1, "Succeeded"),
				subscription("stable", "Manual", csvV1, csvV2, "UpgradePending"))
			dyn.PrependReactor("list", c.resource, boom)
			if c.resource == "packagemanifests" {
				dyn.PrependReactor("get", c.resource, boom)
			}
			if _, err := OperatorUpdate(ctx, dyn, CNV, ""); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func boom(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("api down") }

func TestUpdateString(t *testing.T) {
	cases := []struct {
		u    Update
		want string
	}{
		{Update{Installed: "a", Available: "b"}, "a → b"},
		{Update{Installed: "a"}, "a → upgrading"},
	}
	for _, c := range cases {
		if got := c.u.String(); got != c.want {
			t.Errorf("%+v = %q, want %q", c.u, got, c.want)
		}
	}
}

func TestSubscriptionStatusOf(t *testing.T) {
	if s := SubscriptionStatusOf(subscription("stable", "", "a", "b", "AtLatestKnown")); s.Approval != "Automatic" {
		t.Errorf("approval = %q, want Automatic", s.Approval)
	}
	s := SubscriptionStatusOf(subscription("stable", "Manual", "a", "b", "UpgradePending"))
	if s.Channel != "stable" || s.Approval != "Manual" || s.Installed != "a" || s.Current != "b" || s.State != "UpgradePending" {
		t.Errorf("status = %+v", s)
	}
	if s := SubscriptionStatusOf(&unstructured.Unstructured{Object: map[string]any{}}); s.Approval != "Automatic" {
		t.Errorf("empty approval = %q", s.Approval)
	}
}

func TestInstalledCSV(t *testing.T) {
	ctx := context.Background()
	// The Subscription's installedCSV wins over a Succeeded CSV, because OLM
	// records the running one there.
	dyn := fakeDyn(csv(CNV.Namespace, csvV1, "Succeeded"), subscription("stable", "", csvV2, csvV2, "AtLatestKnown"))
	if got, err := InstalledCSV(ctx, dyn, CNV); err != nil || got != csvV2 {
		t.Errorf("installed = %q %v, want %q", got, err, csvV2)
	}
	// Without a Subscription, the Succeeded CSV is all there is.
	if got, err := InstalledCSV(ctx, fakeDyn(csv(CNV.Namespace, csvV1, "Succeeded")), CNV); err != nil || got != csvV1 {
		t.Errorf("installed = %q %v, want %q", got, err, csvV1)
	}
}

func TestApprove(t *testing.T) {
	ctx := context.Background()
	dyn := fakeDyn(installPlan("install-abc", false, "RequiresApproval", csvV2))
	if err := Approve(ctx, dyn, CNV.Namespace, "install-abc"); err != nil {
		t.Fatal(err)
	}
	got, err := dyn.Resource(GVRInstallPlan).Namespace(CNV.Namespace).Get(ctx, "install-abc", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if approved, _, _ := unstructured.NestedBool(got.Object, "spec", "approved"); !approved {
		t.Error("plan not approved")
	}
	dyn.PrependReactor("patch", "installplans", boom)
	if err := Approve(ctx, dyn, CNV.Namespace, "install-abc"); err == nil || !strings.Contains(err.Error(), "install-abc") {
		t.Errorf("err = %v", err)
	}
}

func TestApprovePending(t *testing.T) {
	ctx := context.Background()
	dyn := fakeDyn(csv(CNV.Namespace, csvV1, "Succeeded"),
		installPlan("install-new", false, "RequiresApproval", csvV2),
		installPlan("install-old", false, "RequiresApproval", csvV1))
	found, err := ApprovePending(ctx, dyn, CNV, csvV1)
	if err != nil || !found {
		t.Fatalf("found = %v, err = %v", found, err)
	}
	// The plan that installs the running CSV isn't the upgrade.
	got, err := dyn.Resource(GVRInstallPlan).Namespace(CNV.Namespace).Get(ctx, "install-new", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if approved, _, _ := unstructured.NestedBool(got.Object, "spec", "approved"); !approved {
		t.Error("the upgrade's plan was not approved")
	}
	if _, err := ApprovePending(ctx, fakeDyn(), CNV, csvV1); err != nil {
		t.Error(err)
	}
	dyn.PrependReactor("list", "installplans", boom)
	if _, err := ApprovePending(ctx, dyn, CNV, csvV1); err == nil {
		t.Error("want an error")
	}
}

func TestSetChannel(t *testing.T) {
	ctx := context.Background()
	sub := subscription("stable-4.21", "", csvV1, csvV1, "AtLatestKnown")
	dyn := fakeDyn(sub)
	if err := SetChannel(ctx, dyn, sub, "stable-4.22"); err != nil {
		t.Fatal(err)
	}
	got, err := dyn.Resource(GVRSubscription).Namespace(CNV.Namespace).Get(ctx, sub.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if c, _, _ := unstructured.NestedString(got.Object, "spec", "channel"); c != "stable-4.22" {
		t.Errorf("channel = %q", c)
	}
	// The package name survives the patch.
	if n, _, _ := unstructured.NestedString(got.Object, "spec", "name"); n != CNV.Package {
		t.Errorf("package = %q", n)
	}
	dyn.PrependReactor("patch", "subscriptions", boom)
	if err := SetChannel(ctx, dyn, sub, "stable"); err == nil || !strings.Contains(err.Error(), "to channel stable") {
		t.Errorf("err = %v", err)
	}
}
