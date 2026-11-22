package precheck

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// proberKube answers the poll's pod reads with Succeeded, keeping the created
// pods so a log seam can tell which node it is reading for.
func proberKube(t *testing.T) *kfake.Clientset {
	t.Helper()
	k := kfake.NewClientset()
	// GenerateName is not unique across concurrent creates in the fake tracker;
	// give each probe pod its own name.
	var mu sync.Mutex
	n := 0
	k.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		n++
		name := fmt.Sprintf("probe-%d", n)
		mu.Unlock()
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		pod.Name, pod.GenerateName = name, ""
		return false, nil, nil
	})
	k.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "" {
			return false, nil, nil
		}
		obj, err := k.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"),
			action.GetNamespace(), action.(k8stesting.GetAction).GetName())
		if err != nil {
			return true, nil, err
		}
		pod := obj.(*corev1.Pod).DeepCopy()
		pod.Status.Phase = corev1.PodSucceeded
		return true, pod, nil
	})
	return k
}

// probeOutput is a real probe's output, the same fixture ParseProbe is tested on.
func probeOutput(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "probe-baremetal-empty-disk.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// logsByNode answers each pod's log from the node it was scheduled on.
func logsByNode(t *testing.T, k *kfake.Clientset, byNode map[string]string) func(context.Context, string, string) (io.ReadCloser, error) {
	t.Helper()
	return func(_ context.Context, ns, pod string) (io.ReadCloser, error) {
		obj, err := k.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), ns, pod)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(strings.NewReader(byNode[obj.(*corev1.Pod).Spec.NodeName])), nil
	}
}

// deletedNamespace reports whether the probe cleaned its namespace up.
func deletedNamespace(k *kfake.Clientset) bool {
	for _, a := range k.Actions() {
		if a.GetVerb() == "delete" && a.GetResource().Resource == "namespaces" {
			return true
		}
	}
	return false
}

func createdNamespace(k *kfake.Clientset) bool {
	for _, a := range k.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "namespaces" {
			return true
		}
	}
	return false
}

func TestProbeNoNodes(t *testing.T) {
	k := proberKube(t)
	p := &PodProber{Kube: k, Timeout: time.Second}

	hw, errs := p.Probe(t.Context(), nil, nil)
	if len(hw) != 0 || len(errs) != 0 {
		t.Errorf("hw=%v errs=%v, want both empty", hw, errs)
	}
	if createdNamespace(k) {
		t.Error("a probe with no nodes must not create a namespace")
	}
}

func TestProbeOneNode(t *testing.T) {
	k := proberKube(t)
	p := &PodProber{Kube: k, Timeout: 5 * time.Second,
		readLog: logsByNode(t, k, map[string]string{"worker-0": probeOutput(t)})}

	var done []string
	hw, errs := p.Probe(t.Context(), []string{"worker-0"}, func(node string, err error) {
		if err != nil {
			t.Errorf("onDone(%s): %v", node, err)
		}
		done = append(done, node)
	})
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}
	if h, ok := hw["worker-0"]; !ok || !h.BareMetal() || h.Threads != 104 {
		t.Errorf("hw = %+v, want the fixture parsed for worker-0", hw)
	}
	if len(done) != 1 || done[0] != "worker-0" {
		t.Errorf("onDone called with %v, want worker-0 once", done)
	}
	if !deletedNamespace(k) {
		t.Error("the probe namespace must be deleted")
	}
}

// One node's probe failing must not lose the other's result.
func TestProbePerNodeErrors(t *testing.T) {
	k := proberKube(t)
	p := &PodProber{Kube: k, Timeout: 5 * time.Second,
		readLog: logsByNode(t, k, map[string]string{"worker-0": probeOutput(t), "worker-1": "not a probe"})}

	hw, errs := p.Probe(t.Context(), []string{"worker-0", "worker-1"}, nil)
	if _, ok := hw["worker-0"]; !ok {
		t.Errorf("hw = %v, want worker-0", hw)
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want only worker-1", errs)
	}
	if _, ok := errs["worker-1"]; !ok {
		t.Errorf("errs = %v, want worker-1", errs)
	}
}

func TestProbeNamespaceCreateFails(t *testing.T) {
	k := proberKube(t)
	k.PrependReactor("create", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, os.ErrPermission
	})
	p := &PodProber{Kube: k, Timeout: time.Second}

	hw, errs := p.Probe(t.Context(), []string{"worker-0", "worker-1"}, nil)
	if len(hw) != 0 {
		t.Errorf("hw = %v, want none", hw)
	}
	for _, node := range []string{"worker-0", "worker-1"} {
		if errs[node] == nil || !strings.Contains(errs[node].Error(), "creating probe namespace") {
			t.Errorf("errs[%s] = %v, want the namespace error", node, errs[node])
		}
	}
}

// A canceled probe returns, and still deletes its namespace.
func TestProbeCancellation(t *testing.T) {
	k := proberKube(t)
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(20*time.Millisecond, cancel)

	p := &PodProber{Kube: k, Timeout: 5 * time.Second,
		readLog: func(ctx context.Context, _, _ string) (io.ReadCloser, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}

	start := time.Now()
	hw, errs := p.Probe(ctx, []string{"worker-0"}, nil)
	if len(hw) != 0 || len(errs) != 1 {
		t.Errorf("hw=%v errs=%v, want one error", hw, errs)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Probe took %s after cancellation", elapsed)
	}
	if !deletedNamespace(k) {
		t.Error("the probe namespace must be deleted even after cancellation")
	}
}
