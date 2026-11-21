// Package builder runs scripts in the environment's builder pod, which has the
// tools vSNOs need (oc, openshift-install, nmstate, ssh) and reaches the
// guests by their Service ClusterIPs.
package builder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/streaming/pkg/httpstream"
)

// Container is the builder Deployment's container.
const Container = "builder"

// LabelSelector finds the builder pod, and so the namespaces that are vsno
// environments.
const LabelSelector = "app=builder"

// IO wires a script's streams. Any may be nil; a non-nil TTY size queue asks
// for a terminal.
type IO struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	TTY            bool
	Resize         remotecommand.TerminalSizeQueue
}

// Runner runs a bash script with arguments in the builder.
type Runner interface {
	Run(ctx context.Context, script string, args []string, streams IO) error
}

// Exec is the Runner for a real cluster.
type Exec struct {
	Config    *rest.Config
	Kube      kubernetes.Interface
	Namespace string
}

// ErrNotReady means the builder pod isn't running and ready; vsno setup
// creates it.
var ErrNotReady = errors.New("no ready builder pod")

// Pod returns the ready builder pod's name.
func Pod(ctx context.Context, kube kubernetes.Interface, ns string) (string, error) {
	p, err := ReadyPod(ctx, kube, ns)
	if err != nil {
		return "", err
	}
	return p.Name, nil
}

// ReadyPod returns the ready builder pod, for a pod that must run beside it
// (its node, its SELinux level).
func ReadyPod(ctx context.Context, kube kubernetes.Interface, ns string) (*corev1.Pod, error) {
	pods, err := kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: LabelSelector})
	if err != nil {
		return nil, fmt.Errorf("finding the builder pod: %w", err)
	}
	for i, p := range pods.Items {
		if p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil {
			continue
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				return &pods.Items[i], nil
			}
		}
	}
	return nil, fmt.Errorf("%w in namespace %s; run `vsno setup -n %s`", ErrNotReady, ns, ns)
}

// Run implements Runner. The script runs as `bash -c script vsno args...`.
// Without a stderr writer, stderr is captured and its last line added to an
// error (exec also needs at least one output stream).
func (e Exec) Run(ctx context.Context, script string, args []string, s IO) error {
	var captured bytes.Buffer
	if s.Stderr == nil && !s.TTY {
		s.Stderr = &captured
	}
	err := e.run(ctx, script, args, s)
	if err != nil && captured.Len() > 0 {
		out := strings.TrimSpace(captured.String())
		err = fmt.Errorf("%w: %s", err, out[strings.LastIndex(out, "\n")+1:])
	}
	return err
}

func (e Exec) run(ctx context.Context, script string, args []string, s IO) error {
	pod, err := Pod(ctx, e.Kube, e.Namespace)
	if err != nil {
		return err
	}
	return ExecPod(ctx, e.Config, e.Kube, e.Namespace, pod, Container,
		append([]string{"bash", "-c", script, "vsno"}, args...), s)
}

// ExecPod runs command in pod's container. It is how vsno reads a container's
// filesystem, such as the registry's volume, without a shell of its own.
func ExecPod(ctx context.Context, cfg *rest.Config, kube kubernetes.Interface, ns, pod, container string, command []string, s IO) error {
	req := kube.CoreV1().RESTClient().Post().Resource("pods").Namespace(ns).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdin:     s.Stdin != nil,
			Stdout:    s.Stdout != nil,
			Stderr:    s.Stderr != nil && !s.TTY, // a TTY merges stderr into stdout
			TTY:       s.TTY,
		}, scheme.ParameterCodec)
	ws, err := remotecommand.NewWebSocketExecutor(cfg, "GET", req.URL().String())
	if err != nil {
		return err
	}
	spdy, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
	if err != nil {
		return err
	}
	ex, err := remotecommand.NewFallbackExecutor(ws, spdy, httpstream.IsUpgradeFailure)
	if err != nil {
		return err
	}
	opts := remotecommand.StreamOptions{Stdin: s.Stdin, Stdout: s.Stdout, Tty: s.TTY, TerminalSizeQueue: s.Resize}
	if !s.TTY {
		opts.Stderr = s.Stderr
	}
	return ex.StreamWithContext(ctx, opts)
}

// LastLine is the last line of a script's output, trimmed.
func LastLine(s string) string {
	s = strings.TrimSpace(s)
	return s[strings.LastIndex(s, "\n")+1:]
}

// Tail is LastLine as ": line", to end an error message; "" for no output.
func Tail(s string) string {
	if l := LastLine(s); l != "" {
		return ": " + l
	}
	return ""
}
