package cmd

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/config"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCandidates(t *testing.T) {
	items := []string{"target", "seed", "seed", "", "builder"}
	for _, c := range []struct {
		name       string
		args       []string
		toComplete string
		want       []string
	}{
		{"all", nil, "", []string{"builder", "seed", "target"}},
		{"prefix", nil, "se", []string{"seed"}},
		{"already given", []string{"seed"}, "", []string{"builder", "target"}},
		{"none", nil, "zzz", []string{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := candidates(items, c.args, c.toComplete); !reflect.DeepEqual(got, c.want) {
				t.Errorf("candidates = %v, want %v", got, c.want)
			}
		})
	}
}

func TestPrefixOf(t *testing.T) {
	if got := prefixOf([]string{"table", "wide", "json"}, "j"); !reflect.DeepEqual(got, []string{"json"}) {
		t.Errorf("prefixOf = %v", got)
	}
}

func TestCompleteOutput(t *testing.T) {
	root := &cobra.Command{Use: "vsno"}
	status, list := &cobra.Command{Use: "status"}, &cobra.Command{Use: "list"}
	root.AddCommand(status, list)
	if got, _ := completeOutput(status, nil, ""); !reflect.DeepEqual(got, []string{formatTable, formatJSON}) {
		t.Errorf("status = %v, want no wide", got)
	}
	if got, _ := completeOutput(list, nil, ""); !reflect.DeepEqual(got, []string{formatTable, formatJSON, formatWide}) {
		t.Errorf("list = %v, want wide", got)
	}
}

func TestCompleteConfigKeys(t *testing.T) {
	want := make([]string, 0, len(config.Settings))
	for _, s := range config.Settings {
		want = append(want, string(s.Key))
	}
	if got, _ := completeConfigKeys(configSetCmd, nil, ""); !reflect.DeepEqual(got, want) {
		t.Errorf("keys = %v, want %v", got, want)
	}
	// A value for a path setting falls back to file completion.
	if _, d := completeConfigKeys(configSetCmd, []string{"seed.sync.ca"}, ""); d != cobra.ShellCompDirectiveDefault {
		t.Errorf("seed.sync.ca value directive = %v, want Default", d)
	}
	if _, d := completeConfigKeys(configSetCmd, []string{"namespace"}, ""); d != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("namespace value directive = %v, want NoFileComp", d)
	}
	// A second positional is past the value: nothing.
	if got, _ := completeConfigKeys(configSetCmd, []string{"namespace", "x"}, ""); len(got) != 0 {
		t.Errorf("after the value = %v, want none", got)
	}
}

func TestCompleteSwallowsErrors(t *testing.T) {
	got, d := completeFrom(completionReadTimeout, func(context.Context) ([]string, error) {
		return nil, errors.New("no cluster")
	}, nil, "")
	if len(got) != 0 || d != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("error = %v, %v; want nothing and NoFileComp", got, d)
	}
}

func TestCompletionNamespace(t *testing.T) {
	namespace = "vsno"
	t.Cleanup(func() { namespace = "vsno" })
	c := fakeClients(t)
	ctx := context.Background()

	if got := completionNamespace(ctx, c); got != "vsno" {
		t.Errorf("no environments = %q, want vsno", got)
	}

	// One labeled environment that is not the default: it is the answer.
	if _, err := c.Kube.CoreV1().Namespaces().Create(ctx,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "vsno-dev", Labels: map[string]string{cluster.ManagedByLabel: "vsno"}}},
		metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := completionNamespace(ctx, c); got != "vsno-dev" {
		t.Errorf("one environment = %q, want vsno-dev", got)
	}

	// The configured namespace wins over that.
	t.Setenv("VSNO_NAMESPACE", "vsno-dev")
	namespace = "vsno"
	if got := completionNamespace(ctx, c); got != "vsno-dev" {
		t.Errorf("configured = %q, want vsno-dev", got)
	}

	// Several environments, none the default: keep the default rather than
	// guess between them.
	if _, err := c.Kube.CoreV1().Namespaces().Create(ctx,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "vsno-other", Labels: map[string]string{cluster.ManagedByLabel: "vsno"}}},
		metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VSNO_NAMESPACE", "")
	namespace = "vsno"
	if got := completionNamespace(ctx, c); got != "vsno" {
		t.Errorf("several environments = %q, want the default vsno", got)
	}
}

// Every command that takes a vSNO name completes it.
func TestNameCommandsComplete(t *testing.T) {
	for _, c := range []*cobra.Command{
		deleteCmd, startCmd, stopCmd, ocCmd, sshCmd, kubeconfigCmd, screenshotCmd,
		prepareCmd, upgradeCmd, seedCreateCmd, seedPrecheckCmd, seedDeleteCmd,
		doctorCmd, snapshotCreateCmd, snapshotDeleteCmd, isoInfoCmd, isoEjectCmd,
		configGetCmd, configSetCmd, configUnsetCmd,
	} {
		if c.ValidArgsFunction == nil {
			t.Errorf("%s has no completion", c.CommandPath())
		}
	}
}

func TestCompleteFromSuccess(t *testing.T) {
	got, d := completeFrom(completionReadTimeout, func(context.Context) ([]string, error) {
		return []string{"seed", "target"}, nil
	}, nil, "s")
	if !reflect.DeepEqual(got, []string{"seed"}) || d != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("completeFrom = %v, %v; want [seed] and NoFileComp", got, d)
	}
}

// Only the first argument is a vSNO: the rest belongs to oc or ssh.
func TestCompleteVSNOFirst(t *testing.T) {
	got, d := completeVSNOFirst(nil, []string{"seed"}, "")
	if len(got) != 0 || d != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("after the name = %v, %v; want nothing", got, d)
	}
}

func TestCompleteStates(t *testing.T) {
	got, d := completeStates(nil, nil, "st")
	if !reflect.DeepEqual(got, []string{string(guest.Starting), string(guest.Stopped)}) ||
		d != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("completeStates = %v, %v", got, d)
	}
}
