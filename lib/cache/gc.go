package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/sebrandon1/vsno/lib/registry"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const (
	// gcJob is the Job that runs the registry's garbage collector.
	gcJob = "registry-gc"
	// gcConfig is where the Job mounts registry-config.
	gcConfig = "/etc/registry"
	// gcTimeout is how long a collection may take.
	gcTimeout = 10 * time.Minute
)

// GarbageCollect frees what deleted images still hold. The registry collects
// only while nothing writes to it, so vsno stops it, runs `registry
// garbage-collect` over its volume in a Job, and starts it again. The registry
// is unavailable while that runs.
func GarbageCollect(ctx context.Context, e Env, ns string) error {
	e.defaults()
	deps := e.Kube.AppsV1().Deployments(ns)
	dep, err := deps.Get(ctx, registry.Service, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading the registry Deployment: %w", err)
	}
	replicas := int32(1)
	if dep.Spec.Replicas != nil {
		replicas = *dep.Spec.Replicas
	}
	if err := scale(ctx, e.Kube, ns, 0); err != nil {
		return err
	}
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
		dep.Spec.Replicas = &replicas
		if _, err := deps.Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("setting the registry's replicas to %d: %w", replicas, err)
		}
		return nil
	})
}

// image is the registry Deployment's container image: the collector must run
// the same registry.
func image(dep *appsv1.Deployment) string {
	if c := dep.Spec.Template.Spec.Containers; len(c) > 0 {
		return c[0].Image
	}
	return ""
}

// waitForPod waits until the registry has a pod (up) or has none (down).
func waitForPod(ctx context.Context, e Env, ns string, up bool) error {
	var last error
	err := wait.PollUntilContextTimeout(ctx, e.poll(), gcTimeout, true, func(ctx context.Context) (bool, error) {
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
	_ = jobs.Delete(ctx, gcJob, metav1.DeleteOptions{}) // a leftover from an interrupted run
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
					{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "registry-config"}}}},
				},
			}},
		},
	}
	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("starting the registry's garbage collection: %w", err)
	}
	defer func() { _ = jobs.Delete(context.WithoutCancel(ctx), gcJob, metav1.DeleteOptions{}) }()
	var failure error
	err := wait.PollUntilContextTimeout(ctx, e.poll(), gcTimeout, true, func(ctx context.Context) (bool, error) {
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
