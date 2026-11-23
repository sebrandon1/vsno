package cache

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/release"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	testDigest = "sha256:1111111111111111aaaa"
	testRepo   = "ocp/release/1111111111111111"
	testTag    = "1111111111111111"
	testSigs   = "copied 3, present 0, none 0"
)

// mirrorEnv is an environment whose registry holds 100Gi, with `free` bytes
// left, whose builder answers the scripts, and whose builder pod is ready.
func mirrorEnv(t *testing.T, free int64, b builder.Runner) Env {
	t.Helper()
	k := kfake.NewClientset(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "registry-auth"},
			Data: map[string][]byte{"username": []byte("u"), "password": []byte("p")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "registry-ca"}, Data: map[string][]byte{"ca.crt": []byte("CA")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "openshift-config", Name: "pull-secret"},
			Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"quay.io":{"auth":"QQ=="}}}`)}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "registry"}, Spec: corev1.ServiceSpec{ClusterIP: "172.30.0.5"}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "builder-abc", Labels: map[string]string{"app": "builder"}},
			Spec: corev1.PodSpec{
				NodeName: "node-1", ServiceAccountName: "vsno",
				SecurityContext: &corev1.PodSecurityContext{SELinuxOptions: &corev1.SELinuxOptions{Level: "s0:c1,c2"}},
				Containers:      []corev1.Container{{Name: builder.Container, Image: "quay.io/centos/centos:stream9"}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
		},
	)
	const total = 100 << 30
	used := total - free
	return Env{
		Kube: k, Builder: b, Now: func() time.Time { return time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC) },
		Usage: func(context.Context, string) (int64, int64, error) { return used, total, nil },
	}
}

// jobs answers for the Job a mirror starts. found is whether the Job still
// exists, so a test can say it was deleted behind vsno's back; done and failed
// are its outcome; logs is what its pod printed.
type jobs struct {
	found  bool
	done   bool
	failed bool
	logs   string
	// created are the Jobs vsno started, deleted the ones it removed.
	created []*batchv1.Job
	deleted []string
}

// newJobs is a Job that is running.
func newJobs() *jobs { return &jobs{found: true} }

// attach answers Get and Create for Jobs, and gives the Env its log.
func (j *jobs) attach(e Env) Env {
	k, ok := e.Kube.(*kfake.Clientset)
	if !ok {
		return e
	}
	k.PrependReactor("create", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		job, _ := a.(k8stesting.CreateAction).GetObject().(*batchv1.Job)
		j.created = append(j.created, job)
		return true, job, nil
	})
	k.PrependReactor("delete", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		j.deleted = append(j.deleted, a.(k8stesting.DeleteAction).GetName())
		return true, nil, nil
	})
	k.PrependReactor("get", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		ga, _ := a.(k8stesting.GetAction)
		if !j.found {
			return true, nil, apierrors.NewNotFound(batchv1.Resource("jobs"), ga.GetName())
		}
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Namespace: ga.GetNamespace(), Name: ga.GetName()},
			Status:     batchv1.JobStatus{Conditions: j.conditions()},
		}, nil
	})
	e.Logs = func(context.Context, string, string, int64) string { return j.logs }
	return e
}

func (j *jobs) conditions() []batchv1.JobCondition {
	switch {
	case !j.done:
		return nil
	case j.failed:
		return []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
			Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit"}}
	}
	return []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
}

// mirrorRunner answers release-info.sh, registry.sh and signature.sh. A fresh
// mirror runs no script through the builder any more: its Job does, and what it
// prints arrives as the Job's log (see jobs.logs).
type mirrorRunner struct {
	runner
	// sigArgs are signature.sh's arguments and sigImages its images.txt;
	// sigOut is what it prints (all copied when empty) and sigErr makes it fail.
	sigArgs   []string
	sigImages string
	sigOut    string
	sigErr    error
}

func (m *mirrorRunner) Run(ctx context.Context, s string, args []string, io_ builder.IO) error {
	if s != script("signature.sh") {
		return m.runner.Run(ctx, s, args, io_)
	}
	m.sigArgs = args
	if io_.Stdin != nil {
		b, _ := io.ReadAll(io_.Stdin)
		m.sigImages = tarFilesOf(b)["images.txt"]
	}
	if m.sigErr != nil {
		_, _ = io.WriteString(io_.Stderr, "reading the signature: denied")
		return m.sigErr
	}
	out := m.sigOut
	if out == "" {
		out = testSigs
	}
	_, _ = io.WriteString(io_.Stdout, out+"\n")
	return nil
}

var rel = release.Release{Requested: "4.20.46", Version: "4.20.46", Image: "quay.io/openshift-release-dev/ocp-release:4.20.46-x86_64"}

// Start does the fast work and leaves the mirror to its Job: that is what
// makes `cache add` return in seconds.
func TestStartLeavesTheMirrorRunning(t *testing.T) {
	ctx := context.Background()
	j := newJobs()
	e := j.attach(mirrorEnv(t, 50<<30, &mirrorRunner{runner: runner{out: releaseInfo}}))
	p, err := Start(ctx, e, EnsureOptions{Namespace: ns, Release: rel})
	if err != nil {
		t.Fatal(err)
	}
	if p.Hit || !p.Running {
		t.Errorf("hit = %v, running = %v", p.Hit, p.Running)
	}
	if p.Job != mirrorJob(testDigest) || p.State != StateMirroring {
		t.Errorf("entry = %+v, want a mirroring one naming its Job", p.Entry)
	}
	if p.BaselineBytes != 50<<30 {
		t.Errorf("baseline = %d, want the volume's usage when the mirror began", p.BaselineBytes)
	}
	if !strings.Contains(p.PullSecret, "172.30.0.5:5000") || p.TrustBundle != "CA" {
		t.Errorf("pull secret = %q, trust bundle = %q", p.PullSecret, p.TrustBundle)
	}
	// What a vSNO installs with: both the payload's repository and the release
	// image's own are mapped to the mirror.
	var mapped []string
	for _, s := range p.Sources {
		mapped = append(mapped, s.Source)
	}
	if want := []string{"quay.io/openshift-release-dev/ocp-release", "quay.io/openshift-release-dev/ocp-v4.0-art-dev"}; !slices.Equal(mapped, want) {
		t.Errorf("sources = %q, want %q", mapped, want)
	}
	if len(j.created) != 1 {
		t.Fatalf("started %d Jobs, want 1", len(j.created))
	}
	checkMirrorJob(t, j.created[0])
	checkMirrorSecret(t, e)
}

// checkMirrorJob is the Job's shape: it must run beside the builder, on its
// node at its SELinux level, because it mounts the builder's volume.
func checkMirrorJob(t *testing.T, job *batchv1.Job) {
	t.Helper()
	spec := job.Spec.Template.Spec
	if spec.NodeName != "node-1" || spec.ServiceAccountName != "vsno" {
		t.Errorf("node = %q, service account = %q", spec.NodeName, spec.ServiceAccountName)
	}
	if spec.SecurityContext == nil || spec.SecurityContext.SELinuxOptions == nil ||
		spec.SecurityContext.SELinuxOptions.Level != "s0:c1,c2" {
		t.Errorf("security context = %+v, want the builder's SELinux level", spec.SecurityContext)
	}
	c := spec.Containers[0]
	if c.Image != "quay.io/centos/centos:stream9" {
		t.Errorf("image = %q, want the builder's", c.Image)
	}
	args := c.Command
	// The mirror and its signature copy target the address the credentials
	// name (the ClusterIP), which is also what guests pull from.
	for _, want := range []string{script("mirror.sh"), script("signature.sh"), rel.Image, "172.30.0.5:5000", testRepo, testTag} {
		if !slices.Contains(args, want) {
			t.Errorf("Job arguments lack %q", want)
		}
	}
	if len(spec.Volumes) != 2 || spec.Volumes[0].PersistentVolumeClaim.ClaimName != builderClaim ||
		spec.Volumes[1].Secret.SecretName != mirrorSecret(testDigest) {
		t.Errorf("volumes = %+v", spec.Volumes)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Error("the Job would retry a mirror that stopped")
	}
}

// checkMirrorSecret is what the Job reads: the credentials, and the images
// whose signatures to copy.
func checkMirrorSecret(t *testing.T, e Env) {
	t.Helper()
	sec, err := e.Kube.CoreV1().Secrets(ns).Get(context.Background(), mirrorSecret(testDigest), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sec.StringData["ca.crt"] != "CA" || !strings.Contains(sec.StringData["auth.json"], "172.30.0.5:5000") {
		t.Errorf("credentials = %v", sec.StringData)
	}
	images := sec.StringData["images.txt"]
	if !strings.Contains(images, "quay.io/openshift-release-dev/ocp-release@sha256:1111111111111111aaaa") ||
		!strings.Contains(images, "ocp-v4.0-art-dev@sha256:aaa") {
		t.Errorf("images.txt = %q", images)
	}
}

// Ensure waits for the Job, so create and run see a release that is ready.
func TestEnsureWaitsForTheJob(t *testing.T) {
	ctx := context.Background()
	j := &jobs{found: true, done: true, logs: "mirroring x\n" + testSigs + "\n"}
	r := &mirrorRunner{runner: runner{out: releaseInfo, size: "23371317248"}}
	e := j.attach(mirrorEnv(t, 50<<30, r))
	// A clock that moves, so the mirror takes time.
	at := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	var ticks int
	e.Now = func() time.Time { ticks++; return at.Add(time.Duration(ticks) * time.Minute) }
	var progress []string
	p, err := Ensure(ctx, e, EnsureOptions{Namespace: ns, Release: rel,
		OnLine: func(s string) { progress = append(progress, s) }})
	if err != nil {
		t.Fatal(err)
	}
	checkFinishedMirror(t, p)
	if len(progress) == 0 || progress[len(progress)-1] != "signatures: "+testSigs {
		t.Errorf("progress = %q", progress)
	}
	// A finished mirror's Job and credentials are not needed any more.
	if !slices.Contains(j.deleted, mirrorJob(testDigest)) {
		t.Errorf("the Job was left behind: deleted %q", j.deleted)
	}
	if _, err := e.Kube.CoreV1().Secrets(ns).Get(ctx, mirrorSecret(testDigest), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the credentials were left behind: %v", err)
	}
	m, err := load(ctx, e.Kube, ns)
	if err != nil {
		t.Fatal(err)
	}
	if ent := m.Entries[testDigest]; ent == nil || ent.State != "" || ent.Job != "" || ent.BaselineBytes != 0 {
		t.Errorf("entry = %+v, want a ready one", ent)
	}
}

// checkFinishedMirror is what Ensure leaves behind once the Job completed.
func checkFinishedMirror(t *testing.T, p *Prepared) {
	t.Helper()
	if p.Hit || p.Running || p.State != "" {
		t.Errorf("hit = %v, running = %v, state = %q", p.Hit, p.Running, p.State)
	}
	if p.SizeBytes != 23371317248 || !p.Measured {
		t.Errorf("size = %d, measured = %v", p.SizeBytes, p.Measured)
	}
	if p.Signatures != testSigs {
		t.Errorf("signatures = %q, want the Job's summary", p.Signatures)
	}
	if p.Elapsed <= 0 {
		t.Errorf("elapsed = %v", p.Elapsed)
	}
}

// A release that is already mirrored is a hit: nothing is started, and the
// entry keeps what it was mirrored as.
func TestEnsureHit(t *testing.T) {
	ctx := context.Background()
	j := newJobs()
	r := &mirrorRunner{runner: runner{out: releaseInfo, exists: "yes"}}
	e := j.attach(mirrorEnv(t, 50<<30, r))
	at := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	_ = update(ctx, e.Kube, ns, func(m *metadata) {
		m.Entries[testDigest] = &Entry{Version: "4.20.46", Image: rel.Image, Digest: testDigest, Repo: testRepo,
			Mirrored: at, SizeBytes: 7 << 30, Requested: []string{"4.20"}}
	})
	p, err := Ensure(ctx, e, EnsureOptions{Namespace: ns, Release: rel})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Hit || p.Running || len(j.created) != 0 {
		t.Errorf("hit = %v, running = %v, jobs = %d", p.Hit, p.Running, len(j.created))
	}
	if !p.Mirrored.Equal(at) || p.SizeBytes != 7<<30 {
		t.Errorf("entry = %+v, want the recorded date and size", p.Entry)
	}
	if want := []string{"4.20", "4.20.46"}; !slices.Equal(p.Requested, want) {
		t.Errorf("requested = %q, want %q", p.Requested, want)
	}
	// A release cached before signatures were copied gets theirs here.
	if len(r.sigArgs) == 0 || !strings.Contains(r.sigImages, "ocp-v4.0-art-dev@sha256:aaa") {
		t.Errorf("signature args = %q, images = %q", r.sigArgs, r.sigImages)
	}
}

// The record can outlive the images (a prune that failed halfway, a manual
// delete): that is not a hit, or the guest installs from upstream believing it
// is reading the mirror.
func TestEnsureMirrorsWhenTheImagesAreGone(t *testing.T) {
	ctx := context.Background()
	j := newJobs()
	r := &mirrorRunner{runner: runner{out: releaseInfo, exists: "no"}}
	e := j.attach(mirrorEnv(t, 50<<30, r))
	_ = update(ctx, e.Kube, ns, func(m *metadata) {
		m.Entries[testDigest] = &Entry{Version: "4.20.46", Image: rel.Image, Digest: testDigest, Repo: testRepo,
			Mirrored: time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC), SizeBytes: 7 << 30}
	})
	p, err := Start(ctx, e, EnsureOptions{Namespace: ns, Release: rel})
	if err != nil {
		t.Fatal(err)
	}
	if p.Hit || !p.Running || len(j.created) != 1 {
		t.Errorf("hit = %v, running = %v, jobs = %d", p.Hit, p.Running, len(j.created))
	}
	if len(r.registryCalls) == 0 || r.registryCalls[0][1] != "exists" {
		t.Errorf("registry calls = %q", r.registryCalls)
	}
}

// A second add while a mirror runs follows that Job instead of starting
// another: two mirrors of one release would fight over the same work dir.
func TestStartFollowsARunningMirror(t *testing.T) {
	ctx := context.Background()
	j := newJobs()
	e := j.attach(mirrorEnv(t, 50<<30, &mirrorRunner{runner: runner{out: releaseInfo, exists: "no"}}))
	_ = update(ctx, e.Kube, ns, func(m *metadata) {
		m.Entries[testDigest] = &Entry{Version: "4.20.46", Image: rel.Image, Digest: testDigest, Repo: testRepo,
			State: StateMirroring, Started: time.Date(2026, 11, 20, 11, 59, 0, 0, time.UTC), Job: mirrorJob(testDigest)}
	})
	p, err := Start(ctx, e, EnsureOptions{Namespace: ns, Release: rel})
	if err != nil {
		t.Fatal(err)
	}
	if p.Hit || !p.Running || p.Job != mirrorJob(testDigest) {
		t.Errorf("hit = %v, running = %v, job = %q", p.Hit, p.Running, p.Job)
	}
	if len(j.created) != 0 {
		t.Errorf("started %d more Jobs", len(j.created))
	}
}

// A Job that finished while nothing was watching is settled by the next
// command, so a second add does not wait for a mirror that already ended.
func TestStartSettlesAFinishedMirror(t *testing.T) {
	ctx := context.Background()
	j := &jobs{found: true, done: true, logs: testSigs}
	r := &mirrorRunner{runner: runner{out: releaseInfo, exists: "yes", size: "7"}}
	e := j.attach(mirrorEnv(t, 50<<30, r))
	_ = update(ctx, e.Kube, ns, func(m *metadata) {
		m.Entries[testDigest] = &Entry{Version: "4.20.46", Image: rel.Image, Digest: testDigest, Repo: testRepo,
			State: StateMirroring, Job: mirrorJob(testDigest), BaselineBytes: 1 << 30}
	})
	p, err := Ensure(ctx, e, EnsureOptions{Namespace: ns, Release: rel})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Hit || p.Running || len(j.created) != 0 {
		t.Errorf("hit = %v, running = %v, jobs = %d", p.Hit, p.Running, len(j.created))
	}
}

// A mirror that failed is retried by the next add rather than blocking it.
func TestStartRetriesAFailedMirror(t *testing.T) {
	ctx := context.Background()
	j := &jobs{found: true, done: true, failed: true, logs: "last line"}
	e := j.attach(mirrorEnv(t, 50<<30, &mirrorRunner{runner: runner{out: releaseInfo}}))
	_ = update(ctx, e.Kube, ns, func(m *metadata) {
		m.Entries[testDigest] = &Entry{Version: "4.20.46", Image: rel.Image, Digest: testDigest, Repo: testRepo,
			State: StateMirroring, Job: mirrorJob(testDigest)}
	})
	p, err := Start(ctx, e, EnsureOptions{Namespace: ns, Release: rel})
	if err != nil {
		t.Fatal(err)
	}
	if p.Hit || !p.Running || len(j.created) != 1 {
		t.Errorf("hit = %v, running = %v, jobs = %d", p.Hit, p.Running, len(j.created))
	}
}

// A mirror that fails says why, from the Job's log, and records it so
// `cache list` shows it.
func TestEnsureReportsAStoppedMirror(t *testing.T) {
	ctx := context.Background()
	j := &jobs{found: true, done: true, failed: true, logs: "uploading images\none or more errors occurred\n"}
	e := j.attach(mirrorEnv(t, 50<<30, &mirrorRunner{runner: runner{out: releaseInfo}}))
	_, err := Ensure(ctx, e, EnsureOptions{Namespace: ns, Release: rel})
	if err == nil || !strings.Contains(err.Error(), "one or more errors occurred") {
		t.Errorf("got %v", err)
	}
	m, lerr := load(ctx, e.Kube, ns)
	if lerr != nil {
		t.Fatal(lerr)
	}
	ent := m.Entries[testDigest]
	if ent == nil || ent.State != StateFailed {
		t.Errorf("entry = %+v, want a failed one", ent)
	}
	if ent.Job == "" {
		t.Error("the failed entry lost its Job's name, so nothing can point at the log")
	}
}

// A Job that is gone will never finish the mirror: saying so beats waiting.
func TestReconcileFailsAMirrorWhoseJobIsGone(t *testing.T) {
	ctx := context.Background()
	j := newJobs()
	j.found = false
	e := j.attach(mirrorEnv(t, 50<<30, &mirrorRunner{runner: runner{out: releaseInfo}}))
	_ = update(ctx, e.Kube, ns, func(m *metadata) {
		m.Entries[testDigest] = &Entry{Version: "4.20.46", Image: rel.Image, Digest: testDigest, Repo: testRepo,
			State: StateMirroring, Job: mirrorJob(testDigest), BaselineBytes: 1 << 30}
	})
	if err := Reconcile(ctx, e, ns); err != nil {
		t.Fatal(err)
	}
	m, err := load(ctx, e.Kube, ns)
	if err != nil {
		t.Fatal(err)
	}
	if ent := m.Entries[testDigest]; ent.State != StateFailed || ent.BaselineBytes != 0 {
		t.Errorf("entry = %+v, want failed", ent)
	}
}

// Reconcile records a mirror that finished unwatched, so `cache list` tells
// the truth about it.
func TestReconcileFinishesAMirror(t *testing.T) {
	ctx := context.Background()
	j := &jobs{found: true, done: true, logs: testSigs}
	r := &mirrorRunner{runner: runner{out: releaseInfo, size: "7"}}
	e := j.attach(mirrorEnv(t, 50<<30, r))
	_ = update(ctx, e.Kube, ns, func(m *metadata) {
		m.Entries[testDigest] = &Entry{Version: "4.20.46", Image: rel.Image, Digest: testDigest, Repo: testRepo,
			State: StateMirroring, Job: mirrorJob(testDigest)}
	})
	if err := Reconcile(ctx, e, ns); err != nil {
		t.Fatal(err)
	}
	m, err := load(ctx, e.Kube, ns)
	if err != nil {
		t.Fatal(err)
	}
	if ent := m.Entries[testDigest]; ent.State != "" || ent.SizeBytes != 7 || ent.Signatures != testSigs {
		t.Errorf("entry = %+v, want ready with its size and signatures", ent)
	}
}

func TestEnsureRefusesWhenFull(t *testing.T) {
	j := newJobs()
	e := j.attach(mirrorEnv(t, 2<<30, &mirrorRunner{runner: runner{out: releaseInfo}}))
	_, err := Start(context.Background(), e, EnsureOptions{Namespace: ns, Release: rel})
	if err == nil || !strings.Contains(err.Error(), "has 2.0Gi free") || !strings.Contains(err.Error(), "cache prune") {
		t.Errorf("got %v", err)
	}
	if len(j.created) != 0 {
		t.Error("mirrored into a full volume")
	}
}

func TestStartWarnsWhenNearlyFull(t *testing.T) {
	j := newJobs()
	var progress []string
	// 22Gi free: enough for a 20Gi estimate, but under 25Gi.
	e := j.attach(mirrorEnv(t, 22<<30, &mirrorRunner{runner: runner{out: releaseInfo}}))
	if _, err := Start(context.Background(), e, EnsureOptions{Namespace: ns, Release: rel,
		OnLine: func(s string) { progress = append(progress, s) }}); err != nil {
		t.Fatal(err)
	}
	if len(progress) == 0 || !strings.Contains(progress[0], "needs about") {
		t.Errorf("progress = %q", progress)
	}
}

func TestStartErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("no sources", func(t *testing.T) {
		r := &mirrorRunner{runner: runner{out: `{"digest":"sha256:1"}`}}
		j := newJobs()
		e := j.attach(mirrorEnv(t, 50<<30, r))
		if _, err := Start(ctx, e, EnsureOptions{Namespace: ns, Release: rel}); err == nil || !strings.Contains(err.Error(), "no images") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("no registry", func(t *testing.T) {
		e := mirrorEnv(t, 50<<30, &mirrorRunner{runner: runner{out: releaseInfo}})
		e.Kube = kfake.NewClientset()
		if _, err := Start(ctx, e, EnsureOptions{Namespace: ns, Release: rel}); err == nil || !strings.Contains(err.Error(), "rerun `vsno setup") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("no builder pod", func(t *testing.T) {
		e := mirrorEnv(t, 50<<30, &mirrorRunner{runner: runner{out: releaseInfo}})
		if err := e.Kube.CoreV1().Pods(ns).Delete(ctx, "builder-abc", metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := Start(ctx, e, EnsureOptions{Namespace: ns, Release: rel}); err == nil || !strings.Contains(err.Error(), "builder pod") {
			t.Errorf("got %v", err)
		}
	})
}

func TestEnsureHitChecksTheSignature(t *testing.T) {
	j := newJobs()
	r := &mirrorRunner{runner: runner{out: releaseInfo, exists: "yes"}, sigOut: "copied 0, present 3, none 0"}
	e := j.attach(mirrorEnv(t, 50<<30, r))
	_ = update(context.Background(), e.Kube, ns, func(m *metadata) {
		m.Entries[testDigest] = &Entry{Version: "4.20.46", Image: rel.Image, Digest: testDigest, Repo: testRepo}
	})
	p, err := Ensure(context.Background(), e, EnsureOptions{Namespace: ns, Release: rel})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Hit || p.Signatures != "" {
		t.Errorf("hit = %v, signatures = %q", p.Hit, p.Signatures)
	}
	// Every image's signature is checked, not only a new mirror's.
	if len(r.sigArgs) == 0 {
		t.Error("signature.sh did not run for a hit")
	}
}

func TestEnsureFailsWhenTheSignatureCantBeCopied(t *testing.T) {
	t.Run("copy fails", func(t *testing.T) {
		j := newJobs()
		r := &mirrorRunner{runner: runner{out: releaseInfo, exists: "yes"}, sigErr: errors.New("exit 1")}
		e := j.attach(mirrorEnv(t, 50<<30, r))
		_ = update(context.Background(), e.Kube, ns, func(m *metadata) {
			m.Entries[testDigest] = &Entry{Version: "4.20.46", Image: rel.Image, Digest: testDigest, Repo: testRepo}
		})
		_, err := Ensure(context.Background(), e, EnsureOptions{Namespace: ns, Release: rel})
		if err == nil || !strings.Contains(err.Error(), "denied") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("unexpected output", func(t *testing.T) {
		j := newJobs()
		r := &mirrorRunner{runner: runner{out: releaseInfo, exists: "yes"}, sigOut: "nothing to say"}
		e := j.attach(mirrorEnv(t, 50<<30, r))
		_ = update(context.Background(), e.Kube, ns, func(m *metadata) {
			m.Entries[testDigest] = &Entry{Version: "4.20.46", Image: rel.Image, Digest: testDigest, Repo: testRepo}
		})
		_, err := Ensure(context.Background(), e, EnsureOptions{Namespace: ns, Release: rel})
		if err == nil || !strings.Contains(err.Error(), "unexpected output") {
			t.Errorf("got %v", err)
		}
	})
}

// ProgressAt is what `cache list` shows for a mirror it did not start: the
// volume's growth since the mirror began, against the estimate.
func TestProgressAt(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 10, 0, 0, time.UTC)
	ent := Entry{State: StateMirroring, Started: now.Add(-5 * time.Minute), BaselineBytes: 40 << 30}
	p := ent.ProgressAt(now, 44<<30, 20<<30)
	if p == nil {
		t.Fatal("no progress for a running mirror")
	}
	if p.BytesComplete != 4<<30 || p.Percent != 20 {
		t.Errorf("complete = %d, percent = %v", p.BytesComplete, p.Percent)
	}
	if p.BytesPerSecond <= 0 || p.ETASeconds <= 0 {
		t.Errorf("rate = %v, eta = %v", p.BytesPerSecond, p.ETASeconds)
	}
	if want := "4.0Gi of ~20.0Gi (20%), 14MiB/s"; !strings.Contains(p.Text(), want) {
		t.Errorf("text = %q, want %q", p.Text(), want)
	}
	if e := (Entry{State: ""}).ProgressAt(now, 0, 0); e != nil {
		t.Errorf("a ready entry has progress %+v", e)
	}
	// A volume that shrank, or has not moved yet, says only how long it has
	// been running.
	q := Entry{State: StateMirroring, Started: now.Add(-2 * time.Minute)}.ProgressAt(now, 40<<30, 20<<30)
	if q.BytesComplete != 0 || q.Text() != "2m0s" {
		t.Errorf("progress = %+v, text = %q", q, q.Text())
	}
}

// The credentials are a Secret volume, so their files are symlinks into
// ..data; copying them as links leaves the scripts reading dangling ones.
func TestMirrorWrapperDereferencesTheCredentials(t *testing.T) {
	for _, want := range []string{"tar -chf - auth.json ca.crt |", "tar -chf - auth.json ca.crt images.txt |"} {
		if !strings.Contains(mirrorWrapper, want) {
			t.Errorf("the wrapper does not dereference the Secret's links: %q missing", want)
		}
	}
}

func TestSigsFromLog(t *testing.T) {
	for _, c := range []struct{ logs, want string }{
		{"", ""},
		{"mirroring x\n" + testSigs + "\n", testSigs},
		{"copied 1, present 0, none 0\nnoise\ncopied 2, present 0, none 1\n", "copied 2, present 0, none 1"},
	} {
		if got := sigsFromLog(c.logs); got != c.want {
			t.Errorf("sigsFromLog(%q) = %q, want %q", c.logs, got, c.want)
		}
	}
}

func TestRate(t *testing.T) {
	for _, c := range []struct {
		bytes int64
		d     time.Duration
		want  string
	}{{0, time.Second, "-"}, {1 << 20, 0, "-"}, {60 << 20, time.Second, "60MiB/s"}, {1 << 30, 2 * time.Second, "512MiB/s"}} {
		if got := Rate(c.bytes, c.d); got != c.want {
			t.Errorf("Rate(%d, %v) = %q, want %q", c.bytes, c.d, got, c.want)
		}
	}
}

// tarFilesOf reads a tar of files.
func tarFilesOf(b []byte) map[string]string {
	out := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		data, _ := io.ReadAll(tr)
		out[h.Name] = string(data)
	}
	return out
}
