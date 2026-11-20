package ibu

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/guestctl"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/registry"
	"github.com/sebrandon1/vsno/lib/seed"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// DefaultLCARepo is where an LCA ref is built from unless --lca-repo names a fork.
const DefaultLCARepo = "https://github.com/openshift-kni/lifecycle-agent"

// LCASource is where Prepare takes LCA from: the vSNO's catalog (the zero
// value), a git ref built in the cluster, or a prebuilt image, deployed with
// the manifests of a ref.
type LCASource struct {
	Repo  string `json:"repo,omitempty"`  // DefaultLCARepo when empty
	Ref   string `json:"ref,omitempty"`   // branch, tag or SHA; "main" with only Image
	Image string `json:"image,omitempty"` // a prebuilt controller image; "" builds Ref
}

// FromCatalog is true for the default: LCA from the vSNO's operator catalog.
func (s LCASource) FromCatalog() bool { return s.Ref == "" && s.Image == "" }

func (s LCASource) withDefaults() LCASource {
	if s.Repo == "" {
		s.Repo = DefaultLCARepo
	}
	if s.Ref == "" {
		s.Ref = "main"
	}
	return s
}

// BuiltLCA is an LCA ready to deploy: its source, the controller image, and
// the manifests that run it.
type BuiltLCA struct {
	Repo    string `json:"repo"`
	Ref     string `json:"ref"`
	SHA     string `json:"sha"`
	Version string `json:"version"` // the Makefile's VERSION, e.g. 4.21.0
	Image   string `json:"image"`

	manifests string
}

// String is "<repo>@<sha> (<image>)", the source vsno records on a vSNO.
func (b BuiltLCA) String() string { return b.Repo + "@" + b.SHA + " (" + b.Image + ")" }

const (
	lcaBuildPod   = "lca-build"
	lcaBuildAuth  = "lca-build-auth"
	lcaBuildImage = "quay.io/buildah/stable:v1.38.0"
	lcaBuildWait  = 45 * time.Minute
	lcaDeployWait = 10 * time.Minute
)

// buildLCA clones s in a buildah pod next to the builder, builds and pushes
// its image unless s names one, and renders its manifests in the builder.
func buildLCA(ctx context.Context, e Env, ns string, s LCASource, reg seed.Registry, ps precheck.ProgressStep) (*BuiltLCA, error) {
	s = s.withDefaults()
	bp, err := builderPod(ctx, e, ns)
	if err != nil {
		return nil, err
	}
	if err := writeBuildAuth(ctx, e, ns, reg); err != nil {
		return nil, err
	}
	defer func() {
		_ = e.Kube.CoreV1().Secrets(ns).Delete(context.WithoutCancel(ctx), lcaBuildAuth, metav1.DeleteOptions{})
	}()
	logs, err := runBuildPod(ctx, e, ns, bp, []string{s.Repo, s.Ref, reg.Host, s.Image}, ps)
	if err != nil {
		return nil, err
	}
	b, err := parseBuild(logs)
	if err != nil {
		return nil, err
	}
	b.Repo, b.Ref = s.Repo, s.Ref
	var out, stderr bytes.Buffer
	if err := e.Builder.Run(ctx, script("lca-render.sh"), []string{b.SHA, b.Image}, builder.IO{Stdout: &out, Stderr: &stderr}); err != nil {
		return nil, fmt.Errorf("rendering LCA's manifests: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	b.manifests = out.String()
	return b, nil
}

// builderPod is the running builder pod. The build pod mounts its
// ReadWriteOnce volume, so it runs on the builder's node, with the builder's
// SELinux level: a pod at another level would get the volume relabeled to
// its own, locking the builder out of its files.
func builderPod(ctx context.Context, e Env, ns string) (*corev1.Pod, error) {
	pods, err := e.Kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "app=builder"})
	if err != nil {
		return nil, fmt.Errorf("finding the builder: %w", err)
	}
	for i, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning && p.Spec.NodeName != "" {
			return &pods.Items[i], nil
		}
	}
	return nil, errors.New("the builder pod isn't running; see `vsno status`")
}

// writeBuildAuth stores what the build pod pulls base images and pushes with:
// the host cluster's pull secret plus the registry's login, and its CA.
func writeBuildAuth(ctx context.Context, e Env, ns string, reg seed.Registry) error {
	ps, err := e.Kube.CoreV1().Secrets("openshift-config").Get(ctx, "pull-secret", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading the cluster pull secret: %w", err)
	}
	auth, err := registry.AddAuth(string(ps.Data[corev1.DockerConfigJsonKey]), reg.Host, reg.User, reg.Password)
	if err != nil {
		return err
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: lcaBuildAuth, Namespace: ns},
		StringData: map[string]string{"auth.json": auth, "ca.crt": reg.CA}}
	_ = e.Kube.CoreV1().Secrets(ns).Delete(ctx, lcaBuildAuth, metav1.DeleteOptions{})
	if _, err := e.Kube.CoreV1().Secrets(ns).Create(ctx, sec, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("storing the build's credentials: %w", err)
	}
	return nil
}

// runBuildPod runs lca-build.sh in a privileged buildah pod beside the builder
// (see builderPod), shows its last log line as progress, and returns its whole
// log once it succeeded.
func runBuildPod(ctx context.Context, e Env, ns string, builder *corev1.Pod, args []string, ps precheck.ProgressStep) (string, error) {
	pods := e.Kube.CoreV1().Pods(ns)
	_ = pods.Delete(ctx, lcaBuildPod, metav1.DeleteOptions{}) // a leftover from an interrupted build
	privileged := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: lcaBuildPod, Namespace: ns, Labels: map[string]string{"app.kubernetes.io/managed-by": "vsno"}},
		Spec: corev1.PodSpec{
			NodeName:        builder.Spec.NodeName,
			SecurityContext: sameSELinux(builder),
			RestartPolicy:   corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name: "build", Image: lcaBuildImage,
				Command:         append([]string{"bash", "-c", script("lca-build.sh"), "lca-build.sh"}, args...),
				SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
				VolumeMounts:    []corev1.VolumeMount{{Name: "work", MountPath: "/work"}, {Name: "auth", MountPath: "/auth"}},
			}},
			Volumes: []corev1.Volume{
				{Name: "work", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "builder-work"}}},
				{Name: "auth", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: lcaBuildAuth}}},
			},
		},
	}
	if _, err := pods.Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return "", fmt.Errorf("starting the LCA build: %w", err)
	}
	defer func() { _ = pods.Delete(context.WithoutCancel(ctx), lcaBuildPod, metav1.DeleteOptions{}) }()
	var phase corev1.PodPhase
	err := guestctl.Poll(ctx, e.Clock(), lcaBuildWait, func() (bool, error) {
		p, err := pods.Get(ctx, lcaBuildPod, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		phase = p.Status.Phase
		if line := lastLine(podLog(ctx, e, ns, 1)); line != "" {
			ps.Update(line)
		}
		return phase == corev1.PodSucceeded || phase == corev1.PodFailed, nil
	})
	logs := podLog(ctx, e, ns, 0)
	switch {
	case errors.Is(err, guestctl.ErrTimeout):
		return "", fmt.Errorf("the LCA build didn't finish in %s; last: %s", lcaBuildWait, lastLine(logs))
	case err != nil:
		return "", err
	case phase == corev1.PodFailed:
		return "", fmt.Errorf("the LCA build failed: %s", lastLine(logs))
	}
	return logs, nil
}

// sameSELinux is a pod security context with builder's SELinux options.
func sameSELinux(builder *corev1.Pod) *corev1.PodSecurityContext {
	if builder.Spec.SecurityContext == nil || builder.Spec.SecurityContext.SELinuxOptions == nil {
		return nil
	}
	o := *builder.Spec.SecurityContext.SELinuxOptions
	return &corev1.PodSecurityContext{SELinuxOptions: &o}
}

// podLog is the build pod's log, its last tail lines when tail > 0, or "".
func podLog(ctx context.Context, e Env, ns string, tail int64) string {
	o := &corev1.PodLogOptions{}
	if tail > 0 {
		o.TailLines = &tail
	}
	b, err := e.Kube.CoreV1().Pods(ns).GetLogs(lcaBuildPod, o).DoRaw(ctx)
	if err != nil {
		return ""
	}
	return string(b)
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	return s[strings.LastIndex(s, "\n")+1:]
}

// parseBuild reads the sha=, version= and image= lines lca-build.sh prints.
func parseBuild(logs string) (*BuiltLCA, error) {
	b := &BuiltLCA{}
	sc := bufio.NewScanner(strings.NewReader(logs))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch k {
		case "sha":
			b.SHA = v
		case "version":
			b.Version = v
		case "image":
			b.Image = v
		}
	}
	if b.SHA == "" || b.Image == "" {
		return nil, fmt.Errorf("the LCA build didn't report its commit and image: %s", lastLine(logs))
	}
	return b, nil
}

// deployLCA replaces the vSNO's catalog LCA, if any, with b: it removes the
// Subscription and CSV (the CRDs and the ImageBasedUpgrade stay), lets the
// node pull from the registry, applies b's manifests and waits for the
// controller to run b's image.
func deployLCA(ctx context.Context, a guestctl.Access, c guestctl.Clock, b *BuiltLCA, reg seed.Registry, ps precheck.ProgressStep) error {
	if err := guestctl.Oc(ctx, a, "", "delete", "subscriptions", seed.LCA.Package, "-n", seed.LCANamespace, "--ignore-not-found"); err != nil {
		return err
	}
	if csv, _, err := seed.LCA.CSV(ctx, a); err != nil {
		return err
	} else if csv != "" {
		if err := guestctl.Oc(ctx, a, "", "delete", "clusterserviceversions", csv, "-n", seed.LCANamespace, "--ignore-not-found"); err != nil {
			return err
		}
	}
	ps.Update("letting the node pull from " + reg.Host)
	if err := ensurePullAuth(ctx, a, reg); err != nil {
		return err
	}
	ps.Update("applying LCA " + b.SHA)
	if err := guestctl.Apply(ctx, a, b.manifests); err != nil {
		return err
	}
	var last string
	err := guestctl.Poll(ctx, c, lcaDeployWait, func() (bool, error) {
		var d struct {
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Image string `json:"image"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
			Status struct {
				Replicas            int `json:"replicas"`
				UpdatedReplicas     int `json:"updatedReplicas"`
				AvailableReplicas   int `json:"availableReplicas"`
				UnavailableReplicas int `json:"unavailableReplicas"`
			} `json:"status"`
		}
		if _, err := guestctl.Get(ctx, a, &d, "get", "deployments", seed.LCADeployment, "-n", seed.LCANamespace); err != nil {
			last = err.Error()
			return false, nil
		}
		ours := false
		for _, ct := range d.Spec.Template.Spec.Containers {
			ours = ours || ct.Image == b.Image
		}
		last = fmt.Sprintf("%d updated, %d available", d.Status.UpdatedReplicas, d.Status.AvailableReplicas)
		ps.Update("LCA controller: " + last)
		// Rolled out: every replica runs b's image and is available. The old
		// controller counts as available until it goes, so available alone
		// doesn't say the new one started.
		st := d.Status
		return ours && st.UpdatedReplicas > 0 && st.UpdatedReplicas == st.Replicas && st.UnavailableReplicas == 0 && st.AvailableReplicas > 0, nil
	})
	if errors.Is(err, guestctl.ErrTimeout) {
		return fmt.Errorf("LCA %s didn't start in %s (%s); `vsno oc <name> -- get pods -n %s`", b.SHA, lcaDeployWait, last, seed.LCANamespace)
	}
	return err
}

// ensurePullAuth adds the registry's login to the vSNO's global pull secret,
// so the node can pull images vsno pushed there. A guest installed from the
// release cache already has it.
func ensurePullAuth(ctx context.Context, a guestctl.Access, reg seed.Registry) error {
	var sec struct {
		Data map[string]string `json:"data"`
	}
	if _, err := guestctl.Get(ctx, a, &sec, "get", "secret", "pull-secret", "-n", "openshift-config"); err != nil {
		return err
	}
	cur, err := base64.StdEncoding.DecodeString(sec.Data[corev1.DockerConfigJsonKey])
	if err != nil {
		return fmt.Errorf("reading the vSNO's pull secret: %w", err)
	}
	if strings.Contains(string(cur), `"`+reg.Host+`"`) {
		return nil
	}
	merged, err := registry.AddAuth(string(cur), reg.Host, reg.User, reg.Password)
	if err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{"data": map[string]string{corev1.DockerConfigJsonKey: base64.StdEncoding.EncodeToString([]byte(merged))}})
	if err != nil {
		return err
	}
	return guestctl.Oc(ctx, a, "", "patch", "secret", "pull-secret", "-n", "openshift-config", "--type", "merge", "-p", string(patch))
}

// recordLCA notes on vSNO name's VM where its LCA came from.
func recordLCA(ctx context.Context, e Env, ns, name string, b *BuiltLCA) error {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{guest.AnnLCASource: b.String()}}})
	if err != nil {
		return err
	}
	_, err = e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return nil // not a vSNO vsno made; nothing to note it on
	}
	return err
}

// script is an embedded script.
func script(name string) string {
	b, err := files.ReadFile("scripts/" + name)
	if err != nil {
		panic(err) // embedded at build time
	}
	return string(b)
}
