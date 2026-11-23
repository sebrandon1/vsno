package registry

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/builder"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kfake "k8s.io/client-go/kubernetes/fake"
)

func TestParseDF(t *testing.T) {
	// df -Pk's real shape: a header, then one row per filesystem.
	const df = `Filesystem     1024-blocks      Used Available Capacity Mounted on
/dev/mapper/vg--vsno  104857600  44564480  60293120      43% /var/lib/registry
`
	used, total, err := parseDF(df)
	if err != nil || used != 44564480*1024 || total != 104857600*1024 {
		t.Errorf("parseDF = %d, %d, %v", used, total, err)
	}

	for _, c := range []struct {
		name, out, want string
	}{
		{"header only", "Filesystem 1024-blocks Used\n", "unexpected df output"},
		{"short row", "h\n/dev/x 1\n", "unexpected df output"},
		{"not a number", "h\n/dev/x lots 1 2 3% /x\n", "reading df's column"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := parseDF(c.out); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("parseDF(%q) error = %v, want %q", c.out, err, c.want)
			}
		})
	}
}

func TestSize(t *testing.T) {
	if n, err := size("1024"); err != nil || n != 1048576 {
		t.Errorf("size(1024) = %d, %v", n, err)
	}
	if _, err := size("1.5"); err == nil || !strings.Contains(err.Error(), `"1.5"`) {
		t.Errorf("size(1.5) error = %v", err)
	}
}

func pod(name string, phase corev1.PodPhase, deleting bool) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{"app": Service}},
		Status:     corev1.PodStatus{Phase: phase},
	}
	if deleting {
		now := metav1.Now()
		p.DeletionTimestamp = &now
	}
	return p
}

// The volume is mounted only in the registry's pod, so every read of it goes
// through this lookup.
func TestPod(t *testing.T) {
	ctx := context.Background()
	k := kfake.NewClientset(pod("registry-old", corev1.PodSucceeded, false), pod("registry-a", corev1.PodRunning, false))
	if got, err := Pod(ctx, k, ns); err != nil || got != "registry-a" {
		t.Errorf("Pod = %q, %v, want the running pod", got, err)
	}

	// A pod on its way out is not one to exec into.
	k = kfake.NewClientset(pod("registry-a", corev1.PodRunning, true))
	if _, err := Pod(ctx, k, ns); err == nil || !strings.Contains(err.Error(), "isn't running") {
		t.Errorf("deleting pod: %v", err)
	}

	k = kfake.NewClientset()
	if _, err := Pod(ctx, k, ns); err == nil || !strings.Contains(err.Error(), "isn't running") {
		t.Errorf("no pod: %v", err)
	}
}

// Usage reads df in the registry pod; the exec is injected, so the whole path
// is exercised without a cluster.
func TestEnvUsage(t *testing.T) {
	ctx := context.Background()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "registry-abc", Labels: map[string]string{"app": Service}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	k := kfake.NewClientset(pod)
	var ran []string
	e := Env{Kube: k, Exec: func(_ context.Context, ns, pod, container string, command []string, s builder.IO) error {
		ran = append(ran, ns+"/"+pod+" "+container+" "+strings.Join(command, " "))
		_, _ = io.WriteString(s.Stdout, "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/x 1048576 524288 524288 50% /var/lib/registry\n")
		return nil
	}}
	used, total, err := e.Usage(ctx, ns)
	if err != nil || used != 512<<20 || total != 1<<30 {
		t.Fatalf("Usage = %d, %d, %v", used, total, err)
	}
	if want := ns + "/registry-abc " + Service + " df -Pk /var/lib/registry"; len(ran) != 1 || ran[0] != want {
		t.Errorf("ran %q, want %q", ran, want)
	}

	failing := Env{Kube: k, Exec: func(_ context.Context, _, _, _ string, _ []string, s builder.IO) error {
		_, _ = io.WriteString(s.Stderr, "exec failed")
		return errors.New("exit 1")
	}}
	if _, _, err := failing.Usage(ctx, ns); err == nil || !strings.Contains(err.Error(), "exec failed") {
		t.Errorf("exec failure: %v", err)
	}

	noPod := Env{Kube: kfake.NewClientset()}
	if _, _, err := noPod.Usage(ctx, ns); err == nil || !strings.Contains(err.Error(), "isn't running") {
		t.Errorf("missing pod: %v", err)
	}
}
