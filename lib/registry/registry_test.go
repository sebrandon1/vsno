package registry

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/builder"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kfake "k8s.io/client-go/kubernetes/fake"
)

const ns = "vsno-test"

var testRegistry = Registry{Host: Host(ns), User: "u", Password: "p", CA: "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----\n"}

func secrets(objs ...*corev1.Secret) *kfake.Clientset {
	k := kfake.NewClientset(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "registry-auth"},
			Data: map[string][]byte{"username": []byte("u"), "password": []byte("p")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "registry-ca"}, Data: map[string][]byte{"ca.crt": []byte(testRegistry.CA)}},
	)
	for _, o := range objs {
		_ = k.Tracker().Add(o)
	}
	return k
}

func TestLoad(t *testing.T) {
	r, err := Load(context.Background(), secrets(), ns)
	if err != nil || r != testRegistry {
		t.Fatalf("Load = %+v, %v", r, err)
	}
	if _, err := Load(context.Background(), kfake.NewClientset(), ns); err == nil || !strings.Contains(err.Error(), "rerun `vsno setup") {
		t.Errorf("missing secrets: %v", err)
	}
	empty := kfake.NewClientset(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "registry-auth"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "registry-ca"}},
	)
	if _, err := Load(context.Background(), empty, ns); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("empty secrets: %v", err)
	}
}

func TestHostAndTrustKey(t *testing.T) {
	if got, want := Host(ns), "registry.vsno-test.svc.cluster.local:5000"; got != want {
		t.Errorf("Host = %s", got)
	}
	if got, want := testRegistry.TrustKey(), "registry.vsno-test.svc.cluster.local..5000"; got != want {
		t.Errorf("TrustKey = %s", got)
	}
	if got, want := testRegistry.Auth(), DockerConfig(testRegistry.Host, b64("u:p")); got != want {
		t.Errorf("Auth = %s", got)
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestAddAuth(t *testing.T) {
	in := `{"auths":{"quay.io":{"auth":"QQ=="}},"extra":"kept"}`
	out, err := AddAuth(in, "172.30.0.5:5000", "vsno", "secret")
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatal(err)
	}
	auths := cfg["auths"].(map[string]any)
	if len(auths) != 2 || auths["quay.io"] == nil {
		t.Errorf("auths = %v", auths)
	}
	got := auths["172.30.0.5:5000"].(map[string]any)["auth"]
	if got != b64("vsno:secret") {
		t.Errorf("auth = %v", got)
	}
	if cfg["extra"] != "kept" {
		t.Errorf("dropped a top-level key: %v", cfg)
	}

	out, err = AddAuth(`{}`, "h:5000", "u", "p")
	if err != nil || !strings.Contains(out, "h:5000") {
		t.Errorf("empty config: %s, %v", out, err)
	}
	if _, err := AddAuth("not json", "h", "u", "p"); err == nil || !strings.Contains(err.Error(), "parsing the pull secret") {
		t.Errorf("bad JSON: %v", err)
	}
}

func TestAddress(t *testing.T) {
	k := kfake.NewClientset(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: Service},
		Spec:       corev1.ServiceSpec{ClusterIP: "172.30.0.5"},
	})
	got, err := Address(context.Background(), k, ns)
	if err != nil || got != "172.30.0.5:5000" {
		t.Errorf("Address = %q, %v", got, err)
	}
	if _, err := Address(context.Background(), kfake.NewClientset(), ns); err == nil || !strings.Contains(err.Error(), "rerun `vsno setup") {
		t.Errorf("missing Service: %v", err)
	}
}

// fakeRunner answers a registry script and records what it ran.
type fakeRunner struct {
	script string
	args   []string
	stdin  string
	out    string
	err    error
}

func (f *fakeRunner) Run(_ context.Context, s string, args []string, io_ builder.IO) error {
	f.script = s
	f.args = args
	if io_.Stdin != nil {
		b, _ := io.ReadAll(io_.Stdin)
		f.stdin = string(b)
	}
	if io_.Stdout != nil {
		_, _ = io.WriteString(io_.Stdout, f.out)
	}
	if f.err != nil {
		_, _ = io.WriteString(io_.Stderr, "some detail\nlast line")
	}
	return f.err
}

func TestRun(t *testing.T) {
	f := &fakeRunner{out: "one\ntwo\n"}
	out, err := testRegistry.Run(context.Background(), f, testRegistry.Host, "list", "seeds/")
	if err != nil || string(out) != "one\ntwo\n" {
		t.Fatalf("Run = %q, %v", out, err)
	}
	if f.script != Script("registry.sh") || len(f.args) != 3 || f.args[2] != "seeds/" {
		t.Errorf("script = %q, args = %q", f.script, f.args)
	}
	// The credentials go on stdin, as JSON.
	if !strings.Contains(f.stdin, `"password":"p"`) || !strings.Contains(f.stdin, "BEGIN CERTIFICATE") {
		t.Errorf("stdin = %q", f.stdin)
	}

	f = &fakeRunner{err: errors.New("exit 1")}
	if _, err := testRegistry.Run(context.Background(), f, "h", "list"); err == nil || !strings.Contains(err.Error(), "last line") {
		t.Errorf("error without the stderr tail: %v", err)
	}
}

// tarNames lists the files a Sync sent the script on stdin.
func tarNames(t *testing.T, tarred string) []string {
	t.Helper()
	var names []string
	tr := tar.NewReader(bytes.NewReader([]byte(tarred)))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil {
			t.Fatalf("reading the tar: %v", err)
		}
		names = append(names, h.Name)
	}
}

func TestSyncPush(t *testing.T) {
	const result = `{"digest":"sha256:abc","sizeBytes":1234}`
	f := &fakeRunner{out: result + "\n"}
	res, err := testRegistry.Sync(context.Background(), f, SyncOptions{
		Source: "h/seeds/a:4.22.1", Destination: "quay.io/me/seeds:4.22.1",
		AuthFile: `{"auths":{"quay.io":{"auth":"eA=="}}}`, CAFile: "PEM", LocalSource: true, Insecure: true, Overwrite: true,
	})
	if err != nil || res.Digest != "sha256:abc" || res.SizeBytes != 1234 {
		t.Fatalf("Sync = %+v, %v", res, err)
	}
	if f.script != Script("sync.sh") {
		t.Errorf("script = %q", f.script)
	}
	want := []string{"out", "h/seeds/a:4.22.1", "quay.io/me/seeds:4.22.1", "true", "true"}
	if !slices.Equal(f.args, want) {
		t.Errorf("args = %q, want %q", f.args, want)
	}
	if got, want := tarNames(t, f.stdin), []string{"auth.json", "ca.crt", "remote-auth.json", "remote-ca.crt"}; !slices.Equal(got, want) {
		t.Errorf("stdin files = %q, want %q", got, want)
	}
}

func TestSyncPullAnonymous(t *testing.T) {
	f := &fakeRunner{out: `{"digest":"sha256:abc","sizeBytes":1}`}
	if _, err := testRegistry.Sync(context.Background(), f, SyncOptions{Source: "quay.io/me/seeds:1", Destination: "h/seeds/a:1"}); err != nil {
		t.Fatal(err)
	}
	if f.args[0] != "in" || f.args[3] != "false" || f.args[4] != "false" {
		t.Errorf("args = %q", f.args)
	}
	if got := tarNames(t, f.stdin); slices.Contains(got, "remote-auth.json") {
		t.Errorf("stdin files = %q, want no remote credentials", got)
	}
}

func TestSyncErrors(t *testing.T) {
	for _, c := range []struct {
		name string
		out  string
		err  error
		want string
	}{
		{"script failure", "", errors.New("exit 1"), "last line"},
		{"unreadable result", "not json\n", nil, "reading the copy's result"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeRunner{out: c.out, err: c.err}
			if _, err := testRegistry.Sync(context.Background(), f, SyncOptions{Source: "s:1", Destination: "d:1"}); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want %q", err, c.want)
			}
		})
	}
}
