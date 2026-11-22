package cmd

import (
	"testing"

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
