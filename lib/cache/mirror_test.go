package cache

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/release"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kfake "k8s.io/client-go/kubernetes/fake"
)

// mirrorEnv is an environment whose registry holds 100Gi, with `free` bytes
// left, and whose builder answers both scripts.
func mirrorEnv(t *testing.T, free int64, b builder.Runner) Env {
	t.Helper()
	k := kfake.NewClientset(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "registry-auth"},
			Data: map[string][]byte{"username": []byte("u"), "password": []byte("p")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "registry-ca"}, Data: map[string][]byte{"ca.crt": []byte("CA")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "openshift-config", Name: "pull-secret"},
			Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"quay.io":{"auth":"QQ=="}}}`)}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "registry"}, Spec: corev1.ServiceSpec{ClusterIP: "172.30.0.5"}},
	)
	const total = 100 << 30
	used := total - free
	return Env{
		Kube: k, Builder: b, Now: func() time.Time { return time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC) },
		Usage: func(context.Context, string) (int64, int64, error) { return used, total, nil },
	}
}

// mirrorRunner answers release-info.sh and mirror.sh.
type mirrorRunner struct {
	runner
	mirrorArgs  []string
	mirrorStdin []byte
	lines       []string
	mirrorErr   error
	// delay keeps the mirror running long enough for a progress line.
	delay time.Duration
}

func (m *mirrorRunner) Run(ctx context.Context, s string, args []string, io_ builder.IO) error {
	if s != script("mirror.sh") {
		return m.runner.Run(ctx, s, args, io_)
	}
	m.mirrorArgs = args
	if io_.Stdin != nil {
		b, _ := io.ReadAll(io_.Stdin)
		m.mirrorStdin = b
	}
	for _, l := range m.lines {
		_, _ = io.WriteString(io_.Stdout, l+"\n")
	}
	if m.delay > 0 {
		time.Sleep(m.delay)
	}
	if m.mirrorErr != nil {
		_, _ = io.WriteString(io_.Stderr, "some detail\nlast line")
	}
	return m.mirrorErr
}

var rel = release.Release{Requested: "4.20.46", Version: "4.20.46", Image: "quay.io/openshift-release-dev/ocp-release:4.20.46-x86_64"}

func TestEnsureMirrors(t *testing.T) {
	r := &mirrorRunner{runner: runner{out: releaseInfo}, lines: []string{"mirroring x", "done"}}
	var progress []string
	p, err := Ensure(context.Background(), mirrorEnv(t, 50<<30, r), EnsureOptions{Namespace: ns, Release: rel,
		OnLine: func(s string) { progress = append(progress, s) }})
	if err != nil {
		t.Fatal(err)
	}
	if p.Hit {
		t.Error("first mirror reported a hit")
	}
	if want := []string{rel.Image, "172.30.0.5:5000", "ocp/release/1111111111111111", "1111111111111111"}; !equal(r.mirrorArgs, want) {
		t.Errorf("args = %q, want %q", r.mirrorArgs, want)
	}
	if got := tarFiles(t, r.mirrorStdin); got["ca.crt"] != "CA" || !strings.Contains(got["auth.json"], "172.30.0.5:5000") {
		t.Errorf("stdin files = %v", got)
	}
	if !strings.Contains(p.PullSecret, "172.30.0.5:5000") || !strings.Contains(p.PullSecret, "quay.io") {
		t.Errorf("pull secret = %s", p.PullSecret)
	}
	// The payload's repository is mapped; the release image's own is not, so the
	// kubelet can keep fetching its signature from the source registry.
	if len(p.Sources) != 1 || p.Sources[0].Source != "quay.io/openshift-release-dev/ocp-v4.0-art-dev" ||
		p.Sources[0].Mirrors[0] != "172.30.0.5:5000/ocp/release/1111111111111111" {
		t.Errorf("sources = %+v", p.Sources)
	}
	if p.TrustBundle != "CA" {
		t.Errorf("trust bundle = %q", p.TrustBundle)
	}
	if len(progress) != 2 { // the mirror's two lines; no room warning at 50Gi free
		t.Errorf("progress = %q", progress)
	}
}

func TestMirrorReportsThroughput(t *testing.T) {
	r := &mirrorRunner{runner: runner{out: releaseInfo}, delay: 50 * time.Millisecond}
	e := mirrorEnv(t, 50<<30, r)
	// The progress goroutine and the mirror share the clock, so it advances
	// through an atomic counter rather than a captured variable.
	clockStart := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	var ticks atomic.Int64
	e.Now = func() time.Time { return clockStart.Add(time.Duration(ticks.Add(1)) * 100 * time.Millisecond) }
	e.Progress = time.Millisecond
	base := int64(1 << 30)
	var calls atomic.Int32
	e.Usage = func(context.Context, string) (int64, int64, error) {
		return base + int64(calls.Add(1))*(1<<20), 100 << 30, nil
	}
	var mu sync.Mutex
	var lines []string
	collect := func(s string) {
		mu.Lock()
		lines = append(lines, s)
		mu.Unlock()
	}
	p, err := Ensure(context.Background(), e, EnsureOptions{Namespace: ns, Release: rel, OnLine: collect})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	var said string
	for _, l := range lines {
		if strings.Contains(l, "MiB/s") {
			said = l
		}
	}
	if said == "" || !strings.Contains(said, "mirrored ") {
		t.Errorf("no throughput line in %q", lines)
	}
	if p.Elapsed <= 0 {
		t.Errorf("elapsed = %v", p.Elapsed)
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

func TestEnsureHit(t *testing.T) {
	ctx := context.Background()
	r := &mirrorRunner{runner: runner{out: releaseInfo, exists: "yes"}}
	e := mirrorEnv(t, 50<<30, r)
	at := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	_ = update(ctx, e.Kube, ns, func(m *metadata) {
		m.Entries["sha256:1111111111111111aaaa"] = &Entry{Version: "4.20.46", Image: rel.Image,
			Digest: "sha256:1111111111111111aaaa", Repo: "ocp/release/1111111111111111", Mirrored: at, SizeBytes: 7 << 30, Requested: []string{"4.20"}}
	})
	p, err := Ensure(ctx, e, EnsureOptions{Namespace: ns, Release: rel})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Hit || len(r.mirrorArgs) != 0 {
		t.Errorf("hit = %v, mirror args %q", p.Hit, r.mirrorArgs)
	}
	if !p.Mirrored.Equal(at) || p.SizeBytes != 7<<30 {
		t.Errorf("entry = %+v, want the recorded date and size", p.Entry)
	}
	if want := []string{"4.20", "4.20.46"}; !equal(p.Requested, want) {
		t.Errorf("requested = %q, want %q", p.Requested, want)
	}
}

// The record can outlive the images (a prune that failed halfway, a manual
// delete): that is not a hit, or the guest installs from upstream believing it
// is reading the mirror.
func TestEnsureMirrorsWhenTheImagesAreGone(t *testing.T) {
	ctx := context.Background()
	r := &mirrorRunner{runner: runner{out: releaseInfo, exists: "no"}}
	e := mirrorEnv(t, 50<<30, r)
	at := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	_ = update(ctx, e.Kube, ns, func(m *metadata) {
		m.Entries["sha256:1111111111111111aaaa"] = &Entry{Version: "4.20.46", Image: rel.Image,
			Digest: "sha256:1111111111111111aaaa", Repo: "ocp/release/1111111111111111", Mirrored: at, SizeBytes: 7 << 30}
	})
	p, err := Ensure(ctx, e, EnsureOptions{Namespace: ns, Release: rel})
	if err != nil {
		t.Fatal(err)
	}
	if p.Hit || len(r.mirrorArgs) == 0 {
		t.Errorf("hit = %v, mirror args %q", p.Hit, r.mirrorArgs)
	}
	if len(r.registryCalls) == 0 || r.registryCalls[0][1] != "exists" {
		t.Errorf("registry calls = %q", r.registryCalls)
	}
}

// An entry that is mid-mirror is not usable yet: another create must wait its
// turn (the script's lock), then mirror, not install from a partial payload.
func TestEnsureMirrorsAnEntryThatIsNotReady(t *testing.T) {
	ctx := context.Background()
	r := &mirrorRunner{runner: runner{out: releaseInfo}}
	e := mirrorEnv(t, 50<<30, r)
	_ = update(ctx, e.Kube, ns, func(m *metadata) {
		m.Entries["sha256:1111111111111111aaaa"] = &Entry{Version: "4.20.46", Image: rel.Image,
			Digest: "sha256:1111111111111111aaaa", Repo: "ocp/release/1111111111111111", State: StateMirroring}
	})
	p, err := Ensure(ctx, e, EnsureOptions{Namespace: ns, Release: rel})
	if err != nil {
		t.Fatal(err)
	}
	if p.Hit || len(r.mirrorArgs) == 0 {
		t.Errorf("hit = %v, mirror args %q", p.Hit, r.mirrorArgs)
	}
	if p.State != "" {
		t.Errorf("state = %q, want ready once the mirror finished", p.State)
	}
}

func TestEnsureRefusesWhenFull(t *testing.T) {
	r := &mirrorRunner{runner: runner{out: releaseInfo}}
	_, err := Ensure(context.Background(), mirrorEnv(t, 2<<30, r), EnsureOptions{Namespace: ns, Release: rel})
	if err == nil || !strings.Contains(err.Error(), "has 2.0Gi free") || !strings.Contains(err.Error(), "cache prune") {
		t.Errorf("got %v", err)
	}
	if len(r.mirrorArgs) != 0 {
		t.Error("mirrored into a full volume")
	}
}

func TestEnsureWarnsWhenNearlyFull(t *testing.T) {
	r := &mirrorRunner{runner: runner{out: releaseInfo}}
	var progress []string
	// 22Gi free: enough for a 20Gi estimate, but under 25Gi.
	if _, err := Ensure(context.Background(), mirrorEnv(t, 22<<30, r), EnsureOptions{Namespace: ns, Release: rel,
		OnLine: func(s string) { progress = append(progress, s) }}); err != nil {
		t.Fatal(err)
	}
	if len(progress) == 0 || !strings.Contains(progress[0], "needs about") {
		t.Errorf("progress = %q", progress)
	}
}

func TestEnsureErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("mirror fails", func(t *testing.T) {
		r := &mirrorRunner{runner: runner{out: releaseInfo}, mirrorErr: errors.New("exit 1")}
		e := mirrorEnv(t, 50<<30, r)
		_, err := Ensure(ctx, e, EnsureOptions{Namespace: ns, Release: rel})
		if err == nil || !strings.Contains(err.Error(), "last line") {
			t.Errorf("got %v", err)
		}
		m, err := load(ctx, e.Kube, ns)
		if err != nil {
			t.Fatal(err)
		}
		// Said out loud, so `cache list` shows it and the next create retries.
		if ent := m.Entries["sha256:1111111111111111aaaa"]; ent == nil || ent.State != StateFailed {
			t.Errorf("entry = %+v, want a failed one", ent)
		}
	})
	t.Run("no sources", func(t *testing.T) {
		r := &mirrorRunner{runner: runner{out: `{"digest":"sha256:1"}`}}
		if _, err := Ensure(ctx, mirrorEnv(t, 50<<30, r), EnsureOptions{Namespace: ns, Release: rel}); err == nil || !strings.Contains(err.Error(), "no images") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("no registry", func(t *testing.T) {
		e := mirrorEnv(t, 50<<30, &mirrorRunner{runner: runner{out: releaseInfo}})
		e.Kube = kfake.NewClientset()
		if _, err := Ensure(ctx, e, EnsureOptions{Namespace: ns, Release: rel}); err == nil || !strings.Contains(err.Error(), "rerun `vsno setup") {
			t.Errorf("got %v", err)
		}
	})
}

// tarFiles reads a tar of files.
func tarFiles(t *testing.T, b []byte) map[string]string {
	t.Helper()
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

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A second mirror of a release finds its blobs already stored, so the volume
// barely grows; the size comes from the blobs the repository references.
func TestEnsureRecordsTheBlobSize(t *testing.T) {
	ctx := context.Background()
	r := &mirrorRunner{runner: runner{out: releaseInfo, size: "23371317248"}}
	e := mirrorEnv(t, 50<<30, r) // the volume never grows
	_ = update(ctx, e.Kube, ns, func(m *metadata) {
		m.Entries["sha256:1111111111111111aaaa"] = &Entry{Version: "4.20.46", Image: rel.Image,
			Digest: "sha256:1111111111111111aaaa", Repo: "ocp/release/1111111111111111", State: StateFailed}
	})
	p, err := Ensure(ctx, e, EnsureOptions{Namespace: ns, Release: rel})
	if err != nil {
		t.Fatal(err)
	}
	if p.Hit || p.SizeBytes != 23371317248 || !p.Measured {
		t.Errorf("hit = %v, size = %d, measured = %v", p.Hit, p.SizeBytes, p.Measured)
	}
	if last := r.registryCalls[len(r.registryCalls)-1]; !equal(last[1:], []string{"size", "ocp/release/1111111111111111"}) {
		t.Errorf("last registry call = %q", last)
	}
	m, err := load(ctx, e.Kube, ns)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Entries["sha256:1111111111111111aaaa"]; got.SizeBytes != 23371317248 || !got.Measured {
		t.Errorf("recorded = %+v", got)
	}
}

// When the registry can't say, the volume's growth is the best there is, and
// the entry stays unmeasured so its next use measures it.
func TestEnsureFallsBackToVolumeGrowth(t *testing.T) {
	r := &mirrorRunner{runner: runner{out: releaseInfo, registryFail: "size"}}
	e := mirrorEnv(t, 50<<30, r)
	var calls atomic.Int64
	e.Usage = func(context.Context, string) (int64, int64, error) {
		if calls.Add(1) == 1 {
			return 50 << 30, 100 << 30, nil
		}
		return 70 << 30, 100 << 30, nil
	}
	p, err := Ensure(context.Background(), e, EnsureOptions{Namespace: ns, Release: rel})
	if err != nil {
		t.Fatal(err)
	}
	if p.SizeBytes != 20<<30 || p.Measured {
		t.Errorf("size = %d, measured = %v; want the volume's 20Gi growth, unmeasured", p.SizeBytes, p.Measured)
	}
}

// A hit on an entry recorded the old way is measured once; a measured one is
// not asked again, so a cached create pays for it only once.
func TestEnsureHitMeasuresAnOldEntry(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name     string
		measured bool
		want     int64
	}{
		{"recorded from the volume", false, 23371317248},
		{"already measured", true, 51 << 20},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := &mirrorRunner{runner: runner{out: releaseInfo, exists: "yes", size: "23371317248"}}
			e := mirrorEnv(t, 50<<30, r)
			_ = update(ctx, e.Kube, ns, func(m *metadata) {
				m.Entries["sha256:1111111111111111aaaa"] = &Entry{Version: "4.20.46", Image: rel.Image,
					Digest: "sha256:1111111111111111aaaa", Repo: "ocp/release/1111111111111111", SizeBytes: 51 << 20, Measured: c.measured}
			})
			p, err := Ensure(ctx, e, EnsureOptions{Namespace: ns, Release: rel})
			if err != nil {
				t.Fatal(err)
			}
			if !p.Hit || p.SizeBytes != c.want || !p.Measured {
				t.Errorf("hit = %v, size = %d, measured = %v; want %d", p.Hit, p.SizeBytes, p.Measured, c.want)
			}
			asked := false
			for _, call := range r.registryCalls {
				asked = asked || (len(call) > 1 && call[1] == "size")
			}
			if asked == c.measured {
				t.Errorf("asked for the size = %v with measured = %v", asked, c.measured)
			}
		})
	}
}
