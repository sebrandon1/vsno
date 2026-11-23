// Package integration runs the built vsno binary as a child process and asserts
// on what it does as a program: its exit code, its stdout and its stderr.
//
// The unit tests in cmd and lib drive the commands in process, against fakes.
// These cover the parts only a real process has: that -v prints to stderr and
// leaves stdout alone, that -o json stays parseable with tracing on, and that a
// refusal is one line of stderr with nothing on stdout. No cluster is needed: a
// fake API server stands in for one.
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode"
	"unicode/utf8"
)

// traceMark begins every trace line.
const traceMark = "→"

// looksTraced reports whether s holds a line trace wrote: the arrow, then a
// verb in the lowercase kubectl style. Progress steps share the arrow but
// begin with a capital ("→ Checking ..."), and a table cell can hold one too,
// so the arrow alone does not say where a line came from.
func looksTraced(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		rest, ok := strings.CutPrefix(line, traceMark+" ")
		if !ok || rest == "" {
			continue
		}
		if r, _ := utf8.DecodeRuneInString(rest); unicode.IsLower(r) {
			return true
		}
	}
	return false
}

// namespaceJSONFor is an environment's namespace, as vsno setup labels it.
func namespaceJSONFor(name string) string {
	return `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"` + name +
		`","labels":{"app.kubernetes.io/managed-by":"vsno"}}}`
}

// notFoundJSON is what the fake host says to every read it does not answer:
// the commands then report "not set up" rather than hanging or panicking.
const notFoundJSON = `{"kind":"Status","apiVersion":"v1","status":"Failure",` +
	`"reason":"NotFound","message":"not found","code":404}`

var (
	// vsno is the binary under test, built once.
	vsno string
	// home is an empty directory, so the CLI never reads a real ~/.kube/config.
	home string
)

func TestMain(m *testing.M) {
	// os.Exit skips deferred cleanup, so the temp dir is removed here instead.
	dir, code := build()
	if code == 0 {
		code = m.Run()
	}
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// build compiles the binary into a temp dir and returns that dir and the exit
// code to use when it cannot.
func build() (string, int) {
	dir, err := os.MkdirTemp("", "vsno-integration-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: temp dir: %v\n", err)
		return "", 1
	}
	vsno = filepath.Join(dir, "vsno")
	home = filepath.Join(dir, "home")

	if err := os.Mkdir(home, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "integration: home dir: %v\n", err)
		return dir, 1
	}
	if out, err := gobuild(vsno); err != nil {
		fmt.Fprintf(os.Stderr, "integration: go build: %v\n%s", err, out)
		return dir, 1
	}
	return dir, 0
}

// gobuild builds the package in the repository root.
func gobuild(out string) ([]byte, error) {
	cmd := exec.CommandContext(context.Background(), "go", "build", "-o", out, ".")
	cmd.Dir = repoRoot()
	return cmd.CombinedOutput()
}

// repoRoot is the directory holding go.mod, two levels above this file.
func repoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "."
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// result is what one run of the binary produced.
type result struct {
	stdout string
	stderr string
	code   int
}

// run runs the binary with a fixed environment and the given arguments. The
// environment carries no inherited kubeconfig, no color and no real home, so a
// developer's own cluster never changes the answer.
func run(t *testing.T, kubeconfig string, args ...string) result {
	t.Helper()
	if kubeconfig == "" {
		kubeconfig = filepath.Join(t.TempDir(), "no-such-kubeconfig")
	}
	cmd := exec.CommandContext(t.Context(), vsno, args...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"NO_COLOR=1",
		"KUBECONFIG=" + kubeconfig,
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	code := 0
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("running vsno %s: %v", strings.Join(args, " "), err)
		}
		code = exit.ExitCode()
	}
	return result{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

// fakeHost answers the reads vsno makes of a host cluster, so that a command
// runs to its own conclusion with no cluster behind it.
type fakeHost struct {
	server *httptest.Server
	// names are the vsno environments the host has.
	names []string

	mu    sync.Mutex
	paths []string
}

// newFakeHost starts a host with one environment, vsno, and nothing in it.
func newFakeHost(t *testing.T) *fakeHost { return newFakeHostNamed(t, "vsno") }

// newFakeHostNamed starts a host holding the named environments.
func newFakeHostNamed(t *testing.T, names ...string) *fakeHost {
	t.Helper()
	f := &fakeHost{names: names}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeHost) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.RequestURI())
	f.mu.Unlock()

	p := r.URL.Path
	switch {
	case p == "/api/v1/namespaces":
		list := make([]string, len(f.names))
		for i, n := range f.names {
			list[i] = namespaceJSONFor(n)
		}
		writeJSON(w, http.StatusOK, `{"apiVersion":"v1","kind":"NamespaceList","items":[`+strings.Join(list, ",")+`]}`)
	case f.namesOneOf(strings.TrimPrefix(p, "/api/v1/namespaces/")):
		name := strings.TrimPrefix(p, "/api/v1/namespaces/")
		writeJSON(w, http.StatusOK, namespaceJSONFor(name))
	case p == "/api/v1/pods":
		writeJSON(w, http.StatusOK, emptyList("v1", "PodList"))
	case strings.HasSuffix(p, "/services"):
		writeJSON(w, http.StatusOK, emptyList("v1", "ServiceList"))
	case strings.HasSuffix(p, "/virtualmachines"):
		writeJSON(w, http.StatusOK, emptyList("kubevirt.io/v1", "VirtualMachineList"))
	default:
		writeJSON(w, http.StatusNotFound, notFoundJSON)
	}
}

// namesOneOf reports whether name is one of the host's environments, rather
// than a path below one.
func (f *fakeHost) namesOneOf(name string) bool {
	return !strings.Contains(name, "/") && slices.Contains(f.names, name)
}

// requests returns the request paths the host has answered.
func (f *fakeHost) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

// kubeconfig writes a kubeconfig pointing at the fake host.
func (f *fakeHost) kubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	cfg := "apiVersion: v1\nkind: Config\n" +
		"clusters:\n- name: fake\n  cluster:\n    server: " + f.server.URL + "\n" +
		"contexts:\n- name: fake\n  context:\n    cluster: fake\n    user: fake\n" +
		"current-context: fake\n" +
		"users:\n- name: fake\n  user: {}\n"
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	return path
}

func writeJSON(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, body)
}

// emptyList is an empty list of the given kind: enough for a command to report
// that nothing is there, rather than that it could not look.
func emptyList(apiVersion, kind string) string {
	return fmt.Sprintf(`{"apiVersion":%q,"kind":%q,"items":[]}`, apiVersion, kind)
}

func TestHelp(t *testing.T) {
	r := run(t, "", "--help")

	if r.code != 0 {
		t.Errorf("exit code = %d, want 0; stderr: %s", r.code, r.stderr)
	}
	for _, want := range []string{"-v, --verbose", "--version"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout does not mention %q", want)
		}
	}
	if r.stderr != "" {
		t.Errorf("stderr = %q, want empty", r.stderr)
	}
}

func TestVersion(t *testing.T) {
	r := run(t, "", "--version")

	if r.code != 0 {
		t.Errorf("exit code = %d, want 0; stderr: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "vsno version") {
		t.Errorf("stdout = %q, want it to name the version", r.stdout)
	}
	if r.stderr != "" {
		t.Errorf("stderr = %q, want empty", r.stderr)
	}
}

// TestVerboseIsNotVersion pins the deliberate break: -v means verbose, so a bare
// "vsno -v" prints help rather than the version. --version and "vsno version"
// both still work, covered above and below.
func TestVerboseIsNotVersion(t *testing.T) {
	r := run(t, "", "-v")

	if r.code != 0 {
		t.Errorf("exit code = %d, want 0; stderr: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "Usage:") {
		t.Errorf("stdout = %q, want the help text", r.stdout)
	}
	// The help text mentions "Print the vsno version", so look for the version
	// line itself rather than the phrase.
	if strings.HasPrefix(strings.TrimSpace(r.stdout), "vsno version") {
		t.Errorf("stdout starts with the version, want help: %q", r.stdout)
	}
	if got := strings.TrimSpace(run(t, "", "version").stdout); got != "dev" {
		t.Errorf("vsno version = %q, want the build version", got)
	}
}

// TestRefusedBeforeTouchingACluster covers the arguments a command rejects
// before it reads a kubeconfig: the error is one line of stderr, and stdout
// stays empty so a caller reading it gets nothing rather than half an answer.
func TestRefusedBeforeTouchingACluster(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"bad output format", []string{"-o", "bogus", "list"}, `unsupported output format "bogus"`},
		{"wide without a wide view", []string{"-o", "wide", "status"}, "has no wide output"},
		{"no kubeconfig", []string{"status"}, "kubeconfig"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := run(t, "", tt.args...)

			if r.code == 0 {
				t.Errorf("exit code = 0, want non-zero")
			}
			if r.stdout != "" {
				t.Errorf("stdout = %q, want empty", r.stdout)
			}
			if !strings.Contains(r.stderr, tt.want) {
				t.Errorf("stderr = %q, want it to contain %q", r.stderr, tt.want)
			}
			if looksTraced(r.stderr) {
				t.Errorf("stderr has trace lines without -v: %q", r.stderr)
			}
		})
	}
}

// TestTraceIsStderrOnly is the point of the flag: with -v the requests appear on
// stderr and the table on stdout is exactly what it was without it.
func TestTraceIsStderrOnly(t *testing.T) {
	host := newFakeHost(t)
	kubeconfig := host.kubeconfig(t)

	quiet := run(t, kubeconfig, "status")
	traced := run(t, kubeconfig, "-v", "status")

	if quiet.stdout != traced.stdout {
		t.Errorf("stdout changed with -v:\nwith: %q\nwithout: %q", traced.stdout, quiet.stdout)
	}
	if !strings.Contains(quiet.stdout, "Environment vsno") {
		t.Errorf("stdout = %q, want the status table", quiet.stdout)
	}
	if looksTraced(traced.stdout) {
		t.Errorf("stdout carries trace lines: %q", traced.stdout)
	}
	if looksTraced(quiet.stderr) {
		t.Errorf("stderr has trace lines without -v: %q", quiet.stderr)
	}
	if !looksTraced(traced.stderr) {
		t.Errorf("stderr has no trace lines with -v: %q", traced.stderr)
	}

	for _, want := range []string{
		"→ get namespaces -l app.kubernetes.io/managed-by=vsno",
		"→ get virtualmachines.kubevirt.io -n vsno",
	} {
		if !strings.Contains(traced.stderr, want) {
			t.Errorf("stderr = %q, want it to contain %q", traced.stderr, want)
		}
	}
	// The traced lines describe traffic the host really answered.
	if len(host.requests()) == 0 {
		t.Error("the fake host answered nothing")
	}
}

// TestJSONStaysCleanWithTrace covers the pipeable half: -o json output parses
// even when -v is writing about the requests that produced it.
func TestJSONStaysCleanWithTrace(t *testing.T) {
	host := newFakeHost(t)

	r := run(t, host.kubeconfig(t), "-v", "-o", "json", "list")

	if r.code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", r.code, r.stderr)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%q", err, r.stdout)
	}
	if !strings.Contains(r.stderr, "→ get services -n vsno -l vsno.io/name") {
		t.Errorf("stderr = %q, want the services request traced", r.stderr)
	}
}

// TestCommandsRejectStrayArguments pins cobra.NoArgs on the commands that take
// no operands: "vsno setup typo" used to succeed, silently doing nothing with
// the typo.
func TestCommandsRejectStrayArguments(t *testing.T) {
	for _, cmd := range []string{"setup", "precheck", "status", "teardown", "version"} {
		t.Run(cmd, func(t *testing.T) {
			r := run(t, "", cmd, "typo")

			if r.code == 0 {
				t.Errorf("%s typo exited 0, want an error", cmd)
			}
			if !strings.Contains(r.stderr, "typo") {
				t.Errorf("stderr = %q, want it to name the argument", r.stderr)
			}
		})
	}
}

// TestListWideEmpty covers the wide view's empty state against a host that
// answers: the hint, not a bare header row. A populated row needs a VM fixture
// the fake host does not carry yet.
func TestListWideEmpty(t *testing.T) {
	host := newFakeHost(t)

	r := run(t, host.kubeconfig(t), "list", "-o", "wide")

	if r.code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "No vSNOs") {
		t.Errorf("stdout = %q, want the empty-state hint", r.stdout)
	}
	if looksTraced(r.stdout) {
		t.Errorf("stdout carries trace lines: %q", r.stdout)
	}
}

// TestStatusJSON: -o json must stay parseable even when the command exits
// non-zero because the environment is not set up.
func TestStatusJSON(t *testing.T) {
	host := newFakeHost(t)

	r := run(t, host.kubeconfig(t), "-v", "-o", "json", "status")

	var out map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%q", err, r.stdout)
	}
	if !looksTraced(r.stderr) {
		t.Errorf("stderr = %q, want the requests traced", r.stderr)
	}
}

func TestVersionJSON(t *testing.T) {
	r := run(t, "", "-o", "json", "version")

	if r.code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", r.code, r.stderr)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%q", err, r.stdout)
	}
	if v, _ := out["version"].(string); v == "" {
		t.Errorf("version = %v, want it set", out["version"])
	}
}

// A command that hands over a stream has nothing to put in -o json, and says so
// rather than printing human text that a parser would choke on.
func TestJSONRefusedForStreams(t *testing.T) {
	r := run(t, "", "-o", "json", "oc", "seed")

	if r.code == 0 {
		t.Errorf("exit code = 0, want an error")
	}
	if !strings.Contains(r.stderr, "has no json output") {
		t.Errorf("stderr = %q, want the refusal", r.stderr)
	}
	if r.stdout != "" {
		t.Errorf("stdout = %q, want empty", r.stdout)
	}
}

// setup and teardown work in the environment that is there, like every other
// command: before this, `vsno teardown` after `vsno setup -n vsno-dev` aimed at
// the default `vsno` and reported nothing to delete.
func TestTeardownPicksTheEnvironment(t *testing.T) {
	host := newFakeHostNamed(t, "vsno-dev")

	r := run(t, host.kubeconfig(t), "teardown", "--dry-run")

	if !strings.Contains(r.stdout+r.stderr, "vsno-dev") {
		t.Errorf("output does not name vsno-dev:\nstdout: %s\nstderr: %s", r.stdout, r.stderr)
	}
	if strings.Contains(r.stderr, "Nothing to delete") {
		t.Errorf("teardown aimed at the wrong environment: %s", r.stderr)
	}
}

// Guessing at which environment to destroy is worse than asking.
func TestTeardownRefusesToGuess(t *testing.T) {
	host := newFakeHostNamed(t, "vsno", "vsno-dev")

	r := run(t, host.kubeconfig(t), "teardown", "--dry-run")

	if r.code == 0 {
		t.Errorf("exit code = 0 with two environments and no -n")
	}
	if !strings.Contains(r.stderr, "several vsno environments") {
		t.Errorf("stderr = %q, want the ambiguity error", r.stderr)
	}
}

func TestSetupPicksTheEnvironment(t *testing.T) {
	host := newFakeHostNamed(t, "vsno-dev")

	r := run(t, host.kubeconfig(t), "setup", "--dry-run", "--no-probe")

	if !strings.Contains(r.stderr, "Setting up vsno-dev") {
		t.Errorf("stderr does not name vsno-dev: %q", r.stderr)
	}
}

// config needs no cluster at all: set, get, path and unset are one file.
func TestConfigRoundTrip(t *testing.T) {
	if r := run(t, "", "config", "set", "seed.sync.to", "quay.io/me/seeds"); r.code != 0 {
		t.Fatalf("set: exit %d, stderr %q", r.code, r.stderr)
	}
	if r := run(t, "", "config", "get", "seed.sync.to"); r.code != 0 || strings.TrimSpace(r.stdout) != "quay.io/me/seeds" {
		t.Errorf("get: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if r := run(t, "", "config", "path"); r.code != 0 || !strings.HasSuffix(strings.TrimSpace(r.stdout), filepath.Join("vsno", "config")) {
		t.Errorf("path: exit %d, stdout %q", r.code, r.stdout)
	}
	if r := run(t, "", "config", "unset", "seed.sync.to"); r.code != 0 {
		t.Fatalf("unset: exit %d, stderr %q", r.code, r.stderr)
	}
	if r := run(t, "", "config", "get", "seed.sync.to"); r.code == 0 {
		t.Errorf("get after unset: exit 0, stdout %q", r.stdout)
	}
}
