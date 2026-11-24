package cmd

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/kube"
)

// row is what one vSNO should come back as.
type row struct {
	name  string
	state guest.State
	ver   string
}

// collect reads one environment: the VMs, and the Service left behind while an
// ISO is being built.
func TestCollect(t *testing.T) {
	for _, c := range []struct {
		name     string
		setup    func(t *testing.T, cl *kube.Clients)
		want     []row
		capacity int
	}{
		{
			name: "one stopped",
			setup: func(t *testing.T, cl *kube.Clients) {
				addVM(t, cl, "a", "4.22", false)
			},
			want:     []row{{"a", guest.Stopped, "4.22"}},
			capacity: 1,
		},
		{
			name: "a stopped one and one starting",
			setup: func(t *testing.T, cl *kube.Clients) {
				addVM(t, cl, "a", "4.22", false)
				addVM(t, cl, "b", "4.21", true)
			},
			want:     []row{{"a", guest.Stopped, "4.22"}, {"b", guest.Starting, "4.21"}},
			capacity: 1,
		},
		{
			name: "a vSNO still building its ISO",
			setup: func(t *testing.T, cl *kube.Clients) {
				addVM(t, cl, "a", "4.22", false)
				addBuilding(t, cl, "b", "4.22")
			},
			want:     []row{{"a", guest.Stopped, "4.22"}, {"b", guest.Building, "4.22"}},
			capacity: 1,
		},
		{
			name:  "empty",
			setup: func(t *testing.T, cl *kube.Clients) {},
			want:  nil,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			cl := fakeClients(t)
			c.setup(t, cl)
			l, err := collect(t.Context(), cl, fakeEnv(cl), true)
			if err != nil {
				t.Fatal(err)
			}
			if l.Namespace != namespace {
				t.Errorf("namespace = %q, want %q", l.Namespace, namespace)
			}
			if len(l.VSNOs) != len(c.want) {
				t.Fatalf("vsnos = %+v, want %d", l.VSNOs, len(c.want))
			}
			for i, w := range c.want {
				g := l.VSNOs[i]
				if g.Name != w.name || g.State != w.state || g.Version != w.ver {
					t.Errorf("vsno %d = %s/%s/%s, want %s/%s/%s", i, g.Name, g.State, g.Version, w.name, w.state, w.ver)
				}
			}
			if len(l.Capacity) != c.capacity {
				t.Errorf("capacity rows = %d, want %d", len(l.Capacity), c.capacity)
			}
		})
	}
}

// filterWorld is three vSNOs in the states and versions a filter picks from.
func filterWorld() []guest.Guest {
	return []guest.Guest{
		{Name: "a", State: guest.Ready, Version: "4.22.18"},
		{Name: "b", State: guest.Stopped, Requested: "4.21"},
		{Name: "c", State: guest.Ready, Version: "4.21.30"},
	}
}

func TestListFilterApply(t *testing.T) {
	for _, c := range []struct {
		name string
		f    listFilter
		want []string
	}{
		{"none", listFilter{}, []string{"a", "b", "c"}},
		{"one name", listFilter{Names: []string{"b"}}, []string{"b"}},
		{"two names", listFilter{Names: []string{"a", "c"}}, []string{"a", "c"}},
		{"state", listFilter{States: []guest.State{guest.Ready}}, []string{"a", "c"}},
		{"two states", listFilter{States: []guest.State{guest.Ready, guest.Stopped}}, []string{"a", "b", "c"}},
		{"exact version", listFilter{Version: "4.22.18"}, []string{"a"}},
		{"minor", listFilter{Version: "4.21"}, []string{"b", "c"}},
		{"requested version", listFilter{Version: "4.21"}, []string{"b", "c"}},
		{"all three", listFilter{Names: []string{"a", "c"}, States: []guest.State{guest.Ready}, Version: "4.21"}, []string{"c"}},
		{"nothing matches", listFilter{Names: []string{"a"}, States: []guest.State{guest.Failed}}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, g := range c.f.apply(filterWorld()) {
				got = append(got, g.Name)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("apply = %v, want %v", got, c.want)
			}
		})
	}
}

func TestParseStates(t *testing.T) {
	for _, c := range []struct {
		name string
		in   []string
		want []guest.State
		err  string
	}{
		{"repeated", []string{"ready", "stopped"}, []guest.State{guest.Ready, guest.Stopped}, ""},
		{"comma", []string{"ready,stopped"}, []guest.State{guest.Ready, guest.Stopped}, ""},
		{"case and space", []string{" READY , stopped "}, []guest.State{guest.Ready, guest.Stopped}, ""},
		{"deduped", []string{"ready", "ready"}, []guest.State{guest.Ready}, ""},
		{"empty pieces", []string{"", " , "}, nil, ""},
		{"unknown", []string{"bogus"}, nil, "--state takes"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseStates(c.in)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Errorf("parseStates(%v) = %v, want error %q", c.in, err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("parseStates(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestVersionMatches(t *testing.T) {
	for _, c := range []struct {
		want, have string
		ok         bool
	}{
		{"4.22.18", "4.22.18", true},
		{"4.22", "4.22.18", true},
		{"4.21", "4.22.18", false},
		{"4.22.18", "4.22.19", false},
		{"4.22", "", false},
		{"", "4.22.18", true},
	} {
		if got := versionMatches(c.want, c.have); got != c.ok {
			t.Errorf("versionMatches(%q, %q) = %v, want %v", c.want, c.have, got, c.ok)
		}
	}
}

func TestRenderListFilteredEmpty(t *testing.T) {
	l := &listing{Namespace: "vsno", Filter: &listFilter{Names: []string{"ghost"}}}
	var b bytes.Buffer
	if err := renderList(&b, l, time.Now(), plain); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "No vSNOs match name ghost in vsno.") {
		t.Errorf("filtered empty:\n%s", out)
	}
	if strings.Contains(out, "Create one") || strings.Contains(out, "Next:") {
		t.Errorf("filtered empty printed a hint or Next:\n%s", out)
	}
}

func TestRenderListJSONFilter(t *testing.T) {
	outputFormat = formatJSON
	t.Cleanup(func() { outputFormat = formatTable })
	l := &listing{Namespace: "vsno", Filter: &listFilter{States: []guest.State{guest.Ready}}, VSNOs: filterWorld()[:1], Next: "next"}
	var b bytes.Buffer
	if err := renderList(&b, l, time.Now(), plain); err != nil {
		t.Fatal(err)
	}
	var got struct {
		VSNOs  []guest.Guest `json:"vsnos"`
		Filter *listFilter   `json:"filter"`
		Next   string        `json:"next"`
	}
	if err := json.Unmarshal(b.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.VSNOs) != 1 || got.VSNOs[0].Name != "a" || got.Filter == nil ||
		len(got.Filter.States) != 1 || got.Next != "next" {
		t.Errorf("json = %+v", got)
	}
}

func TestListFilterNextIsWholeEnvironment(t *testing.T) {
	l := &listing{VSNOs: filterWorld()}
	l.filter(listFilter{States: []guest.State{guest.Stopped}})
	if len(l.VSNOs) != 1 || l.VSNOs[0].Name != "b" {
		t.Fatalf("filtered = %+v", l.VSNOs)
	}
	if l.Next == "" {
		t.Error("Next is about the whole environment and should still be set")
	}
}

func TestListFilterFrom(t *testing.T) {
	f, err := listFilterFrom([]string{" a ", "b"}, []string{"ready,stopped"}, " 4.22 ")
	if err != nil {
		t.Fatal(err)
	}
	want := listFilter{Names: []string{"a", "b"}, States: []guest.State{guest.Ready, guest.Stopped}, Version: "4.22"}
	if !reflect.DeepEqual(f, want) {
		t.Errorf("listFilterFrom = %+v, want %+v", f, want)
	}
	if _, err := listFilterFrom(nil, []string{"bogus"}, ""); err == nil {
		t.Error("listFilterFrom accepted an unknown state")
	}
	if f, err := listFilterFrom(nil, nil, ""); err != nil || !f.empty() {
		t.Errorf("no filter = %+v, %v; want empty", f, err)
	}
}

func TestListFilterString(t *testing.T) {
	for _, c := range []struct {
		name string
		f    listFilter
		want string
	}{
		{"empty", listFilter{}, ""},
		{"names", listFilter{Names: []string{"a", "b"}}, "name a, b"},
		{"states", listFilter{States: []guest.State{guest.Ready, guest.Stopped}}, "state ready, stopped"},
		{"version", listFilter{Version: "4.22"}, "version 4.22"},
		{"all", listFilter{Names: []string{"a"}, States: []guest.State{guest.Ready}, Version: "4.22"},
			"name a, state ready, version 4.22"},
	} {
		if got := c.f.String(); got != c.want {
			t.Errorf("%s: String = %q, want %q", c.name, got, c.want)
		}
	}
}
