package seed

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/guest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kfake "k8s.io/client-go/kubernetes/fake"
)

const ns = "vsno-test"

var update = flag.Bool("update", false, "rewrite the golden files")

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./lib/seed -update)", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from the golden file (go test ./lib/seed -update):\n%s", name, got)
	}
}

// resp is one canned answer.
type resp struct {
	out string
	err error
}

// fakeAccess answers oc and ssh by their arguments (without "-o json"); each
// key's answers are used in turn, the last one repeating. Unknown keys answer
// nothing, successfully.
type fakeAccess struct {
	oc, ssh map[string][]resp
	calls   []string
	stdin   []string
}

func (f *fakeAccess) answer(m map[string][]resp, kind string, args []string, s builder.IO) error {
	key := strings.TrimSuffix(strings.Join(args, " "), " -o json")
	f.calls = append(f.calls, kind+" "+key)
	if s.Stdin != nil {
		b, _ := io.ReadAll(s.Stdin)
		f.stdin = append(f.stdin, string(b))
	}
	q := m[key]
	if len(q) == 0 {
		return nil
	}
	r := q[0]
	if len(q) > 1 {
		m[key] = q[1:]
	}
	if s.Stdout != nil {
		_, _ = io.WriteString(s.Stdout, r.out)
	}
	return r.err
}

func (f *fakeAccess) Oc(_ context.Context, args []string, s builder.IO) error {
	return f.answer(f.oc, "oc", args, s)
}

func (f *fakeAccess) SSH(_ context.Context, args []string, s builder.IO) error {
	return f.answer(f.ssh, "ssh", args, s)
}

func (f *fakeAccess) ran(prefix string) bool {
	return slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

func testEnv(fa *fakeAccess) Env {
	e := Env{Env: guest.Env{Poll: time.Millisecond}, Access: func(string) Access { return fa }}
	e.defaults(ns)
	return e
}

// clock advances a minute every time it's read, so polls time out quickly.
func clock() func() time.Time {
	t := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	return func() time.Time { t = t.Add(time.Minute); return t }
}

// testRegistry's credentials are deliberately trivial: their base64 must not
// look like a credential to secret scanners, including in the goldens.
var testRegistry = Registry{Host: RegistryHost(ns), User: "u", Password: "p", CA: "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----\n"}

func TestTemplates(t *testing.T) {
	for name, d := range map[string]renderData{
		"lca.yaml.tmpl":     {Channel: "stable", Source: "redhat-operators", SourceNamespace: "openshift-marketplace"},
		"trust.yaml.tmpl":   {TrustKey: testRegistry.TrustKey(), CA: testRegistry.CA},
		"seedgen.yaml.tmpl": {SeedAuth: testRegistry.Auth(), Image: Ref(testRegistry, "brave-otter", "4.20.46")},
	} {
		y, err := render(name, d)
		if err != nil {
			t.Fatal(err)
		}
		golden(t, strings.TrimSuffix(name, ".tmpl")+".golden", y)
	}
}

func TestRegistryNames(t *testing.T) {
	if got, want := Ref(testRegistry, "a", "4.20.46"), "registry.vsno-test.svc.cluster.local:5000/seeds/a:4.20.46"; got != want {
		t.Errorf("Ref = %s, want %s", got, want)
	}
	if got, want := testRegistry.TrustKey(), "registry.vsno-test.svc.cluster.local..5000"; got != want {
		t.Errorf("TrustKey = %s, want %s", got, want)
	}
	if got, want := testRegistry.Auth(), fakeAuth(testRegistry.Host, "u:p"); got != want {
		t.Errorf("Auth = %s, want %s", got, want)
	}
}

// fakeAuth builds a dockerconfigjson for host, so the tests hold no literal
// base64 credential (which secret scanners flag).
func fakeAuth(host, userPass string) string {
	auth := base64.StdEncoding.EncodeToString([]byte(userPass))
	return fmt.Sprintf(`{"auths":{"%s":{"%s":"%s"}}}`, host, "auth", auth)
}

func registrySecrets(objs ...*corev1.Secret) *kfake.Clientset {
	k := kfake.NewClientset(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "registry-auth"}, Data: map[string][]byte{"username": []byte("u"), "password": []byte("p")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "registry-ca"}, Data: map[string][]byte{"ca.crt": []byte(testRegistry.CA)}},
	)
	for _, o := range objs {
		_ = k.Tracker().Add(o)
	}
	return k
}

func TestLoadRegistry(t *testing.T) {
	r, err := LoadRegistry(context.Background(), registrySecrets(), ns)
	if err != nil || r != testRegistry {
		t.Fatalf("LoadRegistry = %+v, %v", r, err)
	}
	if _, err := LoadRegistry(context.Background(), kfake.NewClientset(), ns); err == nil || !strings.Contains(err.Error(), "rerun `vsno setup") {
		t.Errorf("missing secrets: %v", err)
	}
	empty := kfake.NewClientset(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "registry-auth"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "registry-ca"}},
	)
	if _, err := LoadRegistry(context.Background(), empty, ns); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("empty secrets: %v", err)
	}
}

func TestSeedAuth(t *testing.T) {
	k := registrySecrets(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "openshift-config", Name: "pull-secret"},
		Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(fakeAuth("quay.io", "quay"))}})
	ctx := context.Background()
	if got, err := seedAuth(ctx, k, testRegistry, Ref(testRegistry, "a", "4.20.46")); err != nil || got != testRegistry.Auth() {
		t.Errorf("environment registry: %s, %v", got, err)
	}
	if got, err := seedAuth(ctx, k, testRegistry, "quay.io/me/seed:1"); err != nil || got != fakeAuth("quay.io", "quay") {
		t.Errorf("quay.io: %s, %v", got, err)
	}
	if _, err := seedAuth(ctx, k, testRegistry, "example.com/seed:1"); err == nil || !strings.Contains(err.Error(), "no credentials for example.com") {
		t.Errorf("unknown registry: %v", err)
	}
}

const (
	cosStable  = `{"items":[{"metadata":{"name":"etcd"},"status":{"conditions":[{"type":"Available","status":"True"},{"type":"Progressing","status":"False"},{"type":"Degraded","status":"False"}]}}]}`
	mcpsStable = `{"items":[{"metadata":{"name":"master"},"status":{"conditions":[{"type":"Updated","status":"True"},{"type":"Updating","status":"False"},{"type":"Degraded","status":"False"}]}}]}`
)

func obj(name string, conds ...condition) object {
	var o object
	o.Metadata.Name = name
	o.Status.Conditions = conds
	return o
}

func TestUnstable(t *testing.T) {
	ok := []condition{{Type: "Available", Status: "True"}, {Type: "Progressing", Status: "False"}, {Type: "Degraded", Status: "False"}}
	updated := []condition{{Type: "Updated", Status: "True"}, {Type: "Updating", Status: "False"}}
	for _, c := range []struct {
		name      string
		cos, mcps []object
		want      []string
	}{
		{"stable", []object{obj("etcd", ok...)}, []object{obj("master", updated...)}, nil},
		{"no operators", nil, nil, []string{"no ClusterOperators reported"}},
		{"unavailable", []object{obj("etcd", condition{Type: "Available", Status: "False"})}, nil, []string{"etcd unavailable"}},
		{"degraded", []object{obj("dns", condition{Type: "Available", Status: "True"}, condition{Type: "Degraded", Status: "True"})}, nil, []string{"dns degraded"}},
		{"progressing", []object{obj("network", condition{Type: "Available", Status: "True"}, condition{Type: "Progressing", Status: "True"})}, nil, []string{"network progressing"}},
		{"pool updating", []object{obj("etcd", ok...)}, []object{obj("master", condition{Type: "Updated", Status: "False"}, condition{Type: "Updating", Status: "True"})}, []string{"MachineConfigPool master updating"}},
		{"pool degraded", []object{obj("etcd", ok...)}, []object{obj("master", condition{Type: "Degraded", Status: "True"})}, []string{"MachineConfigPool master degraded"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := unstable(objectList{Items: c.cos}, objectList{Items: c.mcps}); !slices.Equal(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestWaitStable(t *testing.T) {
	fa := &fakeAccess{oc: map[string][]resp{
		"get clusteroperators":   {{out: `{"items":[{"metadata":{"name":"etcd"},"status":{"conditions":[{"type":"Available","status":"True"},{"type":"Progressing","status":"True"}]}}]}`}, {out: cosStable}},
		"get machineconfigpools": {{out: mcpsStable}},
	}}
	e := testEnv(fa)
	if err := waitStable(context.Background(), e, fa, e.Progress.Start("x")); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.Join(fa.calls, "\n"), "get clusteroperators"); n != 2 {
		t.Errorf("polled %d times, want 2", n)
	}

	stuck := &fakeAccess{oc: map[string][]resp{"get clusteroperators": {{out: `{"items":[]}`}}}}
	e = testEnv(stuck)
	e.Now = clock()
	if err := waitStable(context.Background(), e, stuck, e.Progress.Start("x")); err == nil || !strings.Contains(err.Error(), "didn't settle") {
		t.Errorf("stuck: %v", err)
	}

	broken := &fakeAccess{oc: map[string][]resp{"get clusteroperators": {{err: errors.New("exit 1")}}}}
	e = testEnv(broken)
	if err := waitStable(context.Background(), e, broken, e.Progress.Start("x")); err == nil || !strings.Contains(err.Error(), "oc get clusteroperators") {
		t.Errorf("oc failure: %v", err)
	}
}

const (
	csvGet     = "get clusterserviceversions -n " + LCANamespace + " --ignore-not-found"
	pmGet      = "get packagemanifests lifecycle-agent -n openshift-marketplace --ignore-not-found"
	csvPending = `{"items":[{"metadata":{"name":"lifecycle-agent.v4.20.7"},"status":{"phase":"Installing"}}]}`
	csvDone    = `{"items":[{"metadata":{"name":"lifecycle-agent.v4.20.7"},"status":{"phase":"Succeeded"}}]}`
)

func TestEnsureLCA(t *testing.T) {
	ctx := context.Background()
	t.Run("installed", func(t *testing.T) {
		fa := &fakeAccess{oc: map[string][]resp{csvGet: {{out: csvDone}}}}
		e := testEnv(fa)
		name, err := ensureLCA(ctx, e, fa, e.Progress.Start("x"))
		if err != nil || name != "lifecycle-agent.v4.20.7" || fa.ran("oc apply") {
			t.Errorf("got %s, %v; calls %q", name, err, fa.calls)
		}
	})
	t.Run("installs from the catalog", func(t *testing.T) {
		fa := &fakeAccess{oc: map[string][]resp{
			csvGet: {{out: ""}, {out: csvPending}, {out: csvDone}},
			pmGet:  {{out: `{"status":{"catalogSource":"redhat-operators","catalogSourceNamespace":"openshift-marketplace","defaultChannel":"stable"}}`}},
		}}
		e := testEnv(fa)
		name, err := ensureLCA(ctx, e, fa, e.Progress.Start("x"))
		if err != nil || name != "lifecycle-agent.v4.20.7" {
			t.Fatalf("got %s, %v", name, err)
		}
		if len(fa.stdin) != 1 || !strings.Contains(fa.stdin[0], `channel: "stable"`) || !strings.Contains(fa.stdin[0], `source: "redhat-operators"`) {
			t.Errorf("applied %q", fa.stdin)
		}
	})
	t.Run("subscribed already, still installing", func(t *testing.T) {
		fa := &fakeAccess{oc: map[string][]resp{csvGet: {{out: csvPending}, {out: csvDone}}}}
		e := testEnv(fa)
		if _, err := ensureLCA(ctx, e, fa, e.Progress.Start("x")); err != nil || fa.ran("oc apply") || fa.ran("oc "+pmGet) {
			t.Errorf("%v; calls %q", err, fa.calls)
		}
	})
}

func TestEnsureLCAFailures(t *testing.T) {
	ctx := context.Background()
	t.Run("no package", func(t *testing.T) {
		fa := &fakeAccess{oc: map[string][]resp{}}
		e := testEnv(fa)
		if _, err := ensureLCA(ctx, e, fa, e.Progress.Start("x")); err == nil || !strings.Contains(err.Error(), "no lifecycle-agent package") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("never succeeds", func(t *testing.T) {
		fa := &fakeAccess{oc: map[string][]resp{csvGet: {{out: csvPending}}}}
		e := testEnv(fa)
		e.Now = clock()
		if _, err := ensureLCA(ctx, e, fa, e.Progress.Start("x")); err == nil || !strings.Contains(err.Error(), "wasn't installed") {
			t.Errorf("got %v", err)
		}
	})
}

func TestEnsureTrust(t *testing.T) {
	ctx := context.Background()
	certFile := "test -f /etc/docker/certs.d/" + testRegistry.Host + "/ca.crt"
	for _, c := range []struct {
		name, current string
		patched       bool
		err           string
	}{
		{"new", "", true, ""},
		{"already ours", trustConfigMap, false, ""},
		{"someone else's", "corp-ca", false, "already trusts CAs from ConfigMap openshift-config/corp-ca"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fa := &fakeAccess{
				oc:  map[string][]resp{"get image.config.openshift.io cluster": {{out: `{"spec":{"additionalTrustedCA":{"name":"` + c.current + `"}}}`}}},
				ssh: map[string][]resp{certFile: {{err: errors.New("exit 1")}, {}}},
			}
			e := testEnv(fa)
			err := ensureTrust(ctx, e, fa, testRegistry, e.Progress.Start("x"))
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Errorf("got %v, want %s", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := fa.ran("oc patch image.config.openshift.io cluster"); got != c.patched {
				t.Errorf("patched = %v, want %v", got, c.patched)
			}
			if !strings.Contains(fa.stdin[0], testRegistry.TrustKey()+`: "-----BEGIN CERTIFICATE-----\nCA\n`) {
				t.Errorf("ConfigMap: %s", fa.stdin[0])
			}
		})
	}
	t.Run("node never gets it", func(t *testing.T) {
		fa := &fakeAccess{oc: map[string][]resp{}, ssh: map[string][]resp{certFile: {{err: errors.New("exit 1")}}}}
		e := testEnv(fa)
		e.Now = clock()
		if err := ensureTrust(ctx, e, fa, testRegistry, e.Progress.Start("x")); err == nil || !strings.Contains(err.Error(), "doesn't have") {
			t.Errorf("got %v", err)
		}
	})
}

func TestPhaseOf(t *testing.T) {
	for _, c := range []struct {
		name  string
		conds []condition
		want  seedgenPhase
		msg   string
	}{
		{"new", nil, sgRunning, "starting"},
		{"in progress", []condition{{Type: "SeedGenInProgress", Status: "True", Reason: "InProgress", Message: "Launching imager container"}}, sgRunning, "Launching imager container"},
		{"completed", []condition{{Type: "SeedGenInProgress", Status: "False", Reason: "Completed"}, {Type: "SeedGenCompleted", Status: "True", Reason: "Completed", Message: "Seed Generation completed"}}, sgCompleted, "Seed Generation completed"},
		{"failed", []condition{{Type: "SeedGenInProgress", Status: "False", Reason: "Failed", Message: "push failed"}, {Type: "SeedGenCompleted", Status: "False", Reason: "Failed", Message: "Seed Generation failed: push failed"}}, sgFailed, "Seed Generation failed: push failed"},
		{"failed in progress only", []condition{{Type: "SeedGenInProgress", Status: "False", Reason: "Failed", Message: "precheck"}}, sgFailed, "precheck"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if p, msg := phaseOf(obj("seedimage", c.conds...)); p != c.want || msg != c.msg {
				t.Errorf("got %d %q, want %d %q", p, msg, c.want, c.msg)
			}
		})
	}
}

const (
	sgGet       = "get seedgenerators seedimage --ignore-not-found"
	sgPoll      = sgGet + " --request-timeout=10s"
	sgRunningJS = `{"metadata":{"name":"seedimage"},"status":{"conditions":[{"type":"SeedGenInProgress","status":"True","reason":"InProgress","message":"Launching imager container"}]}}`
	sgDoneJS    = `{"metadata":{"name":"seedimage"},"status":{"conditions":[{"type":"SeedGenCompleted","status":"True","reason":"Completed","message":"Seed Generation completed"}]}}`
	sgFailedJS  = `{"metadata":{"name":"seedimage"},"status":{"conditions":[{"type":"SeedGenCompleted","status":"False","reason":"Failed","message":"Seed Generation failed: push denied"}]}}`
)

func TestGenerate(t *testing.T) {
	ctx := context.Background()
	image := Ref(testRegistry, "a", "4.20.46")
	journal := strings.Join(journalCmd, " ")
	t.Run("follows the API going down and coming back", func(t *testing.T) {
		fa := &fakeAccess{
			oc: map[string][]resp{
				sgGet:  {{out: sgDoneJS}}, // a finished earlier run is replaced
				sgPoll: {{out: sgRunningJS}, {err: errors.New("connection refused")}, {out: ""}, {out: sgDoneJS}},
			},
			ssh: map[string][]resp{journal: {{out: "Pushing the seed image\n"}}},
		}
		e := testEnv(fa)
		if err := generate(ctx, e, fa, image, testRegistry.Auth(), e.Progress.Start("x")); err != nil {
			t.Fatal(err)
		}
		if !fa.ran("oc delete seedgenerators seedimage --wait") || !fa.ran("ssh "+journal) {
			t.Errorf("calls %q", fa.calls)
		}
		if len(fa.stdin) != 1 || !strings.Contains(fa.stdin[0], `seedImage: "`+image+`"`) || !strings.Contains(fa.stdin[0], "seedAuth:") {
			t.Errorf("applied %q", fa.stdin)
		}
	})
	t.Run("failed", func(t *testing.T) {
		fa := &fakeAccess{oc: map[string][]resp{sgPoll: {{out: sgFailedJS}}}}
		e := testEnv(fa)
		if err := generate(ctx, e, fa, image, "{}", e.Progress.Start("x")); err == nil || !strings.Contains(err.Error(), "push denied") {
			t.Errorf("got %v", err)
		}
		if fa.ran("oc delete") {
			t.Errorf("nothing to replace, but deleted: %q", fa.calls)
		}
	})
	t.Run("already running", func(t *testing.T) {
		fa := &fakeAccess{oc: map[string][]resp{sgGet: {{out: sgRunningJS}}}}
		e := testEnv(fa)
		if err := generate(ctx, e, fa, image, "{}", e.Progress.Start("x")); err == nil || !strings.Contains(err.Error(), "already running") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		fa := &fakeAccess{oc: map[string][]resp{sgPoll: {{out: sgRunningJS}}}}
		e := testEnv(fa)
		e.Now, e.Timeout = clock(), 5*time.Minute
		if err := generate(ctx, e, fa, image, "{}", e.Progress.Start("x")); err == nil || !strings.Contains(err.Error(), "not finished after 5m0s") {
			t.Errorf("got %v", err)
		}
	})
}

const listing = `{"repo":"seeds/b","tag":"4.22.0","digest":"sha256:bb","created":"2026-11-19T10:00:00Z","labels":{},"size":1048577}
{"repo":"seeds/a","tag":"4.20.46","digest":"sha256:aa","created":"2026-11-19T12:00:00Z","labels":{"com.openshift.lifecycle-agent.seed_cluster_info":"{\"seed_cluster_ocp_version\":\"4.20.46\"}","com.openshift.lifecycle-agent.seed_format_version":"3"},"size":2147483648}

`

func TestParseImages(t *testing.T) {
	imgs, err := parseImages(testRegistry.Host, []byte(listing))
	if err != nil {
		t.Fatal(err)
	}
	want := []Image{
		{Ref: testRegistry.Host + "/seeds/a:4.20.46", From: "a", Tag: "4.20.46", Digest: "sha256:aa", Version: "4.20.46", Format: "3", SizeBytes: 2 << 30, Created: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)},
		{Ref: testRegistry.Host + "/seeds/b:4.22.0", From: "b", Tag: "4.22.0", Digest: "sha256:bb", SizeBytes: 1048577, Created: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)},
	}
	if !slices.Equal(imgs, want) {
		t.Errorf("got %+v\nwant %+v", imgs, want)
	}
	if _, err := parseImages(testRegistry.Host, []byte("not json\n")); err == nil {
		t.Error("bad line: no error")
	}
}

func TestSelectImages(t *testing.T) {
	imgs, _ := parseImages(testRegistry.Host, []byte(listing+`{"repo":"seeds/a","tag":"4.20.47","digest":"sha256:a2"}`))
	refs := func(imgs []Image) []string {
		var out []string
		for _, i := range imgs {
			out = append(out, i.From+":"+i.Tag)
		}
		return out
	}
	for _, c := range []struct {
		args []string
		want []string
		err  bool
	}{
		{[]string{"a:4.20.46"}, []string{"a:4.20.46"}, false},
		{[]string{"seeds/b:4.22.0"}, []string{"b:4.22.0"}, false},
		{[]string{testRegistry.Host + "/seeds/b:4.22.0"}, []string{"b:4.22.0"}, false},
		{[]string{"a"}, []string{"a:4.20.46", "a:4.20.47"}, false},
		{[]string{"a", "a:4.20.46"}, []string{"a:4.20.46", "a:4.20.47"}, false}, // each once
		{[]string{"a:4.19.0"}, nil, true},
		{[]string{"b", "c"}, nil, true},
	} {
		got, err := selectImages(imgs, c.args, ns)
		if (err != nil) != c.err || !slices.Equal(refs(got), c.want) {
			t.Errorf("selectImages(%q) = %q, %v", c.args, refs(got), err)
		}
	}
}

// fakeRunner runs registry.sh: list prints out; delete echoes each ref.
type fakeRunner struct {
	out   string
	err   error
	calls [][]string
}

func (f *fakeRunner) Run(_ context.Context, s string, args []string, io_ builder.IO) error {
	if s != script("registry.sh") {
		return errors.New("unexpected script")
	}
	if io_.Stdin == nil {
		return errors.New("no credentials on stdin")
	}
	f.calls = append(f.calls, args)
	switch args[1] {
	case "list":
		_, _ = io.WriteString(io_.Stdout, f.out)
	case "delete":
		for _, ref := range args[2:] {
			_, _ = io.WriteString(io_.Stdout, "deleted "+ref+"\n")
		}
	}
	return f.err
}

func imagesEnv(r *fakeRunner) Env {
	return Env{Env: guest.Env{Kube: registrySecrets(), Builder: r}}
}

func TestList(t *testing.T) {
	r := &fakeRunner{out: listing}
	imgs, err := List(context.Background(), imagesEnv(r), ns)
	if err != nil || len(imgs) != 2 {
		t.Fatalf("got %d, %v", len(imgs), err)
	}
	if !slices.Equal(r.calls[0], []string{testRegistry.Host, "list"}) {
		t.Errorf("args %q", r.calls[0])
	}
	r = &fakeRunner{err: errors.New("exit 22")}
	if _, err := List(context.Background(), imagesEnv(r), ns); err == nil || !strings.Contains(err.Error(), "listing the registry") {
		t.Errorf("got %v", err)
	}
}

func TestDelete(t *testing.T) {
	ctx := context.Background()
	t.Run("dry run", func(t *testing.T) {
		r := &fakeRunner{out: listing}
		res, err := Delete(ctx, imagesEnv(r), DeleteOptions{Namespace: ns, Args: []string{"a"}, DryRun: true})
		if err != nil || len(res.Images) != 1 || len(r.calls) != 1 {
			t.Errorf("got %+v, %v, calls %q", res, err, r.calls)
		}
	})
	t.Run("not confirmed", func(t *testing.T) {
		r := &fakeRunner{out: listing}
		shown := 0
		_, err := Delete(ctx, imagesEnv(r), DeleteOptions{Namespace: ns, Args: []string{"a"},
			ShowPlan: func(imgs []Image) { shown = len(imgs) }, Confirm: func(_, _ string) bool { return false }})
		if err == nil || shown != 1 || len(r.calls) != 1 {
			t.Errorf("err %v, shown %d, calls %q", err, shown, r.calls)
		}
	})
	t.Run("confirmed", func(t *testing.T) {
		r := &fakeRunner{out: listing}
		var asked string
		res, err := Delete(ctx, imagesEnv(r), DeleteOptions{Namespace: ns, Args: []string{"a:4.20.46", "b"},
			Confirm: func(_, answer string) bool { asked = answer; return true }})
		if err != nil || asked != "a:4.20.46 b" {
			t.Fatalf("err %v, asked %q", err, asked)
		}
		if want := []string{testRegistry.Host, "delete", "seeds/a:4.20.46", "seeds/b:4.22.0"}; !slices.Equal(r.calls[1], want) {
			t.Errorf("args %q, want %q", r.calls[1], want)
		}
		if len(res.Deleted) != 2 || res.Deleted[0] != testRegistry.Host+"/seeds/a:4.20.46" {
			t.Errorf("deleted %q", res.Deleted)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		r := &fakeRunner{out: listing}
		if _, err := Delete(ctx, imagesEnv(r), DeleteOptions{Namespace: ns, Args: []string{"zzz"}, Yes: true}); err == nil || !strings.Contains(err.Error(), "no seed image zzz") {
			t.Errorf("got %v", err)
		}
	})
}
