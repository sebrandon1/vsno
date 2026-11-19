package guest

import (
	"testing"
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
		{"nothing answers", running(&Probe{}), Unknown, "booting or rebooting"},
		{"agent installing", running(&Probe{Agent: &Agent{Status: "installing-in-progress", Progress: 42}}), Installing, "installing-in-progress 42%"},
		{"validations", running(&Probe{Agent: &Agent{Status: "insufficient", Validations: validations}}), Installing, "waiting on validations: dns-wildcard-not-configured, has-memory-for-role"},
		{"agent error", running(&Probe{Agent: &Agent{Status: "error", Info: "boom"}}), Failed, "install error: boom"},
		{"api up", running(&Probe{CV: &CV{Message: "Working towards 4.20.46: 600 of 900 done"}}), Finishing, "Working towards 4.20.46: 600 of 900 done"},
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
