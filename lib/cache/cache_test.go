package cache

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/registry"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const ns = "vsno-test"

// releaseInfo is `oc adm release info -o json` as the builder prints it.
const releaseInfo = `{"digest":"sha256:1111111111111111aaaa",
 "references":{"spec":{"tags":[
  {"from":{"name":"quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:aaa"}},
  {"from":{"name":"quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:ccc"}}
 ]}}}`

// runner answers release-info.sh and registry.sh.
type runner struct {
	out   string
	err   error
	ran   []string
	stdin string
	// registryCalls are registry.sh's arguments, in order.
	registryCalls [][]string
	// registryFail makes the named registry.sh operation fail.
	registryFail string
	// exists answers registry.sh exists.
	exists string
	// size answers registry.sh size; "" prints nothing, which fails to parse.
	size string
}

func (r *runner) Run(_ context.Context, s string, args []string, io_ builder.IO) error {
	if s == registry.Script("registry.sh") {
		r.registryCalls = append(r.registryCalls, args)
		if len(args) > 1 && args[1] == "exists" {
			_, _ = io.WriteString(io_.Stdout, r.exists+"\n")
			return nil
		}
		if len(args) > 1 && args[1] == r.registryFail {
			return errors.New("exit 1")
		}
		if len(args) > 1 && args[1] == "size" && r.size != "" {
			_, _ = io.WriteString(io_.Stdout, r.size+"\n")
		}
		return nil
	}
	if s != script("release-info.sh") {
		return errors.New("unexpected script")
	}
	r.ran = args
	if io_.Stdin != nil {
		b, _ := io.ReadAll(io_.Stdin)
		r.stdin = string(b)
	}
	if io_.Stdout != nil {
		_, _ = io.WriteString(io_.Stdout, r.out)
	}
	if r.err != nil {
		_, _ = io.WriteString(io_.Stderr, "some detail\nlast line")
	}
	return r.err
}

func env(r *runner) Env {
	k := kfake.NewClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "openshift-config", Name: "pull-secret"},
		Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"quay.io":{}}}`)}})
	return Env{Kube: k, Builder: r, Now: func() time.Time { return time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC) }}
}

func TestDescribe(t *testing.T) {
	r := &runner{out: releaseInfo}
	rel, err := describe(context.Background(), env(r), "quay.io/x:4.20.46")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Digest != "sha256:1111111111111111aaaa" {
		t.Errorf("digest = %s", rel.Digest)
	}
	// The component images, whose signatures the mirror needs too.
	if want := []string{"quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:aaa", "quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:ccc"}; !slices.Equal(rel.Images, want) {
		t.Errorf("images = %v, want %v", rel.Images, want)
	}
	sources := rel.Sources
	// A release's references name only its payload's repository; Ensure adds
	// the release image's own.
	want := []string{"quay.io/openshift-release-dev/ocp-v4.0-art-dev"}
	if !slices.Equal(sources, want) {
		t.Errorf("sources = %v, want %v", sources, want)
	}
	if r.stdin != `{"auths":{"quay.io":{}}}` {
		t.Errorf("stdin = %q", r.stdin)
	}
}

func TestDescribeErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := describe(ctx, env(&runner{err: errors.New("exit 1")}), "x"); err == nil || !strings.Contains(err.Error(), "last line") {
		t.Errorf("got %v", err)
	}
	if _, err := describe(ctx, env(&runner{out: "not json"}), "x"); err == nil || !strings.Contains(err.Error(), "reading release") {
		t.Errorf("got %v", err)
	}
	if _, err := describe(ctx, env(&runner{out: `{}`}), "x"); err == nil || !strings.Contains(err.Error(), "no digest") {
		t.Errorf("got %v", err)
	}
	noPull := Env{Kube: kfake.NewClientset(), Builder: &runner{out: releaseInfo}}
	if _, err := describe(ctx, noPull, "x"); err == nil || !strings.Contains(err.Error(), "pull secret") {
		t.Errorf("got %v", err)
	}
}

func TestRepoOf(t *testing.T) {
	for _, c := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"quay.io/a/b-c@sha256:aa", "quay.io/a/b-c", true},
		{"quay.io/a/b-c:tag", "quay.io/a/b-c", true},
		// A CI nightly's repositories shadow both the release and its images.
		{"registry.ci.openshift.org/ocp/4.20-art-artefacts@sha256:aa", "registry.ci.openshift.org/ocp/4.20-art-artefacts", true}, //nolint:misspell
		{"", "", false},
		{"no-slash", "", false},
	} {
		got, ok := repoOf(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("repoOf(%q) = %q, %v", c.in, got, ok)
		}
	}
}

func TestShort(t *testing.T) {
	if got := short("sha256:1111111111111111aaaa"); got != "1111111111111111" {
		t.Errorf("short = %q", got)
	}
}

func TestUpdateAndLoad(t *testing.T) {
	ctx := context.Background()
	e := env(&runner{})
	at := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	err := update(ctx, e.Kube, ns, func(m *metadata) {
		m.Entries["sha256:1"] = &Entry{Version: "4.20.46", Image: "quay.io/x:4.20.46", Digest: "sha256:1", Mirrored: at, LastUsed: at}
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := load(ctx, e.Kube, ns)
	if err != nil || m.Entries["sha256:1"].Version != "4.20.46" {
		t.Fatalf("load = %+v, %v", m, err)
	}
	// A second update keeps the first entry: it rewrites the same ConfigMap.
	if err := update(ctx, e.Kube, ns, func(m *metadata) { m.Entries["sha256:2"] = &Entry{Version: "4.21.0"} }); err != nil {
		t.Fatal(err)
	}
	m, _ = load(ctx, e.Kube, ns)
	if len(m.Entries) != 2 {
		t.Errorf("entries = %d", len(m.Entries))
	}
	// A conflict retries and succeeds.
	conflicted := kfake.NewClientset()
	conflicted.PrependReactor("update", "configmaps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(corev1.Resource("configmaps"), ConfigMap, errors.New("changed"))
	})
	conflicted.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: ConfigMap}, Data: map[string]string{key: `{"entries":{}}`}}
		return true, cm, nil
	})
	ce := Env{Kube: conflicted, Builder: &runner{}}
	if err := update(ctx, ce.Kube, "other", func(*metadata) {}); err == nil {
		t.Error("a persistent conflict should fail")
	}
}

func TestPending(t *testing.T) {
	ctx := context.Background()
	e := env(&runner{})
	if got, err := Pending(ctx, e.Kube, ns); err != nil || len(got) != 0 {
		t.Fatalf("empty cache: %v %v", got, err)
	}
	err := update(ctx, e.Kube, ns, func(m *metadata) {
		m.Entries["sha256:ready"] = &Entry{Version: "4.20.46", Digest: "sha256:ready"}
		m.Entries["sha256:running"] = &Entry{Version: "4.21.0", Digest: "sha256:running", State: StateMirroring}
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Pending(ctx, e.Kube, ns)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Digest != "sha256:running" {
		t.Errorf("pending = %+v", got)
	}
}

func TestList(t *testing.T) {
	ctx := context.Background()
	e := env(&runner{})
	at := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	_ = update(ctx, e.Kube, ns, func(m *metadata) {
		m.Entries["sha256:b"] = &Entry{Version: "4.21.0", Image: "quay.io/x:4.21.0", Digest: "sha256:b", Mirrored: at}
		m.Entries["sha256:a"] = &Entry{Version: "4.20.46", Image: "quay.io/x:4.20.46", Digest: "sha256:a", Mirrored: at}
	})
	rep, err := List(ctx, e, ns, map[string][]string{"quay.io/x:4.20.46": {"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Entries) != 2 || rep.Entries[0].Version != "4.20.46" || rep.Entries[1].Version != "4.21.0" {
		t.Fatalf("entries = %+v", rep.Entries)
	}
	if !rep.Entries[0].InUse || !slices.Equal(rep.Entries[0].Users, []string{"a", "b"}) {
		t.Errorf("first = %+v", rep.Entries[0])
	}
	if rep.Entries[1].InUse {
		t.Error("4.21.0 has no vSNO")
	}
	// No registry pod: the volume's numbers stay zero rather than failing.
	if rep.Used != 0 || rep.Total != 0 {
		t.Errorf("usage = %d/%d", rep.Used, rep.Total)
	}
}

func TestListEmpty(t *testing.T) {
	rep, err := List(context.Background(), env(&runner{}), ns, nil)
	if err != nil || len(rep.Entries) != 0 {
		t.Errorf("report = %+v, %v", rep, err)
	}
}

// A release its channel has not published yet is named by its pullspec; this is
// how `vsno run` turns that back into a version it can compare.
func TestVersion(t *testing.T) {
	const info = `{"digest":"sha256:1111111111111111aaaa","metadata":{"version":"4.22.23"}}`
	r := &runner{out: info}
	got, err := Version(context.Background(), env(r), "quay.io/x:4.22.23-x86_64")
	if err != nil || got != "4.22.23" {
		t.Fatalf("Version = %q, %v", got, err)
	}
	if len(r.ran) != 1 || r.ran[0] != "quay.io/x:4.22.23-x86_64" {
		t.Errorf("ran %q", r.ran)
	}

	r = &runner{out: `{"digest":"sha256:1111111111111111aaaa"}`}
	if _, err := Version(context.Background(), env(r), "quay.io/x:1"); err == nil || !strings.Contains(err.Error(), "names no version") {
		t.Errorf("no version: %v", err)
	}
	r = &runner{err: errors.New("exit 1")}
	if _, err := Version(context.Background(), env(r), "quay.io/x:1"); err == nil || !strings.Contains(err.Error(), "last line") {
		t.Errorf("failure: %v", err)
	}
}
