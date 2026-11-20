package registry

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/sebrandon1/vsno/lib/builder"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Usage is the registry volume's used and total size in bytes. It reads df
// inside the registry pod, which is the only place the volume is mounted.
func Usage(ctx context.Context, cfg *rest.Config, kube kubernetes.Interface, ns string) (used, total int64, err error) {
	pod, err := Pod(ctx, kube, ns)
	if err != nil {
		return 0, 0, err
	}
	var out, stderr bytes.Buffer
	err = builder.ExecPod(ctx, cfg, kube, ns, pod, Service, []string{"df", "-Pk", mount}, builder.IO{Stdout: &out, Stderr: &stderr})
	if err != nil {
		return 0, 0, fmt.Errorf("reading the registry volume: %w%s", err, tail(stderr.String()))
	}
	return parseDF(out.String())
}

// mount is where the registry Deployment mounts its volume.
const mount = "/var/lib/registry"

// Pod finds the registry's running pod.
func Pod(ctx context.Context, kube kubernetes.Interface, ns string) (string, error) {
	pods, err := kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "app=" + Service})
	if err != nil {
		return "", fmt.Errorf("finding the registry pod: %w", err)
	}
	for _, p := range pods.Items {
		if p.Status.Phase == "Running" && p.DeletionTimestamp == nil {
			return p.Name, nil
		}
	}
	return "", fmt.Errorf("the registry pod isn't running; see `vsno status`")
}

// parseDF reads df -Pk's last line: total, used, available, in 1k blocks.
func parseDF(out string) (used, total int64, err error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return 0, 0, fmt.Errorf("unexpected df output: %q", out)
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 3 {
		return 0, 0, fmt.Errorf("unexpected df output: %q", lines[len(lines)-1])
	}
	total, err = size(f[1])
	if err != nil {
		return 0, 0, err
	}
	used, err = size(f[2])
	return used, total, err
}

// size converts df's 1k blocks to bytes.
func size(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("reading df's column %q: %w", s, err)
	}
	return n * 1024, nil
}
