package cache

import (
	"context"
	"fmt"
	"time"

	"bytes"
	"errors"
	"strings"

	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/manifests"
	"github.com/sebrandon1/vsno/lib/registry"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const (
	// gcJob is the Job that runs the registry's garbage collector.
	gcJob = "registry-gc"
	// gcConfig is where the Job mounts registry-config.
	gcConfig = "/etc/registry"
	// configName is the ConfigMap the collector reads.
	configName = manifests.RegistryConfig
	// gcTimeout is how long a collection may take.
	gcTimeout = 10 * time.Minute
)

// GarbageCollect frees what deleted images still hold. The registry collects
// only while nothing writes to it, so vsno stops it, runs `registry
// garbage-collect` over its volume in a Job, and starts it again. The registry
// is unavailable while that runs.
func GarbageCollect(ctx context.Context, e Env, ns string) error {
	e.defaults()
	if e.mirrorRunning(ctx) {
		return errors.New("a release mirror is running, and stopping the registry would fail it; run this again when it finishes")
	}
	deps := e.Kube.AppsV1().Deployments(ns)
	dep, err := deps.Get(ctx, registry.Service, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading the registry Deployment: %w", err)
	}
	replicas := int32(1)
	if dep.Spec.Replicas != nil {
		replicas = *dep.Spec.Replicas
	}
	if err := e.ensureConfig(ctx, ns); err != nil {
		return err
	}
	if err := scale(ctx, e.Kube, ns, 0); err != nil {
		return err
	}
	// The registry must come back whatever happens next.
	defer func() { _ = scale(context.WithoutCancel(ctx), e.Kube, ns, replicas) }()
	collectErr := func() error {
		if err := waitForPod(ctx, e, ns, false); err != nil {
			return err
		}
		return runGCJob(ctx, e, ns, image(dep))
	}()
	if err := scale(context.WithoutCancel(ctx), e.Kube, ns, replicas); err != nil {
		return fmt.Errorf("%w (the registry is still stopped; `kubectl -n %s scale deploy/%s --replicas=%d`)", err, ns, registry.Service, replicas)
	}
	if collectErr != nil {
		return collectErr
	}
	return waitForPod(ctx, e, ns, true)
}

// scale sets the registry Deployment's replicas.
func scale(ctx context.Context, kube kubernetes.Interface, ns string, replicas int32) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deps := kube.AppsV1().Deployments(ns)
		dep, err := deps.Get(ctx, registry.Service, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("reading the registry Deployment: %w", err)
		}
		if dep.Spec.Replicas != nil && *dep.Spec.Replicas == replicas {
			return nil // already there: the restore after the collector is usually a no-op
		}
		dep.Spec.Replicas = &replicas
		if _, err := deps.Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("setting the registry's replicas to %d: %w", replicas, err)
		}
		return nil
	})
}

// ensureConfig makes sure the collector's ConfigMap is there: environments set
// up before it existed have only the Deployment's environment variables.
func (e Env) ensureConfig(ctx context.Context, ns string) error {
	_, err := e.Kube.CoreV1().ConfigMaps(ns).Get(ctx, configName, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("reading ConfigMap %s: %w", configName, err)
	}
	objs, err := manifests.Render(manifests.RegistryConfig, manifests.NamesConfig(ns))
	if err != nil {
		return err
	}
	for _, o := range objs {
		if o.GetKind() != "ConfigMap" {
			continue
		}
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: o.GetName(),
			Labels: map[string]string{"app.kubernetes.io/managed-by": "vsno"}}}
		cm.Data, _, _ = unstructured.NestedStringMap(o.Object, "data")
		if _, err := e.Kube.CoreV1().ConfigMaps(ns).Create(ctx, cm, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating ConfigMap %s: %w", configName, err)
		}
	}
	return nil
}

// lockScript prints "yes" when a mirror holds the builder's lock.
const lockScript = `exec 9>/work/.mirror.lock
if flock -n 9; then echo no; else echo yes; fi`

// mirrorRunning reports whether a mirror is pushing to the registry. Without a
// builder there is nothing to ask, and no mirror vsno started.
func (e Env) mirrorRunning(ctx context.Context) bool {
	if e.Builder == nil {
		return false
	}
	var out, stderr bytes.Buffer
	if err := e.Builder.Run(ctx, lockScript, nil, builder.IO{Stdout: &out, Stderr: &stderr}); err != nil {
		return false
	}
	return strings.TrimSpace(out.String()) == "yes"
}

// image is the registry Deployment's container image: the collector must run
// the same registry.
func image(dep *appsv1.Deployment) string {
	if c := dep.Spec.Template.Spec.Containers; len(c) > 0 {
		return c[0].Image
	}
	return ""
}

// waitUntil polls cond every e.Poll until it's true, or the collection's
// deadline passes.
func (e Env) waitUntil(ctx context.Context, cond func(context.Context) (bool, error)) error {
	return wait.PollUntilContextTimeout(ctx, e.poll(), gcTimeout, true, cond)
}

// waitForPod waits until the registry has a pod (up) or has none (down).
func waitForPod(ctx context.Context, e Env, ns string, up bool) error {
	var last error
	err := e.waitUntil(ctx, func(ctx context.Context) (bool, error) {
		_, last = registry.Pod(ctx, e.Kube, ns)
		return (last == nil) == up, nil
	})
	if err != nil {
		if up {
			return fmt.Errorf("the registry didn't come back: %w", last)
		}
		return fmt.Errorf("the registry didn't stop: %w", last)
	}
	return nil
}

// runGCJob runs the collector over the registry's volume and waits for it.
func runGCJob(ctx context.Context, e Env, ns, image string) error {
	jobs := e.Kube.BatchV1().Jobs(ns)
	_ = jobs.Delete(ctx, gcJob, deleteOptions()) // a leftover from an interrupted run
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: gcJob,
			Labels: map[string]string{"app.kubernetes.io/managed-by": "vsno"}},
		Spec: batchv1.JobSpec{
			BackoffLimit: int32Ptr(0),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers: []corev1.Container{{
					Name:    "gc",
					Image:   image,
					Command: []string{"/bin/registry", "garbage-collect", gcConfig + "/config.yml"},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "data", MountPath: "/var/lib/registry"},
						{Name: "config", MountPath: gcConfig},
					},
				}},
				Volumes: []corev1.Volume{
					{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "registry-data"}}},
					{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: configName}}}},
				},
			}},
		},
	}
	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("starting the registry's garbage collection: %w", err)
	}
	defer func() { _ = jobs.Delete(context.WithoutCancel(ctx), gcJob, deleteOptions()) }()
	var failure error
	err := e.waitUntil(ctx, func(ctx context.Context) (bool, error) {
		got, err := jobs.Get(ctx, gcJob, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		done, msg := jobState(got)
		if msg != "" {
			failure = fmt.Errorf("the registry's garbage collection failed: %s (see `kubectl -n %s logs job/%s`)", msg, ns, gcJob)
		}
		return done, nil
	})
	if err != nil {
		return fmt.Errorf("waiting for the registry's garbage collection: %w", err)
	}
	return failure
}

// deleteOptions removes the Job's pod with the Job, so a retry leaves nothing
// behind.
func deleteOptions() metav1.DeleteOptions {
	policy := metav1.DeletePropagationBackground
	return metav1.DeleteOptions{PropagationPolicy: &policy}
}

// jobState says whether a Job finished, and why it failed.
func jobState(job *batchv1.Job) (bool, string) {
	for _, c := range job.Status.Conditions {
		switch {
		case c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue:
			return true, ""
		case c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue:
			return true, c.Message + " " + c.Reason
		}
	}
	return false, ""
}

func int32Ptr(n int32) *int32 { return &n }

// poll is how often the cache waits for the registry or the Job.
func (e Env) poll() time.Duration {
	if e.Poll > 0 {
		return e.Poll
	}
	return 2 * time.Second
}
