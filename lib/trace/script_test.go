package trace

import (
	"os"
	"strings"
	"testing"
)

func readScript(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The scripts vsno ships are the real input: what a --verbose user sees for
// them is what this asserts.
func TestScriptCommandsShippedScripts(t *testing.T) {
	t.Run("probe.sh", func(t *testing.T) {
		got := ScriptCommands(readScript(t, "../guest/scripts/probe.sh"), []string{"vsno.lab", "seed=10.0.0.5"})
		for _, l := range got {
			t.Logf("→ %s", l)
		}
		if len(got) != 4 {
			t.Fatalf("got %d commands, want 4", len(got))
		}
		if !strings.HasPrefix(got[0], "curl -sf -m 5 ") {
			t.Errorf("first = %q, want the agent installer check", got[0])
		}
		for i, want := range []string{"get clusterversion version", "get clusteroperators", "get imagebasedupgrades upgrade"} {
			if !strings.Contains(got[i+1], want) {
				t.Errorf("%d = %q, want it to contain %q", i+1, got[i+1], want)
			}
			if !strings.Contains(got[i+1], `--tls-server-name "api.$name.vsno.lab"`) {
				t.Errorf("%d = %q, want the array prefix resolved", i+1, got[i+1])
			}
		}
	})

	t.Run("mirror.sh", func(t *testing.T) {
		got := ScriptCommands(readScript(t, "../cache/scripts/mirror.sh"),
			[]string{"quay.io/openshift-release-dev/ocp-release:4.21.1-x86_64", "registry.lab:5000", "ocp", "4.21.1"})
		if len(got) != 1 {
			t.Fatalf("got %d commands, want 1: %q", len(got), got)
		}
		want := `oc adm release mirror --registry-config "$dir/auth.json" --from "quay.io/openshift-release-dev/ocp-release:4.21.1-x86_64" --to "registry.lab:5000/ocp"`
		if !strings.HasPrefix(got[0], want) {
			t.Errorf("got %q, want it to start %q", got[0], want)
		}
		if strings.Contains(got[0], "\n") {
			t.Error("continuations were not joined")
		}
	})

	t.Run("rm.sh", func(t *testing.T) {
		got := ScriptCommands(readScript(t, "../guest/scripts/rm.sh"), []string{"seed"})
		want := []string{
			`rm -f "/work/www/$name.iso" "/work/www/$name.iso.tmp"`,
			`rm -f "/work/guests/$name"/build/agent.*.iso`,
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

func TestScriptCommands(t *testing.T) {
	for _, c := range []struct {
		desc, script string
		args         []string
		want         []string
	}{
		{
			desc:   "continuations join",
			script: "oc adm release info \\\n  --registry-config /tmp/auth.json \\\n  quay.io/x:1\n",
			want:   []string{"oc adm release info --registry-config /tmp/auth.json quay.io/x:1"},
		},
		{
			desc:   "positional arguments and variables",
			script: "name=$1 ip=$2\nshift 2\nssh -i /key \"core@$ip\" systemctl is-active kubelet\n",
			args:   []string{"seed", "10.0.0.5", "extra"},
			want:   []string{`ssh -i /key "core@10.0.0.5" systemctl is-active kubelet`},
		},
		{
			desc:   "a command substitution is cut at its redirect",
			script: "v=$(oc get clusterversion version -o json 2>/dev/null | jq -r .x)\n",
			want:   []string{"oc get clusterversion version -o json"},
		},
		{
			desc:   "array uses read as the array's command",
			script: "o=(oc --kubeconfig /k get nodes)\nx=$(\"${o[@]}\" -o name)\n",
			want:   []string{"oc --kubeconfig /k get nodes -o name"},
		},
		{
			desc:   "operators inside quotes are kept",
			script: "skopeo copy --src-creds 'a|b' docker://x docker://y\n",
			want:   []string{"skopeo copy --src-creds 'a|b' docker://x docker://y"},
		},
		{
			desc:   "duplicates collapse in order",
			script: "oc get pods\noc get pods\noc get nodes\n",
			want:   []string{"oc get pods", "oc get nodes"},
		},
		{
			desc:   "a script with no command yields nothing",
			script: "set -euo pipefail\ntrue\n",
			want:   nil,
		},
		{
			desc:   "comments are dropped",
			script: "# oc get secrets\noc get pods\n",
			want:   []string{"oc get pods"},
		},
	} {
		if got := ScriptCommands(c.script, c.args); strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s: got %q, want %q", c.desc, got, c.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	long := "oc " + strings.Repeat("x", 400)
	got := truncate(long)
	if n := len([]rune(got)); n != maxLine {
		t.Errorf("truncate kept %d runes, want %d", n, maxLine)
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("truncate lost its ellipsis")
	}
	if short := "oc get pods"; truncate(short) != short {
		t.Error("truncate changed a short line")
	}
}

// registry.sh dispatches on its operation argument. Showing every branch would
// make a list look like it deletes things.
func TestScriptCommandsRegistryDispatch(t *testing.T) {
	const host = "registry.vsno.svc.cluster.local:5000"
	script := readScript(t, "../registry/scripts/registry.sh")

	t.Run("list", func(t *testing.T) {
		got := ScriptCommands(script, []string{host, "list", "seeds/"})
		for _, l := range got {
			t.Logf("→ %s", l)
			if strings.Contains(l, "delete") || strings.Contains(l, "DELETE") {
				t.Errorf("%q: a list must not show a delete", l)
			}
		}
		if len(got) != 3 {
			t.Fatalf("got %d commands, want 3: %q", len(got), got)
		}
		if !strings.HasPrefix(got[0], "curl -sSf -K") || !strings.Contains(got[0], "/v2/_catalog") {
			t.Errorf("first = %q, want the catalog read", got[0])
		}
		if !strings.HasPrefix(got[1], "skopeo list-tags ") || !strings.HasPrefix(got[2], "skopeo inspect ") {
			t.Errorf("got %q, want list-tags then inspect", got[1:])
		}
	})

	t.Run("delete", func(t *testing.T) {
		got := ScriptCommands(script, []string{host, "delete", "seeds/seed:4.22.22"})
		if len(got) != 1 || !strings.HasPrefix(got[0], "skopeo delete ") {
			t.Fatalf("got %q, want one skopeo delete", got)
		}
		if strings.Contains(got[0], "_catalog") {
			t.Errorf("%q: a delete must not show the catalog read", got[0])
		}
	})

	t.Run("unknown operation runs the default branch", func(t *testing.T) {
		for _, l := range ScriptCommands(script, []string{host, "bogus"}) {
			if strings.Contains(l, "_catalog") || strings.Contains(l, "skopeo") {
				t.Errorf("%q: no named branch runs for an unknown operation", l)
			}
		}
	})
}

func TestMatchBranch(t *testing.T) {
	for _, c := range []struct {
		pat, value string
		matched    bool
		want       bool
	}{
		{"list", "list", false, true},
		{"list", "delete", false, false},
		{"202|404", "404", false, true},
		{"202|404", "500", false, false},
		{"*", "anything", false, true},
		{"*", "list", true, false}, // "*)" is the default, not a catch-all
		{`"quoted"`, "quoted", false, true},
	} {
		if got := matchBranch(c.pat, c.value, c.matched); got != c.want {
			t.Errorf("matchBranch(%q, %q, matched=%v) = %v, want %v", c.pat, c.value, c.matched, got, c.want)
		}
	}
}
