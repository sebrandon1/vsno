package builder

import (
	"bytes"
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

	"github.com/sebrandon1/vsno/lib/trace"
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

func TestLastLineAndTail(t *testing.T) {
	for _, c := range []struct{ in, last, tail string }{
		{"", "", ""},
		{"  \n", "", ""},
		{"one", "one", ": one"},
		{"a\nb\n", "b", ": b"},
	} {
		if got := LastLine(c.in); got != c.last {
			t.Errorf("LastLine(%q) = %q, want %q", c.in, got, c.last)
		}
		if got := Tail(c.in); got != c.tail {
			t.Errorf("Tail(%q) = %q, want %q", c.in, got, c.tail)
		}
	}
}

// The commands are reported before the pod is even found, so a create that
// fails to reach the builder still says what it was about to run.
func TestRunTracesCommands(t *testing.T) {
	var buf bytes.Buffer
	trace.Enable(&buf)
	t.Cleanup(trace.Disable)

	e := Exec{Config: &rest.Config{Host: "https://127.0.0.1:1"}, Kube: kfake.NewClientset(), Namespace: "vsno"}
	if err := e.Run(context.Background(), "oc adm release info quay.io/x:1\n", nil, IO{}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("err = %v, want ErrNotReady", err)
	}
	if got, want := buf.String(), "→ exec builder: oc adm release info quay.io/x:1\n"; got != want {
		t.Errorf("trace = %q, want %q", got, want)
	}
}

func TestTracePodCommand(t *testing.T) {
	var buf bytes.Buffer
	trace.Enable(&buf)
	t.Cleanup(trace.Disable)

	tracePodCommand("registry", []string{"df", "-Pk", "/var/lib/registry"})
	tracePodCommand("builder", []string{"bash", "-c", "oc get nodes", "vsno"})
	if got, want := buf.String(), "→ exec registry: df -Pk /var/lib/registry\n"; got != want {
		t.Errorf("trace = %q, want %q", got, want)
	}
}

func TestIsShellScript(t *testing.T) {
	for _, c := range []struct {
		command []string
		want    bool
	}{
		{[]string{"bash", "-c", "script", "vsno"}, true},
		{[]string{"df", "-Pk", "/x"}, false},
		{[]string{"bash"}, false},
		{nil, false},
	} {
		if got := isShellScript(c.command); got != c.want {
			t.Errorf("isShellScript(%q) = %v, want %v", c.command, got, c.want)
		}
	}
}

// The exec request carries the container, the command and the attached streams;
// a TTY merges stderr into stdout, so stderr is not attached with one.
func TestExecOptions(t *testing.T) {
	var buf bytes.Buffer
	plain := execOptions("builder", []string{"bash", "-c", "true"},
		IO{Stdin: strings.NewReader("in"), Stdout: &buf, Stderr: &buf})
	if plain.Container != "builder" || len(plain.Command) != 3 || plain.Command[2] != "true" {
		t.Errorf("container/command = %q %q", plain.Container, plain.Command)
	}
	if !plain.Stdin || !plain.Stdout || !plain.Stderr || plain.TTY {
		t.Errorf("streams = stdin %v, stdout %v, stderr %v, tty %v", plain.Stdin, plain.Stdout, plain.Stderr, plain.TTY)
	}
	tty := execOptions("c", nil, IO{Stdout: &buf, Stderr: &buf, TTY: true})
	if !tty.TTY || tty.Stderr || tty.Stdin {
		t.Errorf("tty = stdin %v, stderr %v, tty %v", tty.Stdin, tty.Stderr, tty.TTY)
	}
	bare := execOptions("c", nil, IO{})
	if bare.Stdin || bare.Stdout || bare.Stderr || bare.TTY {
		t.Errorf("no streams attached: %+v", bare)
	}
}

func TestStreamOptions(t *testing.T) {
	var buf bytes.Buffer
	plain := streamOptions(IO{Stdin: strings.NewReader("in"), Stdout: &buf, Stderr: &buf})
	if plain.Stdin == nil || plain.Stdout == nil || plain.Stderr == nil || plain.Tty {
		t.Errorf("streams = %+v", plain)
	}
	tty := streamOptions(IO{Stdout: &buf, Stderr: &buf, TTY: true})
	if !tty.Tty || tty.Stderr != nil {
		t.Errorf("tty streams = %+v", tty)
	}
}
