package guest

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

func addService(t *testing.T, w *world, name string, age time.Duration) {
	t.Helper()
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: name, Labels: map[string]string{LabelName: name},
		CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
	}}
	if _, err := w.kube.CoreV1().Services(ns).Create(context.Background(), svc, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestAbandonedServices(t *testing.T) {
	w := newWorld(t, vm("seed", "Running"))
	addService(t, w, "old", 2*AbandonGrace())
	addService(t, w, "young", time.Minute)
	addService(t, w, "seed", 2*AbandonGrace()) // its VM exists: not abandoned

	got, err := AbandonedServices(context.Background(), w.env(), ns, AbandonGrace(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "old" {
		t.Errorf("got %v, want [old]", got)
	}

	got, err = AbandonedServices(context.Background(), w.env(), ns, AbandonGrace(), []string{"young"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("a young Service was named abandoned: %v", got)
	}
}

// A cluster without KubeVirt answers NotFound for the VM list; that means no
// VM, not that the sweep cannot tell.
func TestAbandonedServicesWithoutKubeVirt(t *testing.T) {
	w := newWorld(t)
	addService(t, w, "old", 2*AbandonGrace())
	w.dyn.PrependReactor("list", "virtualmachines", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "kubevirt.io", Resource: "virtualmachines"}, "virtualmachines")
	})
	got, err := AbandonedServices(context.Background(), w.env(), ns, AbandonGrace(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "old" {
		t.Errorf("got %v, want [old]", got)
	}
}

func TestAbandonGraceIsListGrace(t *testing.T) {
	// list's own classifier and reconcile's must agree, or one says building
	// while the other deletes.
	if AbandonGrace() != abandonGrace {
		t.Errorf("AbandonGrace() = %v", AbandonGrace())
	}
}
