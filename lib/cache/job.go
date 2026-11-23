package cache

import (
	"context"
	"fmt"
	"strings"

	"github.com/sebrandon1/vsno/lib/builder"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// mirrorJobPrefix names the Job that mirrors one release; short(digest)
	// finishes it, so two releases never share one.
	mirrorJobPrefix = "cache-mirror-"
	// mirrorSecretPrefix names the Secret holding one mirror's credentials.
	// It is per release because mirrors queue on the builder's lock, so a
	// second mirror's pod may exist while the first still runs: one shared
	// Secret would hand the queued mirror the wrong credentials.
	mirrorSecretPrefix = "cache-mirror-auth-" // #nosec G101 -- the name of a Secret, not a credential
	// mirrorWork is where the builder's volume is mounted, as in the builder.
	mirrorWork = "/work"
	// mirrorAuth is where the credentials Secret is mounted.
	mirrorAuth = "/auth"
	// builderClaim is the builder's volume, shared so mirror.sh's lock and
	// its work directory are the ones the builder's scripts use.
	builderClaim = "builder-work"
)

// mirrorJob is the Job that mirrors digest.
func mirrorJob(digest string) string { return mirrorJobPrefix + short(digest) }

// mirrorSecret is the Secret holding digest's mirror credentials.
func mirrorSecret(digest string) string { return mirrorSecretPrefix + short(digest) }

// mirrorWrapper runs the two scripts the Job is given, each with the
// credentials it reads on stdin. mirror.sh runs first and signature.sh after
// it: a signature copied before its image would have nothing to sit beside.
const mirrorWrapper = `set -euo pipefail
printf '%s\n' "$1" > /tmp/mirror.sh
printf '%s\n' "$2" > /tmp/signature.sh
cd /auth
tar -cf - auth.json ca.crt | bash /tmp/mirror.sh "$3" "$4" "$5" "$6"
tar -cf - auth.json ca.crt images.txt | bash /tmp/signature.sh "$7" "$5"
`

// startMirrorJob runs mirror.sh and signature.sh in a pod beside the builder.
// The pod mounts the builder's ReadWriteOnce volume, so it runs on the
// builder's node at its SELinux level: a pod at another level would get the
// volume relabeled to its own, locking the builder out.
func startMirrorJob(ctx context.Context, e Env, ns, digest string, files map[string]string, image, host, repo, tag, addr string) error {
	bp, err := builder.ReadyPod(ctx, e.Kube, ns)
	if err != nil {
		return err
	}
	if err := writeMirrorAuth(ctx, e, ns, digest, files); err != nil {
		return err
	}
	jobs := e.Kube.BatchV1().Jobs(ns)
	_ = jobs.Delete(ctx, mirrorJob(digest), deleteOptions()) // a leftover from an interrupted mirror
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: mirrorJob(digest),
			Labels: map[string]string{"app.kubernetes.io/managed-by": "vsno"}},
		Spec: batchv1.JobSpec{
			// One attempt: a mirror that stops part way keeps what it pushed,
			// and starting over silently would hide why it stopped.
			BackoffLimit: int32Ptr(0),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				NodeName:           bp.Spec.NodeName,
				ServiceAccountName: bp.Spec.ServiceAccountName,
				SecurityContext:    sameSELinux(bp),
				RestartPolicy:      corev1.RestartPolicyNever,
				Containers: []corev1.Container{{
					Name:       "mirror",
					Image:      builderImage(bp),
					WorkingDir: mirrorWork,
					Command: []string{"bash", "-c", mirrorWrapper, "mirror",
						script("mirror.sh"), script("signature.sh"), image, host, repo, tag, addr},
					Env:             []corev1.EnvVar{{Name: "PATH", Value: "/work/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin"}},
					SecurityContext: &corev1.SecurityContext{RunAsUser: int64Ptr(0)},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "work", MountPath: mirrorWork},
						{Name: "auth", MountPath: mirrorAuth},
					},
				}},
				Volumes: []corev1.Volume{
					{Name: "work", VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: builderClaim}}},
					{Name: "auth", VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{SecretName: mirrorSecret(digest), DefaultMode: int32Ptr(0o400)}}},
				},
			}},
		},
	}
	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("starting the mirror: %w", err)
	}
	return nil
}

// writeMirrorAuth stores what the mirror and its signature copy read: the
// cluster pull secret with the registry's login added, the registry's CA, and
// the images whose signatures to copy.
func writeMirrorAuth(ctx context.Context, e Env, ns, digest string, files map[string]string) error {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: mirrorSecret(digest), Namespace: ns,
		Labels: map[string]string{"app.kubernetes.io/managed-by": "vsno"}}, StringData: files}
	secrets := e.Kube.CoreV1().Secrets(ns)
	_ = secrets.Delete(ctx, sec.Name, metav1.DeleteOptions{})
	if _, err := secrets.Create(ctx, sec, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("storing the mirror's credentials: %w", err)
	}
	return nil
}

// removeMirrorJob deletes a finished mirror's Job and its credentials.
func removeMirrorJob(ctx context.Context, e Env, ns, digest string) {
	_ = e.Kube.BatchV1().Jobs(ns).Delete(context.WithoutCancel(ctx), mirrorJob(digest), deleteOptions())
	_ = e.Kube.CoreV1().Secrets(ns).Delete(context.WithoutCancel(ctx), mirrorSecret(digest), metav1.DeleteOptions{})
}

// mirrorJobState reports whether the Job exists, whether it finished, and why
// it failed.
func (e Env) mirrorJobState(ctx context.Context, ns, name string) (found, done bool, msg string, err error) {
	job, err := e.Kube.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, false, "", nil
	}
	if err != nil {
		return false, false, "", fmt.Errorf("reading Job %s: %w", name, err)
	}
	done, msg = jobState(job)
	return true, done, msg, nil
}

// jobLog is a finished mirror's log, its last tail lines when tail > 0. A log
// that can't be read is no log: the mirror's own state still says what
// happened.
func jobLog(ctx context.Context, e Env, ns, job string, tail int64) string {
	if e.Logs != nil {
		return e.Logs(ctx, ns, job, tail)
	}
	pods, err := e.Kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + job})
	if err != nil || len(pods.Items) == 0 {
		return ""
	}
	o := &corev1.PodLogOptions{}
	if tail > 0 {
		o.TailLines = &tail
	}
	b, err := e.Kube.CoreV1().Pods(ns).GetLogs(pods.Items[0].Name, o).DoRaw(ctx)
	if err != nil {
		return ""
	}
	return string(b)
}

// sigsFromLog is the last signature summary a mirror printed, if it got that
// far.
func sigsFromLog(logs string) string {
	var last string
	for _, line := range strings.Split(logs, "\n") {
		if sigsRe.MatchString(strings.TrimSpace(line)) {
			last = strings.TrimSpace(line)
		}
	}
	return last
}

// builderImage is the builder container's image: the mirror needs the same oc
// and ca bundle the builder has.
func builderImage(bp *corev1.Pod) string {
	for _, c := range bp.Spec.Containers {
		if c.Name == builder.Container {
			return c.Image
		}
	}
	if c := bp.Spec.Containers; len(c) > 0 {
		return c[0].Image
	}
	return ""
}

// sameSELinux is the builder's SELinux level, so a pod mounting its volume
// does not relabel it.
func int64Ptr(n int64) *int64 { return &n }

func sameSELinux(bp *corev1.Pod) *corev1.PodSecurityContext {
	if bp.Spec.SecurityContext == nil || bp.Spec.SecurityContext.SELinuxOptions == nil {
		return nil
	}
	o := *bp.Spec.SecurityContext.SELinuxOptions
	return &corev1.PodSecurityContext{SELinuxOptions: &o}
}
