package cache

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/builder"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// pruneEnv has two cached releases, one in use, and a registry pod.
func pruneEnv(t *testing.T) Env {
	t.Helper()
	e := mirrorEnv(t, 50<<30, &mirrorRunner{runner: runner{out: releaseInfo}})
	at := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	_ = update(context.Background(), e.Kube, ns, func(m *metadata) {
		m.Entries["sha256:aaaa"] = &Entry{Version: "4.20.46", Image: "quay.io/x:4.20.46", Digest: "sha256:aaaa",
			Repo: "ocp/release/aaaa", SizeBytes: 6 << 30, Mirrored: at, LastUsed: at}
		m.Entries["sha256:bbbb"] = &Entry{Version: "4.21.0", Image: "quay.io/x:4.21.0", Digest: "sha256:bbbb",
			Repo: "ocp/release/bbbb", SizeBytes: 3 << 30, Mirrored: at, LastUsed: at.Add(-24 * time.Hour)}
	})
	return e
}

func TestSelectEntries(t *testing.T) {
	rep := &Report{Entries: []Entry{
		{Version: "4.20.46", Image: "quay.io/x:4.20.46", Digest: "sha256:aaaa", Repo: "ocp/release/aaaa", SizeBytes: 6 << 30, LastUsed: time.Unix(10, 0), InUse: true, Users: []string{"a"}},
		{Version: "4.21.0", Image: "quay.io/x:4.21.0", Digest: "sha256:bbbb", Repo: "ocp/release/bbbb", SizeBytes: 3 << 30, LastUsed: time.Unix(20, 0)},
		{Version: "4.22.0", Image: "quay.io/x:4.22.0", Digest: "sha256:cccc", Repo: "ocp/release/cccc", SizeBytes: 2 << 30, LastUsed: time.Unix(30, 0), Requested: []string{"4.22"}},
	}}
	// No argument: unused ones beyond --keep, newest used first.
	p, err := selectEntries(rep, PruneOptions{Keep: 0})
	if err != nil || len(p.Entries) != 2 || p.Entries[0].Version != "4.22.0" || p.Entries[1].Version != "4.21.0" {
		t.Fatalf("plan = %+v, %v", p, err)
	}
	if p.Bytes != 5<<30 {
		t.Errorf("bytes = %d", p.Bytes)
	}
	if p, _ := selectEntries(rep, PruneOptions{Keep: 1}); len(p.Entries) != 1 || p.Entries[0].Version != "4.21.0" {
		t.Errorf("keep 1 = %+v", p.Entries)
	}
	// An in-use release is not picked up by the default plan.
	if p, _ := selectEntries(rep, PruneOptions{}); slices.ContainsFunc(p.Entries, func(e Entry) bool { return e.Version == "4.20.46" }) {
		t.Errorf("default plan took the in-use release: %+v", p.Entries)
	}
}

func TestSelectEntriesByName(t *testing.T) {
	rep := &Report{Entries: []Entry{
		{Version: "4.20.46", Image: "quay.io/x:4.20.46", Digest: "sha256:aaaa", InUse: true, Users: []string{"a"}},
		{Version: "4.22.0", Image: "quay.io/x:4.22.0", Digest: "sha256:cccc", Requested: []string{"4.22"}},
	}}
	// Any form the release was asked for names it.
	for _, arg := range []string{"4.22.0", "4.22", "quay.io/x:4.22.0", "sha256:cccc", "cccc"} {
		p, err := selectEntries(rep, PruneOptions{Version: arg})
		if err != nil || len(p.Entries) != 1 || p.Entries[0].Version != "4.22.0" {
			t.Errorf("%s = %+v, %v", arg, p.Entries, err)
		}
	}
	if _, err := selectEntries(rep, PruneOptions{Version: "4.20.46"}); err == nil || !strings.Contains(err.Error(), "vSNO a still runs") {
		t.Errorf("in use: %v", err)
	}
	if _, err := selectEntries(rep, PruneOptions{Version: "9.9.9"}); err == nil || !strings.Contains(err.Error(), "no cached release") {
		t.Errorf("unknown: %v", err)
	}
}

func TestPruneRefusesInUse(t *testing.T) {
	e := pruneEnv(t)
	users := map[string][]string{"quay.io/x:4.20.46": {"a"}}
	for _, c := range []struct {
		o    PruneOptions
		want string
	}{
		{PruneOptions{Namespace: ns, Version: "4.20.46", Users: users}, "still runs"},
		{PruneOptions{Namespace: ns, Version: "9.9.9", Users: users}, "no cached release"},
	} {
		_, err := Prune(context.Background(), e, c.o)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v", c.o, err)
		}
	}
}

func TestPruneDryRun(t *testing.T) {
	e := pruneEnv(t)
	running := e.Builder.(*mirrorRunner)
	res, err := Prune(context.Background(), e, PruneOptions{Namespace: ns, Version: "4.21.0", DryRun: true})
	if err != nil || len(res.Removed) != 0 || len(res.Plan.Entries) != 1 {
		t.Fatalf("res = %+v, %v", res, err)
	}
	if len(running.ran) != 0 {
		t.Errorf("ran %q", running.ran)
	}
}

func TestPruneNotConfirmed(t *testing.T) {
	e := pruneEnv(t)
	shown := 0
	_, err := Prune(context.Background(), e, PruneOptions{Namespace: ns, Version: "4.21.0",
		ShowPlan: func(p *PrunePlan) { shown = len(p.Entries) }, Confirm: func(_, a string) bool {
			// y, or the word the prompt asked for before.
			if a != "y|prune" {
				t.Errorf("confirm answer = %q", a)
			}
			return false
		}})
	if err == nil || !strings.Contains(err.Error(), "not confirmed") || shown != 1 {
		t.Errorf("err %v, shown %d", err, shown)
	}
}

// regWorld is a namespace whose registry scales and whose Job finishes as
// jobCondition says.
type regWorld struct {
	k            *kfake.Clientset
	replicas     int32
	seen         []int32
	jobCondition batchv1.JobCondition
}

func newRegWorld(t *testing.T, cond batchv1.JobCondition) *regWorld {
	t.Helper()
	w := &regWorld{replicas: 1, jobCondition: cond}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "registry"},
		Spec: appsv1.DeploymentSpec{Replicas: &w.replicas,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "registry", Image: "quay.io/libpod/registry:2.8.2"}}}}},
	}
	w.k = kfake.NewClientset(dep,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "registry-auth"},
			Data: map[string][]byte{"username": []byte("u"), "password": []byte("p")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "registry-ca"}, Data: map[string][]byte{"ca.crt": []byte("CA")}},
	)
	w.k.PrependReactor("update", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		dep := a.(k8stesting.UpdateAction).GetObject().(*appsv1.Deployment)
		w.replicas = *dep.Spec.Replicas
		w.seen = append(w.seen, w.replicas)
		return false, nil, nil // the tracker stores it
	})
	// A stopped registry has no pod; a running one has a pod.
	w.k.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		list := &corev1.PodList{}
		if w.replicas > 0 {
			list.Items = []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "registry-0",
				Labels: map[string]string{"app": "registry"}},
				Status: corev1.PodStatus{Phase: corev1.PodRunning}}}
		}
		return true, list, nil
	})
	w.k.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: gcJob},
			Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{w.jobCondition}}}
		return true, j, nil
	})
	return w
}

func (w *regWorld) env() Env {
	return Env{Kube: w.k, Poll: time.Millisecond,
		Usage: func(context.Context, string) (int64, int64, error) { return 0, 100 << 30, nil }}
}

func TestGarbageCollect(t *testing.T) {
	w := newRegWorld(t, batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})
	if err := GarbageCollect(context.Background(), w.env(), ns); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(w.seen, []int32{0, 1}) {
		t.Errorf("scaled %v, want down then up", w.seen)
	}
	if _, err := w.k.CoreV1().ConfigMaps(ns).Get(context.Background(), configName, metav1.GetOptions{}); err != nil {
		t.Errorf("the collector's ConfigMap wasn't created: %v", err)
	}
	jobs, _ := w.k.BatchV1().Jobs(ns).List(context.Background(), metav1.ListOptions{})
	for _, j := range jobs.Items {
		if j.Name == gcJob {
			t.Error("the Job was left behind")
		}
	}
}

func TestGarbageCollectRestartsOnJobFailure(t *testing.T) {
	w := newRegWorld(t, batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "blobs are read-only", Reason: "Failed"})
	err := GarbageCollect(context.Background(), w.env(), ns)
	if err == nil || !strings.Contains(err.Error(), "blobs are read-only") {
		t.Errorf("got %v", err)
	}
	if !slices.Equal(w.seen, []int32{0, 1}) {
		t.Errorf("scaled %v: the registry must come back after a failed job", w.seen)
	}
}

func TestPruneDeletesAndCollects(t *testing.T) {
	ctx := context.Background()
	e := pruneEnv(t)
	w := newRegWorld(t, batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})
	e.Kube = w.k
	e.Digests = func(context.Context, string, string) ([]string, error) {
		return []string{"sha256:1111", "sha256:2222"}, nil
	}
	// The cache metadata lives in the same fake client as the registry.
	_ = update(ctx, w.k, ns, func(m *metadata) {
		m.Entries["sha256:bbbb"] = &Entry{Version: "4.21.0", Image: "quay.io/x:4.21.0", Digest: "sha256:bbbb",
			Repo: "ocp/release/bbbb", SizeBytes: 3 << 30}
	})
	r := &mirrorRunner{runner: runner{out: releaseInfo}}
	e.Builder = r
	res, err := Prune(ctx, e, PruneOptions{Namespace: ns, Version: "4.21.0", Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 1 || !strings.Contains(res.Removed[0], "ocp/release/bbbb") {
		t.Errorf("removed %q", res.Removed)
	}
	// The tag goes first, then the manifests.
	if len(r.registryCalls) != 2 || r.registryCalls[0][1] != "delete" || r.registryCalls[1][1] != "delete-manifests" {
		t.Errorf("registry.sh calls = %q", r.registryCalls)
	}
	if len(r.registryCalls[1]) != 5 { // host, delete-manifests, repo, two digests
		t.Errorf("delete-manifests args = %q", r.registryCalls[1])
	}
	m, err := load(ctx, w.k, ns)
	if err != nil || m.Entries["sha256:bbbb"] != nil {
		t.Errorf("metadata still holds the pruned release: %+v", m.Entries)
	}
	if !slices.Equal(w.seen, []int32{0, 1}) {
		t.Errorf("scaled %v: prune must collect the garbage", w.seen)
	}
}

func TestDeleteToleratesAGoneTag(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		exists string
		fail   bool
	}{
		{"the tag is already gone", "no", false},
		{"the tag is still there", "yes", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newRegWorld(t, batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})
			r := &mirrorRunner{runner: runner{out: releaseInfo, registryFail: "delete", exists: c.exists}}
			e := Env{Kube: w.k, Builder: r, Now: time.Now, Poll: time.Millisecond,
				Usage:   func(context.Context, string) (int64, int64, error) { return 0, 100 << 30, nil },
				Digests: func(context.Context, string, string) ([]string, error) { return []string{"sha256:1"}, nil }}
			_ = update(ctx, w.k, ns, func(m *metadata) {
				m.Entries["sha256:bbbb"] = &Entry{Version: "4.21.0", Image: "quay.io/x:4.21.0", Digest: "sha256:bbbb",
					Repo: "ocp/release/bbbb", SizeBytes: 3 << 30}
			})
			_, err := Prune(ctx, e, PruneOptions{Namespace: ns, Version: "4.21.0", Yes: true, NoGC: true})
			if c.fail != (err != nil) {
				t.Fatalf("err = %v", err)
			}
			if !c.fail && len(r.registryCalls) != 3 { // delete, exists, delete-manifests
				t.Errorf("registry.sh calls = %q", r.registryCalls)
			}
		})
	}
}

// lockRunner answers the builder's mirror-lock check.
type lockRunner struct{ held bool }

func (l lockRunner) Run(_ context.Context, _ string, _ []string, io_ builder.IO) error {
	answer := "no"
	if l.held {
		answer = "yes"
	}
	_, _ = io.WriteString(io_.Stdout, answer+"\n")
	return nil
}

func TestGarbageCollectRefusesWhileMirroring(t *testing.T) {
	w := newRegWorld(t, batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})
	e := w.env()
	e.Builder = lockRunner{held: true}
	err := GarbageCollect(context.Background(), e, ns)
	if err == nil || !strings.Contains(err.Error(), "a release mirror is running") {
		t.Fatalf("got %v", err)
	}
	if len(w.seen) != 0 {
		t.Errorf("scaled the registry while a mirror ran: %v", w.seen)
	}
}

func TestEnsureConfig(t *testing.T) {
	ctx := context.Background()
	w := newRegWorld(t, batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})
	e := w.env()
	// An environment set up before the collector existed has no ConfigMap.
	if err := e.ensureConfig(ctx, ns); err != nil {
		t.Fatal(err)
	}
	cm, err := w.k.CoreV1().ConfigMaps(ns).Get(ctx, configName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cm.Data["config.yml"], "rootdirectory: /var/lib/registry") {
		t.Errorf("config.yml = %q", cm.Data["config.yml"])
	}
	// A second run leaves it alone.
	cm.Data["config.yml"] = "custom"
	if _, err := w.k.CoreV1().ConfigMaps(ns).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := e.ensureConfig(ctx, ns); err != nil {
		t.Fatal(err)
	}
	cm, _ = w.k.CoreV1().ConfigMaps(ns).Get(ctx, configName, metav1.GetOptions{})
	if cm.Data["config.yml"] != "custom" {
		t.Errorf("overwrote the ConfigMap: %q", cm.Data["config.yml"])
	}
}

// A negative --keep is a mistake, not "keep none".
func TestPruneRefusesNegativeKeep(t *testing.T) {
	rep := &Report{Entries: []Entry{
		{Version: "4.20.46", Image: "q:4.20.46"},
		{Version: "4.21.42", Image: "q:4.21.42"},
	}}
	if _, err := selectEntries(rep, PruneOptions{Keep: -1}); err == nil || !strings.Contains(err.Error(), "--keep takes how many releases to keep, not -1") {
		t.Errorf("err = %v, want a refusal", err)
	}
}
