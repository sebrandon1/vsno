package builder

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

func pod(name string, phase corev1.PodPhase, ready bool) *corev1.Pod {
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "vsno", Labels: map[string]string{"app": "builder"}},
		Status: corev1.PodStatus{Phase: phase, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: st}}}}
}

func TestPod(t *testing.T) {
	ctx := context.Background()
	k := kfake.NewClientset(pod("old", corev1.PodRunning, false), pod("pending", corev1.PodPending, false), pod("good", corev1.PodRunning, true))
	if name, err := Pod(ctx, k, "vsno"); err != nil || name != "good" {
		t.Errorf("Pod = %q, %v", name, err)
	}
	k = kfake.NewClientset(pod("old", corev1.PodRunning, false))
	if _, err := Pod(ctx, k, "vsno"); !errors.Is(err, ErrNotReady) || !strings.Contains(err.Error(), "run `vsno setup -n vsno`") {
		t.Errorf("not ready: %v", err)
	}
	k.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("api down") })
	if _, err := Pod(ctx, k, "vsno"); err == nil || !strings.Contains(err.Error(), "finding the builder pod: api down") {
		t.Errorf("list error: %v", err)
	}
}

// ReadyPod returns the pod itself, for its node and SELinux level.
func TestReadyPod(t *testing.T) {
	good := pod("good", corev1.PodRunning, true)
	good.Spec.NodeName = "worker-1"
	p, err := ReadyPod(context.Background(), kfake.NewClientset(pod("old", corev1.PodRunning, false), good), "vsno")
	if err != nil || p.Name != "good" || p.Spec.NodeName != "worker-1" {
		t.Errorf("ReadyPod = %v, %v", p, err)
	}
}

func TestRunWithoutBuilder(t *testing.T) {
	e := Exec{Config: &rest.Config{Host: "https://127.0.0.1:1"}, Kube: kfake.NewClientset(), Namespace: "vsno"}
	if err := e.Run(context.Background(), "true", nil, IO{}); !errors.Is(err, ErrNotReady) {
		t.Errorf("err = %v", err)
	}
}
