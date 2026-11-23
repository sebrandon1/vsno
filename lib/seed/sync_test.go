package seed

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/registry"
	"github.com/sebrandon1/vsno/lib/state"
	kfake "k8s.io/client-go/kubernetes/fake"
)

// syncRunner answers registry.sh list with a listing and sync.sh with a result,
// recording the copy it was asked to make.
type syncRunner struct {
	listing string
	out     string
	copyErr error
	listErr error

	args  []string
	stdin []byte
}

func (r *syncRunner) Run(_ context.Context, script string, args []string, s builder.IO) error {
	switch script {
	case registry.Script("registry.sh"):
		if r.listErr != nil {
			return r.listErr
		}
		_, _ = io.WriteString(s.Stdout, r.listing)
		return nil
	case registry.Script("sync.sh"):
		r.args = args
		if s.Stdin != nil {
			r.stdin, _ = io.ReadAll(s.Stdin)
		}
		_, _ = io.WriteString(s.Stdout, r.out)
		if r.copyErr != nil {
			_, _ = io.WriteString(s.Stderr, "already exists; pass --overwrite to replace it")
		}
		return r.copyErr
	}
	return errors.New("unexpected script")
}

func syncEnv(r builder.Runner) Env {
	return Env{Env: guest.Env{Kube: registrySecrets(), Builder: r}}
}

// tarFiles lists the files a copied image's credentials were packed as.
func tarFiles(t *testing.T, tarred []byte) []string {
	t.Helper()
	var names []string
	tr := tar.NewReader(bytes.NewReader(tarred))
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

// authFile writes a container auth file holding credentials for host.
func authFile(t *testing.T, host string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(p, []byte(fakeAuth(host, "u:p")), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// caFile is a peer registry's CA certificate, as --ca takes it.
func caFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(p, []byte(testRegistry.CA), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSplitSeed(t *testing.T) {
	for _, c := range []struct {
		in, name, version string
		needVersion       bool
		wantErr           string
	}{
		{in: "a", name: "a"},
		{in: "a:4.22.0", name: "a", version: "4.22.0"},
		{in: "seeds/a:4.22.0", name: "a", version: "4.22.0"},
		{in: "reg.example.com:5000/a:4.22.0", name: "reg.example.com:5000/a", version: "4.22.0"},
		{in: "a", needVersion: true, wantErr: "name the version"},
		{in: "", wantErr: "name the seed as"},
		{in: "seeds/", wantErr: "name the seed as"},
	} {
		name, version, err := splitSeed(c.in, c.needVersion)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("splitSeed(%q) error = %v, want %q", c.in, err, c.wantErr)
			}
			continue
		}
		if err != nil || name != c.name || version != c.version {
			t.Errorf("splitSeed(%q) = %q, %q, %v", c.in, name, version, err)
		}
	}
}

func TestSyncNeedsOneDirection(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		o    SyncOptions
		want string
	}{
		{SyncOptions{Namespace: ns, Seed: "a:4.20.46"}, "pick a direction"},
		{SyncOptions{Namespace: ns, Seed: "a:4.20.46", To: "quay.io/me/seeds", From: "quay.io/me/seeds:4.20.46"}, "different directions"},
	} {
		if _, err := Sync(ctx, syncEnv(&syncRunner{}), c.o); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Sync(%+v) error = %v, want %q", c.o, err, c.want)
		}
	}
}

func TestSyncPush(t *testing.T) {
	r := &syncRunner{listing: listing, out: `{"digest":"sha256:dd","sizeBytes":2147483648}`}
	res, err := Sync(context.Background(), syncEnv(r), SyncOptions{
		Namespace: ns, Seed: "a:4.20.46", To: "quay.io/me/seeds/", AuthFile: authFile(t, "quay.io"), CAPath: caFile(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The tag comes from the registry's listing, and the trailing slash on the
	// repository does not double up with it.
	want := []string{"out", testRegistry.Host + "/seeds/a:4.20.46", "quay.io/me/seeds:4.20.46", "false", "false"}
	if !slices.Equal(r.args, want) {
		t.Errorf("args = %q, want %q", r.args, want)
	}
	if res.Name != "a" || res.Version != "4.20.46" || res.Digest != "sha256:dd" || res.SizeBytes != 2147483648 {
		t.Errorf("result = %+v", res)
	}
	files := tarFiles(t, r.stdin)
	for _, want := range []string{"remote-auth.json", "remote-ca.crt"} {
		if !slices.Contains(files, want) {
			t.Errorf("stdin files = %q, want %s", files, want)
		}
	}
}

func TestSyncPushByName(t *testing.T) {
	r := &syncRunner{listing: listing, out: `{"digest":"sha256:dd","sizeBytes":1}`}
	if _, err := Sync(context.Background(), syncEnv(r), SyncOptions{
		Namespace: ns, Seed: "a", To: "quay.io/me/seeds", AuthFile: authFile(t, "quay.io"),
	}); err != nil {
		t.Fatal(err)
	}
	if r.args[2] != "quay.io/me/seeds:4.20.46" {
		t.Errorf("destination = %q", r.args[2])
	}
}

// An image without LCA's version label still syncs; the version is the one the
// argument named.
func TestSyncPushWithoutVersionLabel(t *testing.T) {
	const plain = `{"repo":"seeds/plain","tag":"4.22.0","digest":"sha256:pp","created":"2026-11-19T10:00:00Z","labels":{},"size":1}
`
	r := &syncRunner{listing: plain, out: `{"digest":"sha256:pp","sizeBytes":1}`}
	res, err := Sync(context.Background(), syncEnv(r), SyncOptions{
		Namespace: ns, Seed: "plain:4.22.0", To: "quay.io/me/seeds", AuthFile: authFile(t, "quay.io"),
	})
	if err != nil || res.Version != "4.22.0" || res.Destination != "quay.io/me/seeds:4.22.0" {
		t.Errorf("result = %+v, %v", res, err)
	}
}

func TestSyncPushAmbiguousName(t *testing.T) {
	two := `{"repo":"seeds/b","tag":"4.22.0","digest":"sha256:bb","created":"2026-11-19T10:00:00Z","labels":{},"size":1}
{"repo":"seeds/b","tag":"4.22.1","digest":"sha256:bc","created":"2026-11-19T10:00:00Z","labels":{},"size":1}
`
	r := &syncRunner{listing: two}
	_, err := Sync(context.Background(), syncEnv(r), SyncOptions{
		Namespace: ns, Seed: "b", To: "quay.io/me/seeds", AuthFile: authFile(t, "quay.io"),
	})
	if err == nil || !strings.Contains(err.Error(), "names 2 seed images") {
		t.Errorf("got %v", err)
	}
}

func TestSyncPushUnknownSeed(t *testing.T) {
	r := &syncRunner{listing: listing}
	_, err := Sync(context.Background(), syncEnv(r), SyncOptions{
		Namespace: ns, Seed: "zzz:4.20.46", To: "quay.io/me/seeds", AuthFile: authFile(t, "quay.io"),
	})
	if err == nil || !strings.Contains(err.Error(), "no seed image zzz:4.20.46") {
		t.Errorf("got %v", err)
	}
}

func TestSyncPull(t *testing.T) {
	r := &syncRunner{out: `{"digest":"sha256:ee","sizeBytes":5}`}
	res, err := Sync(context.Background(), syncEnv(r), SyncOptions{
		Namespace: ns, Seed: "copy:4.22.0", From: "quay.io/me/seeds:4.22.0", AuthFile: authFile(t, "quay.io"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"in", "quay.io/me/seeds:4.22.0", testRegistry.Host + "/seeds/copy:4.22.0", "false", "false"}; !slices.Equal(r.args, want) {
		t.Errorf("args = %q, want %q", r.args, want)
	}
	if res.Source != "quay.io/me/seeds:4.22.0" || res.Destination != testRegistry.Host+"/seeds/copy:4.22.0" || res.Version != "4.22.0" {
		t.Errorf("result = %+v", res)
	}
	// A pull never lists the registry: the image is not there yet.
	if r.listing != "" {
		t.Error("the pull listed the registry")
	}
}

func TestSyncRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("from this registry", func(t *testing.T) {
		_, err := Sync(ctx, syncEnv(&syncRunner{}), SyncOptions{
			Namespace: ns, Seed: "a:4.20.46", From: testRegistry.Host + "/seeds/a:4.20.46",
		})
		if err == nil || !strings.Contains(err.Error(), "somewhere other than") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("insecure against this registry", func(t *testing.T) {
		r := &syncRunner{listing: listing}
		_, err := Sync(ctx, syncEnv(r), SyncOptions{
			Namespace: ns, Seed: "a:4.20.46", To: testRegistry.Host + "/tmp", Insecure: true, AuthFile: authFile(t, testRegistry.Host),
		})
		if err == nil || !strings.Contains(err.Error(), "would weaken") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("no registry secrets", func(t *testing.T) {
		env := Env{Env: guest.Env{Kube: kfake.NewClientset(), Builder: &syncRunner{}}}
		_, err := Sync(ctx, env, SyncOptions{Namespace: ns, Seed: "a:4.20.46", To: "quay.io/me/seeds"})
		if err == nil || !strings.Contains(err.Error(), "rerun `vsno setup") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("listing fails", func(t *testing.T) {
		r := &syncRunner{listErr: errors.New("exit 1")}
		_, err := Sync(ctx, syncEnv(r), SyncOptions{
			Namespace: ns, Seed: "a:4.20.46", To: "quay.io/me/seeds", AuthFile: authFile(t, "quay.io"),
		})
		if err == nil || !strings.Contains(err.Error(), "listing the registry through the builder") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("missing --ca file", func(t *testing.T) {
		r := &syncRunner{listing: listing}
		_, err := Sync(ctx, syncEnv(r), SyncOptions{
			Namespace: ns, Seed: "a:4.20.46", To: "quay.io/me/seeds",
			AuthFile: authFile(t, "quay.io"), CAPath: filepath.Join(t.TempDir(), "absent.crt"),
		})
		if err == nil || !strings.Contains(err.Error(), "reading the CA") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("copy failure keeps the refs", func(t *testing.T) {
		r := &syncRunner{listing: listing, copyErr: errors.New("exit 1")}
		res, err := Sync(ctx, syncEnv(r), SyncOptions{
			Namespace: ns, Seed: "a:4.20.46", To: "quay.io/me/seeds", AuthFile: authFile(t, "quay.io"),
		})
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("got %v", err)
		}
		if res == nil || res.Source != testRegistry.Host+"/seeds/a:4.20.46" {
			t.Errorf("result = %+v", res)
		}
	})
}

func TestFarAuth(t *testing.T) {
	const ref = "quay.io/me/seeds:4.22.0"
	if _, err := farAuth("", ref, true, nil); err == nil || !strings.Contains(err.Error(), "no credentials for quay.io") {
		t.Errorf("push without credentials: %v", err)
	}
	if got, err := farAuth("", ref, false, nil); err != nil || got != "" {
		t.Errorf("anonymous pull = %q, %v", got, err)
	}

	p := authFile(t, "quay.io")
	got, err := farAuth("", ref, true, []string{filepath.Join(t.TempDir(), "absent.json"), p})
	if err != nil || !strings.Contains(got, "quay.io") {
		t.Errorf("from the auth files = %q, %v", got, err)
	}
	if _, err := farAuth(filepath.Join(t.TempDir(), "absent.json"), ref, true, nil); err == nil || !strings.Contains(err.Error(), "reading the auth file") {
		t.Errorf("missing --authfile: %v", err)
	}
}

// A pull records when the image arrived: its own build time is the upstream's.
func TestSyncPullRecordsWhenItArrived(t *testing.T) {
	e := syncEnv(&syncRunner{out: `{"digest":"sha256:ee","sizeBytes":5}`})
	at := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	e.Now = func() time.Time { return at }
	res, err := Sync(context.Background(), e, SyncOptions{
		Namespace: ns, Seed: "copy:4.22.0", From: "quay.io/me/seeds:4.22.0", AuthFile: authFile(t, "quay.io"),
	})
	if err != nil {
		t.Fatal(err)
	}
	st, _, err := state.Load(context.Background(), e.Kube, ns)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := st.Synced(res.Destination)
	if !ok || !got.SyncedAt.Equal(at) || got.Digest != "sha256:ee" {
		t.Errorf("recorded %+v, ok = %v; want %s at %s", got, ok, res.Destination, at)
	}
}

// A push copies the seed out; the environment's copy is unchanged, so nothing
// is recorded.
func TestSyncPushRecordsNothing(t *testing.T) {
	e := syncEnv(&syncRunner{listing: listing, out: `{"digest":"sha256:dd","sizeBytes":1}`})
	if _, err := Sync(context.Background(), e, SyncOptions{
		Namespace: ns, Seed: "a:4.20.46", To: "quay.io/me/seeds", AuthFile: authFile(t, "quay.io"),
	}); err != nil {
		t.Fatal(err)
	}
	st, _, err := state.Load(context.Background(), e.Kube, ns)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Syncs) != 0 {
		t.Errorf("syncs = %+v, want none", st.Syncs)
	}
}
