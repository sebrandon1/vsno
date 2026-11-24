package cmd

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/guest"
)

func TestWaitTarget(t *testing.T) {
	for _, c := range []struct {
		in   string
		want guest.State
		bad  bool
	}{
		{"ready", guest.Ready, false},
		{"READY", guest.Ready, false},
		{" stopped ", guest.Stopped, false},
		{"failed", guest.Failed, false},
		{"installing", "", true},
		{"", "", true},
	} {
		got, err := waitTarget(c.in)
		if c.bad {
			if err == nil || !strings.Contains(err.Error(), "--for takes ready, stopped or failed") {
				t.Errorf("waitTarget(%q) = %v, want an error", c.in, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("waitTarget(%q) = %s, %v", c.in, got, err)
		}
	}
}

func TestWaitAppetite(t *testing.T) {
	for _, c := range []struct {
		want guest.State
		has  []guest.State
	}{

		{guest.Ready, []guest.State{guest.Ready, guest.Failed, guest.Abandoned}},
		{guest.Stopped, []guest.State{guest.Stopped, guest.Failed, guest.Abandoned}},
		{guest.Failed, []guest.State{guest.Failed, guest.Abandoned, guest.Ready, guest.Stopped}},
	} {
		got := waitAppetite(c.want)
		for _, s := range c.has {
			if !slices.Contains(got, s) {
				t.Errorf("appetite(%s) = %v, want %s", c.want, got, s)
			}
		}
		seen := map[guest.State]bool{}
		for _, s := range got {
			if seen[s] {
				t.Errorf("appetite(%s) = %v, has duplicates", c.want, got)
			}
			seen[s] = true
		}
	}
}

func TestWaitStatus(t *testing.T) {
	for _, c := range []struct {
		name string
		res  waitResult
		err  string
	}{
		{"reached", waitResult{VSNO: "a", For: guest.Ready, State: guest.Ready}, ""},
		{"waiting for failure found it", waitResult{VSNO: "a", For: guest.Failed, State: guest.Failed}, ""},
		{"failed while waiting ready", waitResult{VSNO: "a", For: guest.Ready, State: guest.Failed, Detail: "boot error"}, "is failed: boot error"},
		{"abandoned", waitResult{VSNO: "a", For: guest.Ready, State: guest.Abandoned}, "is abandoned"},
		{"settled but wrong", waitResult{VSNO: "a", For: guest.Failed, State: guest.Ready}, "is ready, not failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := waitStatus(c.res)
			if c.err == "" {
				if err != nil {
					t.Errorf("waitStatus = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("waitStatus = %v, want %q", err, c.err)
			}
		})
	}
}

func TestWaitRender(t *testing.T) {
	var b bytes.Buffer
	waitRender(&b, waitResult{VSNO: "a", For: guest.Ready, State: guest.Ready, Detail: "4.22.18"}, plain)
	if !strings.Contains(b.String(), "vSNO a is ready") || !strings.Contains(b.String(), "4.22.18") {
		t.Errorf("reached:\n%s", b.String())
	}
	b.Reset()
	waitRender(&b, waitResult{VSNO: "a", For: guest.Ready, State: guest.Failed}, plain)
	if b.Len() != 0 {
		t.Errorf("not reached printed %q, want nothing", b.String())
	}
}
