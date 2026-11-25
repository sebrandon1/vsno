package reconcile

import (
	"context"
	"fmt"
	"os"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/precheck"
)

// LeaseName is the advisory lock two prunes take, so they do not both plan the
// same deletion. A report-only sweep takes nothing: it changes nothing.
const LeaseName = "vsno-reconcile"

// leaseSeconds is how long a holder is trusted without renewing. A sweep is
// bounded by Options.Timeout, well under this.
const leaseSeconds = 60

// acquire takes the reconcile lease for this process. blocked is set when
// another sweep holds it. warning is set when the lease API cannot be used at
// all (a namespace-scoped user without leases): the sweep then carries on
// unlocked rather than failing, since the actions are idempotent.
func acquire(ctx context.Context, kube kubernetes.Interface, ns string, now time.Time) (release func(), blocked, warning *Item) {
	me := holder()
	c := kube.CoordinationV1().Leases(ns)
	l, err := c.Get(ctx, LeaseName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, cerr := c.Create(ctx, &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: LeaseName, Namespace: ns, Labels: map[string]string{cluster.ManagedByLabel: "vsno"}},
			Spec:       leaseSpec(me, now),
		}, metav1.CreateOptions{})
		if cerr != nil {
			return nil, nil, advisory("couldn't take the reconcile lease", cerr, "continuing unlocked")
		}
		return func() { releaseLease(kube, ns) }, nil, nil
	case err != nil:
		return nil, nil, advisory("couldn't check the reconcile lease", err, "continuing unlocked")
	}
	if heldByOther(l, now) {
		return nil, &Item{
			Kind: "reconcile", Action: Observe,
			Result: precheck.Result{Group: groupName, Check: checkLease, Status: precheck.Info,
				Detail: fmt.Sprintf("another reconcile (%s) is running; nothing was changed", holderOf(l))},
		}, nil
	}
	l.Spec = leaseSpec(me, now)
	if _, uerr := c.Update(ctx, l, metav1.UpdateOptions{}); uerr != nil {
		if apierrors.IsConflict(uerr) {
			return nil, &Item{
				Kind: "reconcile", Action: Observe,
				Result: precheck.Result{Group: groupName, Check: checkLease, Status: precheck.Info,
					Detail: "another reconcile took the lease first; nothing was changed"},
			}, nil
		}
		return nil, nil, advisory("couldn't take the reconcile lease", uerr, "continuing unlocked")
	}
	return func() { releaseLease(kube, ns) }, nil, nil
}

func leaseSpec(me string, now time.Time) coordinationv1.LeaseSpec {
	at := metav1.NewMicroTime(now)
	secs := int32(leaseSeconds)
	return coordinationv1.LeaseSpec{HolderIdentity: &me, LeaseDurationSeconds: &secs, AcquireTime: &at, RenewTime: &at}
}

func heldByOther(l *coordinationv1.Lease, now time.Time) bool {
	if l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity == "" || l.Spec.RenewTime == nil {
		return false
	}
	secs := int32(leaseSeconds)
	if l.Spec.LeaseDurationSeconds != nil {
		secs = *l.Spec.LeaseDurationSeconds
	}
	return now.Sub(l.Spec.RenewTime.Time) < time.Duration(secs)*time.Second
}

func holderOf(l *coordinationv1.Lease) string {
	if l.Spec.HolderIdentity == nil {
		return "unknown"
	}
	return *l.Spec.HolderIdentity
}

// releaseLease deletes the lease rather than blanking its holder: a waiting
// sweep then creates it afresh, and a crash only costs it the wait for the
// duration to pass.
func releaseLease(kube kubernetes.Interface, ns string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = kube.CoordinationV1().Leases(ns).Delete(ctx, LeaseName, metav1.DeleteOptions{})
}

func holder() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		h = "vsno"
	}
	return fmt.Sprintf("%s:%d", h, os.Getpid())
}

func advisory(what string, err error, so string) *Item {
	return &Item{
		Kind: "reconcile", Action: Observe,
		Result: precheck.Result{Group: groupName, Check: checkLease, Status: precheck.Info,
			Detail: what + ": " + err.Error() + "; " + so},
	}
}
