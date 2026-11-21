package guestctl_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/guestctl"
	"github.com/sebrandon1/vsno/lib/guestctl/guestctltest"
	"github.com/sebrandon1/vsno/lib/precheck"
)

func TestGet(t *testing.T) {
	ctx := context.Background()
	f := &guestctltest.Fake{OcReplies: map[string][]guestctltest.Reply{
		"get ibu upgrade":                    {{Out: `{"metadata":{"name":"upgrade"},"status":{"conditions":[{"type":"Idle","status":"True"}]}}`}},
		"get ibu upgrade --ignore-not-found": {{Out: "\n"}},
		"get ibu broken":                     {{Out: "not json"}},
		"get ibu gone":                       {{Err: errors.New("exit status 1")}},
	}}
	var o guestctl.Object
	if found, err := guestctl.Get(ctx, f, &o, "get", "ibu", "upgrade"); err != nil || !found || !o.IsTrue("Idle") || o.Metadata.Name != "upgrade" {
		t.Errorf("found = %v, err = %v, object = %+v", found, err, o)
	}
	if found, err := guestctl.Get(ctx, f, &o, "get", "ibu", "upgrade", "--ignore-not-found"); err != nil || found {
		t.Errorf("a missing object: found = %v, err = %v", found, err)
	}
	if _, err := guestctl.Get(ctx, f, &o, "get", "ibu", "broken"); err == nil || !strings.Contains(err.Error(), "parsing the output") {
		t.Errorf("bad JSON: err = %v", err)
	}
	if _, err := guestctl.Get(ctx, f, &o, "get", "ibu", "gone"); err == nil || !strings.Contains(err.Error(), "oc get ibu gone") {
		t.Errorf("failed oc: err = %v", err)
	}
}

// A failed oc reports its arguments, its error and the last line of stderr.
func TestOcReportsTheLastStderrLine(t *testing.T) {
	a := stderrAccess{stderr: "Warning: something\nError from server (NotFound): ibu \"x\" not found\n"}
	err := guestctl.Oc(context.Background(), a, "", "delete", "ibu", "x")
	want := `oc delete ibu x: exit status 1: Error from server (NotFound): ibu "x" not found`
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}

func TestApplyPassesTheManifests(t *testing.T) {
	f := &guestctltest.Fake{}
	if err := guestctl.Apply(context.Background(), f, "kind: Namespace\n"); err != nil {
		t.Fatal(err)
	}
	if !f.Ran("oc apply -f -") || len(f.Stdin) != 1 || f.Stdin[0] != "kind: Namespace\n" {
		t.Errorf("calls = %q, stdin = %q", f.Calls, f.Stdin)
	}
}

func TestSSHOutTrims(t *testing.T) {
	f := &guestctltest.Fake{SSHReplies: map[string][]guestctltest.Reply{"uptime": {{Out: " up 3 days \n"}}}}
	if out, err := guestctl.SSHOut(context.Background(), f, "uptime"); err != nil || out != "up 3 days" {
		t.Errorf("out = %q, err = %v", out, err)
	}
}

func TestObjectConditions(t *testing.T) {
	var o guestctl.Object
	o.Status.Conditions = []guestctl.Condition{{Type: "PrepCompleted", Status: "False", Message: "pulling"}}
	if o.IsTrue("PrepCompleted") || o.Cond("PrepCompleted").Message != "pulling" || o.Cond("Idle") != nil || o.IsTrue("Idle") {
		t.Errorf("conditions = %+v", o.Status.Conditions)
	}
}

func TestPoll(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	c := guestctl.Clock{Every: time.Millisecond, Now: func() time.Time { now = now.Add(time.Minute); return now }}

	n := 0
	if err := guestctl.Poll(ctx, c, time.Hour, func() (bool, error) { n++; return n == 3, nil }); err != nil || n != 3 {
		t.Errorf("done on the third try: n = %d, err = %v", n, err)
	}
	if err := guestctl.Poll(ctx, c, 5*time.Minute, func() (bool, error) { return false, nil }); !errors.Is(err, guestctl.ErrTimeout) {
		t.Errorf("never done: err = %v, want a timeout", err)
	}
	boom := errors.New("boom")
	if err := guestctl.Poll(ctx, c, time.Hour, func() (bool, error) { return false, boom }); !errors.Is(err, boom) {
		t.Errorf("failing: err = %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	slow := guestctl.Clock{Every: time.Hour, Now: time.Now}
	if err := guestctl.Poll(canceled, slow, time.Hour, func() (bool, error) { return false, nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: err = %v", err)
	}
}

func TestStep(t *testing.T) {
	p := &recorder{}
	if err := guestctl.Step(p, "Prep", func(precheck.ProgressStep) (string, error) { return "done in 3m", nil }); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	if err := guestctl.Step(p, "Upgrade", func(precheck.ProgressStep) (string, error) { return "", boom }); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
	want := []string{"start Prep", "done done in 3m", "start Upgrade", "fail boom"}
	if strings.Join(p.events, "|") != strings.Join(want, "|") {
		t.Errorf("events = %q, want %q", p.events, want)
	}
}

// recorder is a Progress that records what its steps report.
type recorder struct{ events []string }

func (r *recorder) Start(title string) precheck.ProgressStep {
	r.events = append(r.events, "start "+title)
	return r
}
func (r *recorder) Update(string)        {}
func (r *recorder) SetProgress(int, int) {}
func (r *recorder) Done(m string)        { r.events = append(r.events, "done "+m) }
func (r *recorder) Warn(m string)        { r.events = append(r.events, "warn "+m) }
func (r *recorder) Fail(m string)        { r.events = append(r.events, "fail "+m) }

// stderrAccess fails every oc call after writing stderr.
type stderrAccess struct{ stderr string }

func (s stderrAccess) Oc(_ context.Context, _ []string, io builder.IO) error {
	_, _ = io.Stderr.Write([]byte(s.stderr))
	return errors.New("exit status 1")
}
func (s stderrAccess) SSH(context.Context, []string, builder.IO) error { return nil }

func TestDeployment(t *testing.T) {
	var d guestctl.Deployment
	if err := json.Unmarshal([]byte(`{"spec":{"template":{"spec":{"containers":[{"name":"manager","image":"reg/lca:1"},{"name":"proxy","image":"reg/p:2"}]}}},`+
		`"status":{"replicas":1,"updatedReplicas":1,"availableReplicas":1}}`), &d); err != nil {
		t.Fatal(err)
	}
	if d.Image("manager") != "reg/lca:1" || d.Image("nope") != "" || !d.RunsImage("reg/p:2") || d.RunsImage("reg/x:3") || !d.RolledOut() {
		t.Errorf("%+v", d)
	}
	// Mid-rollout: the old replica is still the available one.
	d.Status.Replicas, d.Status.UnavailableReplicas = 2, 1
	if d.RolledOut() {
		t.Error("mid-rollout reads rolled out")
	}
}
