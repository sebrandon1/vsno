package precheck

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

// DefaultProbeImage only needs bash and chroot; the probe itself runs the node's own tools.
const DefaultProbeImage = "quay.io/centos/centos:stream9"

// PodProber runs ProbeScript on each node in a short-lived privileged pod, the
// same way `oc debug node` does: a temporary namespace that is deleted afterwards.
type PodProber struct {
	Kube    kubernetes.Interface
	Image   string
	Timeout time.Duration
}

// Probe implements Prober.
func (p *PodProber) Probe(ctx context.Context, nodes []string, onDone func(node string, err error)) (map[string]NodeHardware, map[string]error) {
	hw := map[string]NodeHardware{}
	errs := map[string]error{}
	if len(nodes) == 0 {
		return hw, errs
	}

	ns := "vsno-precheck-" + rand.String(5)
	if err := p.createNamespace(ctx, ns); err != nil {
		for _, n := range nodes {
			errs[n] = fmt.Errorf("creating probe namespace: %w", err)
		}
		return hw, errs
	}
	defer func() {
		// Use a fresh context so cleanup still happens after cancellation.
		_ = p.Kube.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{})
	}()

	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, node := range nodes {
		wg.Add(1)
		go func(node string) {
			defer wg.Done()
			out, err := p.probeNode(ctx, ns, node)
			var h NodeHardware
			if err == nil {
				h, err = ParseProbe(out)
			}
			mu.Lock()
			if err == nil {
				hw[node] = h
			} else {
				errs[node] = err
			}
			mu.Unlock()
			if onDone != nil {
				onDone(node, err)
			}
		}(node)
	}
	wg.Wait()
	return hw, errs
}

func (p *PodProber) createNamespace(ctx context.Context, ns string) error {
	_, err := p.Kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: ns,
		Labels: map[string]string{
			"pod-security.kubernetes.io/enforce":             "privileged",
			"pod-security.kubernetes.io/audit":               "privileged",
			"pod-security.kubernetes.io/warn":                "privileged",
			"security.openshift.io/scc.podSecurityLabelSync": "false",
		},
		// Like oc debug: ignore any project default node selector.
		Annotations: map[string]string{"openshift.io/node-selector": ""},
	}}, metav1.CreateOptions{})
	return err
}

func (p *PodProber) probeNode(ctx context.Context, ns, node string) (string, error) {
	privileged := true
	hostPathDir := corev1.HostPathDirectory
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "probe-", Namespace: ns, Labels: map[string]string{"app": "vsno-precheck"}},
		Spec: corev1.PodSpec{
			NodeName:      node,
			HostPID:       true,
			RestartPolicy: corev1.RestartPolicyNever,
			Tolerations:   []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:            "probe",
				Image:           p.Image,
				Command:         []string{"chroot", "/host", "bash", "-c", ProbeScript},
				SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
				VolumeMounts:    []corev1.VolumeMount{{Name: "host", MountPath: "/host", ReadOnly: true}},
			}},
			Volumes: []corev1.Volume{{Name: "host", VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/", Type: &hostPathDir},
			}}},
		},
	}
	created, err := p.Kube.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("creating probe pod: %w", err)
	}

	var phase corev1.PodPhase
	err = wait.PollUntilContextTimeout(ctx, 2*time.Second, p.Timeout, true, func(ctx context.Context) (bool, error) {
		got, gerr := p.Kube.CoreV1().Pods(ns).Get(ctx, created.Name, metav1.GetOptions{})
		if gerr != nil {
			return false, nil // transient
		}
		phase = got.Status.Phase
		return phase == corev1.PodSucceeded || phase == corev1.PodFailed, nil
	})
	if err != nil {
		return "", fmt.Errorf("probe pod did not finish within %s (phase %s); is %s pullable?", p.Timeout, phase, p.Image)
	}

	stream, err := p.Kube.CoreV1().Pods(ns).GetLogs(created.Name, &corev1.PodLogOptions{}).Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("reading probe logs: %w", err)
	}
	defer func() { _ = stream.Close() }()
	b, err := io.ReadAll(stream)
	if err != nil {
		return "", fmt.Errorf("reading probe logs: %w", err)
	}
	if phase == corev1.PodFailed {
		return "", fmt.Errorf("probe failed: %s", strings.TrimSpace(string(b)))
	}
	return string(b), nil
}
