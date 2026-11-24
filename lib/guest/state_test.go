package guest

import (
	"testing"
	"time"
)

func intp(n int) *int { return &n }

func TestClassify(t *testing.T) {
	running := func(p *Probe) Observed { return Observed{VMStatus: "Running", ISOPhase: "Succeeded", Probe: p} }
	validations := `{"network":[{"id":"dns-wildcard-not-configured","status":"failure"},{"id":"api-domain-name-resolved-correctly","status":"success"}],"hardware":[{"id":"has-memory-for-role","status":"failure"}]}`
	for _, c := range []struct {
		name   string
		o      Observed
		state  State
		detail string
	}{
		{"deleting", Observed{VMStatus: "Terminating"}, Deleting, ""},
		{"stopped", Observed{VMStatus: "Stopped"}, Stopped, ""},
		{"stopping", Observed{VMStatus: "Stopping"}, Stopped, ""},
		{"unschedulable", Observed{VMStatus: "ErrorUnschedulable"}, Failed, "VM ErrorUnschedulable"},
		{"iso failed", Observed{VMStatus: "Provisioning", ISOPhase: "Failed"}, Failed, "ISO import failed"},
		{"importing", Observed{VMStatus: "Provisioning", ISOPhase: "ImportInProgress", ISOProgress: "45.10% "}, Importing, "45.10%"},
		{"import not started", Observed{VMStatus: "Provisioning", ISOPhase: "ImportScheduled", ISOProgress: "N/A"}, Importing, ""},
		{"starting", Observed{VMStatus: "Starting", ISOPhase: "Succeeded"}, Starting, "Starting"},
		{"no ISO DataVolume", Observed{VMStatus: "Starting"}, Starting, "Starting"},
		{"not probed", Observed{VMStatus: "Running", ISOPhase: "Succeeded"}, Unknown, "not probed"},
		{"nothing answers", running(&Probe{}), Unknown, "no answer from the installer or API"},
		// KubeVirt reports a VM Stopped for a moment after it is created.
		{"just created", Observed{VMStatus: "Stopped", WantsRunning: true, ISOPhase: "Succeeded"}, Starting, ""},
		{"stopped on purpose", Observed{VMStatus: "Stopped", ISOPhase: "Succeeded"}, Stopped, ""},
		{"agent installing", running(&Probe{Agent: &Agent{Status: "installing-in-progress", Progress: 42}}), Installing, "installing-in-progress 42%"},
		{"validations", running(&Probe{Agent: &Agent{Status: "insufficient", Validations: validations}}), Installing, "waiting on validations: dns-wildcard-not-configured, has-memory-for-role"},
		{"agent error", running(&Probe{Agent: &Agent{Status: "error", Info: "boom"}}), Failed, "install error: boom"},
		{"api up", running(&Probe{CV: &CV{Message: "Working towards 4.20.46: 600 of 900 done"}}), Finishing, "control plane rolling out"},
		// The CVO reports errors that clear themselves while the control plane
		// rolls out; only one that has been going on is worth repeating.
		{"a fresh rollout does not read as a failure", func() Observed {
			o := running(&Probe{CV: &CV{Message: "Unable to apply 4.20.46: MultipleErrors", Since: time.Now().Add(-2 * time.Minute)}})
			o.Now = time.Now()
			return o
		}(), Finishing, "control plane rolling out"},
		{"a rollout that never finished shows its message", func() Observed {
			o := running(&Probe{CV: &CV{Message: "Unable to apply 4.20.46: MultipleErrors", Since: time.Now().Add(-time.Hour)}})
			o.Now = time.Now()
			return o
		}(), Finishing, "Unable to apply 4.20.46: MultipleErrors"},
		// ...unless every operator reports Available: then the CVO is naming an
		// operator that has since come back (authentication after an IBU), and
		// the raw message stays in CVMessage for `-o json` instead.
		{"a message the operators contradict is stale", func() Observed {
			o := running(&Probe{CV: &CV{Message: "Error while reconciling 4.22.23: the cluster operator authentication is not available", Since: time.Now().Add(-time.Hour)}, Unhealthy: intp(0)})
			o.Now = time.Now()
			return o
		}(), Finishing, "control plane rolling out"},
		// Seed generation stops the guest's services: no probe answer is progress.
		{"making the seed image", Observed{VMStatus: "Running", ISOPhase: "Succeeded", Probe: &Probe{}, Seeding: true}, MakingSeed, seedDetail},
		{"making the seed image, not probed", Observed{VMStatus: "Running", ISOPhase: "Succeeded", Seeding: true}, MakingSeed, seedDetail},
		{"a stale seeding mark does not hide a silent guest", Observed{VMStatus: "Running", ISOPhase: "Succeeded", Probe: &Probe{}, Seeding: false}, Unknown, "no answer from the installer or API"},
		{"ready", running(&Probe{CV: &CV{Available: true}, Unhealthy: intp(0)}), Ready, ""},
		{"ready, degraded", running(&Probe{CV: &CV{Available: true}, Unhealthy: intp(2)}), Ready, "2 operator(s) degraded or unavailable"},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, detail := Classify(c.o)
			if st != c.state || detail != c.detail {
				t.Errorf("got %s %q, want %s %q", st, detail, c.state, c.detail)
			}
		})
	}
}

func TestFailing(t *testing.T) {
	var nilAgent *Agent
	for _, a := range []*Agent{nilAgent, {}, {Validations: "not json"}, {Validations: `{"x":[{"id":"a","status":"success"}]}`}} {
		if got := a.Failing(); got != nil {
			t.Errorf("Failing(%+v) = %v", a, got)
		}
	}
}

func TestIBUText(t *testing.T) {
	for _, c := range []struct {
		u    *IBU
		want string
	}{
		{nil, ""},
		{&IBU{Stage: "Idle", Completed: false}, ""},
		{&IBU{}, ""},
		{&IBU{Stage: "Prep"}, "Prep"},
		{&IBU{Stage: "Upgrade", Completed: true}, "Upgrade done"},
	} {
		if got := c.u.Text(); got != c.want {
			t.Errorf("%+v = %q, want %q", c.u, got, c.want)
		}
	}
}

// IBUDone reads the text Guest.IBU carries: a finished stage ends in " done".
func TestIBUDone(t *testing.T) {
	for _, c := range []struct {
		ibu  string
		want bool
	}{{"", false}, {"Upgrade", false}, {"Upgrade done", true}, {"Rollback done", true}, {"Prep done", true}} {
		if got := (Guest{IBU: c.ibu}).IBUDone(); got != c.want {
			t.Errorf("IBUDone(%q) = %v", c.ibu, got)
		}
	}
}
