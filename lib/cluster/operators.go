package cluster

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// SubscriptionStatus is the part of an operator's Subscription vsno reads.
type SubscriptionStatus struct {
	Channel   string // spec.channel
	Approval  string // spec.installPlanApproval; Automatic when unset
	Installed string // status.installedCSV
	Current   string // status.currentCSV
	State     string // status.state, e.g. AtLatestKnown or UpgradePending
}

// SubscriptionStatusOf reads a Subscription.
func SubscriptionStatusOf(sub *unstructured.Unstructured) SubscriptionStatus {
	s := SubscriptionStatus{
		Channel:   nested(sub, "spec", "channel"),
		Approval:  nested(sub, "spec", "installPlanApproval"),
		Installed: nested(sub, "status", "installedCSV"),
		Current:   nested(sub, "status", "currentCSV"),
		State:     nested(sub, "status", "state"),
	}
	if s.Approval == "" {
		s.Approval = "Automatic"
	}
	return s
}

// Update is an operator whose installed CSV isn't what its channel offers.
type Update struct {
	Operator  string `json:"operator"`  // cnv or lvms
	Installed string `json:"installed"` // the CSV that runs
	// Available is the CSV at the head of the channel to use; it is empty when
	// the catalog doesn't say, as with an upgrade OLM has already started.
	Available string `json:"available,omitempty"`
	Channel   string `json:"channel"` // the Subscription's channel
	// Suggested is the channel precheck picks when it differs from Channel, so
	// an upgrade has to move the Subscription first.
	Suggested string `json:"suggested,omitempty"`
	Approval  string `json:"approval"`       // Automatic or Manual
	Plan      string `json:"plan,omitempty"` // InstallPlan waiting for approval
}

// SameCSV reports whether the channel to move to already runs this very CSV, so
// the Subscription's channel is the only thing to change.
func (u Update) SameCSV() bool {
	return u.Suggested != "" && (u.Available == "" || u.Available == u.Installed)
}

// String is "<installed> → <available>", "channel <old> → <new>" when only the
// channel moves, or "<installed> → upgrading" when the catalog can't say what
// the update installs.
func (u Update) String() string {
	switch {
	case u.SameCSV():
		return "channel " + u.Channel + " → " + u.Suggested
	case u.Available == "":
		return u.Installed + " → upgrading"
	}
	return u.Installed + " → " + u.Available
}

// Subscription returns op's Subscription, or nil when it has none (an operator
// installed without one, or none installed at all).
func Subscription(ctx context.Context, dyn dynamic.Interface, op Operator) (*unstructured.Unstructured, error) {
	l, err := dyn.Resource(GVRSubscription).Namespace(op.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing the Subscriptions in %s: %w", op.Namespace, err)
	}
	for i := range l.Items {
		if nested(&l.Items[i], "spec", "name") == op.Package {
			return &l.Items[i], nil
		}
	}
	return nil, nil
}

// InstalledCSV returns the CSV op runs: its Subscription's status.installedCSV,
// or its first Succeeded CSV when there is no Subscription to ask.
func InstalledCSV(ctx context.Context, dyn dynamic.Interface, op Operator) (string, error) {
	sub, err := Subscription(ctx, dyn, op)
	if err != nil {
		return "", err
	}
	if sub != nil {
		if s := SubscriptionStatusOf(sub).Installed; s != "" {
			return s, nil
		}
	}
	return SucceededCSV(ctx, dyn, op)
}

// OperatorUpdate reports how op is out of date, or nil when it isn't: the
// installed CSV lags the channel (the suggested one when precheck picked a
// different channel), the Subscription is waiting to upgrade, or a Manual
// Subscription has an InstallPlan to approve. It is nil too when the operator
// isn't installed or the catalog can't be read, so callers only warn.
func OperatorUpdate(ctx context.Context, dyn dynamic.Interface, op Operator, suggested string) (*Update, error) {
	sub, err := Subscription(ctx, dyn, op)
	if err != nil || sub == nil {
		return nil, err
	}
	st := SubscriptionStatusOf(sub)
	installed, err := InstalledCSV(ctx, dyn, op)
	if err != nil || installed == "" {
		return nil, err
	}
	u := &Update{Operator: op.Key, Installed: installed, Channel: st.Channel, Approval: st.Approval}
	if u.Available, u.Suggested, err = targetCSV(ctx, dyn, op.Package, st, suggested); err != nil {
		return nil, err
	}
	if st.Approval == "Manual" {
		if u.Plan, err = pendingPlan(ctx, dyn, op, installed); err != nil {
			return nil, err
		}
	}
	newer := u.Available != "" && u.Available != installed
	if !newer && u.Suggested == "" && st.State != "UpgradePending" && u.Plan == "" {
		return nil, nil
	}
	return u, nil
}

// targetCSV returns the CSV an upgrade would install and the channel to move to
// when the Subscription is behind the suggested one. The suggestion is only
// used for a version channel, the one precheck judges against the cluster
// version (stable-4.22): a maturity channel such as CNV's stable or candidate
// is the user's choice, so the Subscription is left alone. The channel stays
// "" too when the catalog doesn't list it, since patching one it doesn't have
// would fail in OLM.
func targetCSV(ctx context.Context, dyn dynamic.Interface, pkg string, st SubscriptionStatus, suggested string) (csv, move string, err error) {
	if csv, _, err = channelCSV(ctx, dyn, pkg, st.Channel); err != nil {
		return "", "", err
	}
	if suggested == "" || suggested == st.Channel || !versioned(suggested) || !versioned(st.Channel) {
		return csv, "", nil
	}
	other, ok, err := channelCSV(ctx, dyn, pkg, suggested)
	if err != nil || !ok {
		return csv, "", err
	}
	return other, suggested, nil
}

// versioned reports whether channel pins an OCP minor, as stable-4.22 does.
func versioned(channel string) bool {
	return strings.HasPrefix(channel, "stable-")
}

// channelCSV returns the CSV at the head of pkg's channel, and whether the
// catalog lists that channel.
func channelCSV(ctx context.Context, dyn dynamic.Interface, pkg, channel string) (string, bool, error) {
	pm, err := dyn.Resource(GVRPackageManifest).Namespace(MarketplaceNamespace).Get(ctx, pkg, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("reading the catalog entry for %s: %w", pkg, err)
	}
	chs, _, _ := unstructured.NestedSlice(pm.Object, "status", "channels")
	for _, c := range chs {
		if m, ok := c.(map[string]any); ok && m["name"] == channel {
			csv, _ := m["currentCSV"].(string)
			return csv, true, nil
		}
	}
	return "", false, nil
}

// pendingPlan returns the InstallPlan waiting for approval that would install
// another of op's CSVs, or "".
func pendingPlan(ctx context.Context, dyn dynamic.Interface, op Operator, installed string) (string, error) {
	l, err := dyn.Resource(GVRInstallPlan).Namespace(op.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return "", nil
		}
		return "", fmt.Errorf("listing the InstallPlans in %s: %w", op.Namespace, err)
	}
	for i := range l.Items {
		p := &l.Items[i]
		if approved, _, _ := unstructured.NestedBool(p.Object, "spec", "approved"); approved {
			continue
		}
		switch nested(p, "status", "phase") {
		case "Complete", "Failed":
			continue
		}
		csvs, _, _ := unstructured.NestedStringSlice(p.Object, "spec", "clusterServiceVersionNames")
		for _, c := range csvs {
			if strings.HasPrefix(c, op.CSVPrefix) && c != installed {
				return p.GetName(), nil
			}
		}
	}
	return "", nil
}

// Approve approves InstallPlan plan, so a Manual Subscription installs it.
func Approve(ctx context.Context, dyn dynamic.Interface, ns, plan string) error {
	if _, err := dyn.Resource(GVRInstallPlan).Namespace(ns).
		Patch(ctx, plan, types.MergePatchType, []byte(`{"spec":{"approved":true}}`), metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("approving InstallPlan %s/%s: %w", ns, plan, err)
	}
	return nil
}

// ApprovePending approves the InstallPlan waiting for op's next CSV, reporting
// whether it found one. OLM creates a new InstallPlan after a channel change, so
// a Manual Subscription needs this again while its upgrade runs.
func ApprovePending(ctx context.Context, dyn dynamic.Interface, op Operator, installed string) (bool, error) {
	plan, err := pendingPlan(ctx, dyn, op, installed)
	if err != nil || plan == "" {
		return false, err
	}
	return true, Approve(ctx, dyn, op.Namespace, plan)
}

// SetChannel moves sub to channel, which makes OLM resolve the upgrade through
// its own graph.
func SetChannel(ctx context.Context, dyn dynamic.Interface, sub *unstructured.Unstructured, channel string) error {
	p := []byte(`{"spec":{"channel":` + strconv.Quote(channel) + `}}`)
	if _, err := dyn.Resource(GVRSubscription).Namespace(sub.GetNamespace()).
		Patch(ctx, sub.GetName(), types.MergePatchType, p, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("moving Subscription %s/%s to channel %s: %w", sub.GetNamespace(), sub.GetName(), channel, err)
	}
	return nil
}

// nested returns u's string at path, or "".
func nested(u *unstructured.Unstructured, path ...string) string {
	s, _, _ := unstructured.NestedString(u.Object, path...)
	return s
}
