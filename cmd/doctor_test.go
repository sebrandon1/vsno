package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/doctor"
	"github.com/sebrandon1/vsno/lib/precheck"
)

// A healthy environment prints nothing; findings print as a table, or as JSON.
func TestRenderDoctor(t *testing.T) {
	t.Cleanup(func() { outputFormat = formatTable })
	var b bytes.Buffer
	c := doctorCmd
	c.SetOut(&b)
	t.Cleanup(func() { c.SetOut(nil) })
	if err := renderDoctor(c, nil); err != nil {
		t.Fatal(err)
	}
	if b.Len() != 0 {
		t.Errorf("healthy printed %q", b.String())
	}

	findings := []doctor.Finding{
		{VSNo: "a", Result: precheck.Result{Group: "doctor", Check: "a: guest services", Status: precheck.Fail, Detail: "kubelet disabled"}, Fix: "enable them", Action: doctor.EnableServices},
	}
	b.Reset()
	if err := renderDoctor(c, findings); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CHECK", "a: guest services", "fail", "kubelet disabled", "enable them"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q:\n%s", want, b.String())
		}
	}

	b.Reset()
	outputFormat = formatJSON
	if err := renderDoctor(c, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "[]") {
		t.Errorf("json with no findings = %q, want an empty array", b.String())
	}
}
