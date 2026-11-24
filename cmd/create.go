package cmd

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/sebrandon1/go-toolbox/cliout"
	"github.com/sebrandon1/vsno/lib/apply"
	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/cache"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/kube"
	"github.com/sebrandon1/vsno/lib/release"
	"github.com/sebrandon1/vsno/lib/snapshot"
	"github.com/sebrandon1/vsno/lib/ui"
	"github.com/spf13/cobra"
	"k8s.io/client-go/discovery"
)

var (
	crVersion string
	crNode    string
	crCores   int
	crMemory  int64
	crDisk    int64
	crWatch   bool
	crNoCache bool
	crTimeout time.Duration
	// crFromSnapshot restores the root disk from the vSNO's snapshot.
	crFromSnapshot bool
)

var createCmd = &cobra.Command{
	Use:   "create [name] --version <version>",
	Short: "Create a vSNO",
	Long: `create installs a single-node OpenShift cluster in a VM in the environment
(vsno setup must have run). It resolves --version to a release image, builds
the agent ISO in the builder pod, creates the VM, its disks, a Service and DNS
entries. The install then takes about 45 minutes: --watch follows it with
progress until the cluster is ready; otherwise follow it with vsno list --watch.

Without -n, vSNO commands use the environment vsno setup created, when there
is exactly one.

Without a name, it picks a free random one such as brave-otter.

--version accepts ` + release.Forms + `.
Nightlies need credentials for ` + release.CIRegistry + ` in the cluster pull secret.

The vSNO's API is api.<name>.vsno.lab, reached through the builder pod:
see vsno oc, vsno ssh and vsno kubeconfig.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runCreate,
}

func init() {
	f := createCmd.Flags()
	f.StringVar(&crVersion, "version", "", "OpenShift version to install (required unless --from-snapshot)")
	f.StringVar(&crNode, "node", "", "worker to run on (default: the node vsno setup chose)")
	f.IntVar(&crCores, "cores", guest.DefaultCores, "vCPUs")
	f.Int64Var(&crMemory, "memory", guest.DefaultMemoryGiB, "memory, GiB")
	f.Int64Var(&crDisk, "disk", guest.DefaultDiskGiB, "root disk, GiB")
	f.BoolVar(&crNoCache, "no-cache", false, "install from the internet instead of the environment registry's mirror (the vSNO still trusts its CA)")
	f.BoolVar(&crFromSnapshot, "from-snapshot", false, "restore the disk from the vSNO's snapshot (see vsno snapshot) instead of installing")
	f.BoolVarP(&crWatch, "watch", "w", false, "follow the install until the vSNO is ready")
	f.DurationVar(&crTimeout, "timeout", 90*time.Minute, "how long to wait for the install")
	// --version is required unless the disk comes from a snapshot, which brings
	// its own; runCreate checks.
	rootCmd.AddCommand(createCmd)
}

// guestEnv wires the guest package to the cluster, in the environment
// useEnvironment picks.
func guestEnv(ctx context.Context, c *kube.Clients, timeout time.Duration) (guest.Env, error) {
	if err := useEnvironment(ctx, c.Kube); err != nil {
		return guest.Env{}, err
	}
	return guestEnvFor(c, namespace, timeout)
}

// guestEnvFor wires the guest package to one environment's namespace.
func guestEnvFor(c *kube.Clients, ns string, timeout time.Duration) (guest.Env, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(c.Config)
	if err != nil {
		return guest.Env{}, err
	}
	be := builder.Exec{Config: c.Config, Kube: c.Kube, Namespace: ns}
	env := guest.Env{
		Kube: c.Kube, Dyn: c.Dyn, Applier: apply.NewSSA(dc, c.Dyn),
		Builder:  be,
		Resolver: release.Resolver{},
		Progress: precheckProgress{newProgress()},
		Timeout:  timeout,
	}
	env.Cache = cacheClient{env: cache.Env{Kube: c.Kube, Dyn: c.Dyn, Config: c.Config, Builder: be}}
	return env, nil
}

// cacheClient adapts the cache package to the guest package's interface.
type cacheClient struct{ env cache.Env }

func (c cacheClient) Ensure(ctx context.Context, ns string, rel release.Release, onLine func(string)) (*cache.Prepared, error) {
	return cache.Ensure(ctx, c.env, cache.EnsureOptions{Namespace: ns, Release: rel, OnLine: onLine})
}

func runCreate(cmd *cobra.Command, args []string) error {
	c, err := kube.New(kubeconfig, kubeContext)
	if err != nil {
		return err
	}
	env, err := guestEnv(cmd.Context(), c, crTimeout)
	if err != nil {
		return err
	}
	name := ""
	if len(args) == 1 {
		name = args[0]
	} else if !crFromSnapshot {
		if name, err = guest.FreeName(cmd.Context(), env, namespace); err != nil {
			return err
		}
	}
	if err := checkCreateArgs(name, crFromSnapshot, crVersion); err != nil {
		return err
	}
	if outputFormat != formatJSON {
		fmt.Fprint(cmd.ErrOrStderr(), envNote(cmd.Context(), c.Kube, namespace))
		fmt.Fprintf(cmd.ErrOrStderr(), "Creating vSNO %s in %s on %s (context %q)...\n\n", name, namespace, c.Config.Host, c.Context)
	}
	snap, err := restoreFrom(cmd, name)
	if err != nil {
		return err
	}
	g, err := guest.Create(cmd.Context(), env, guest.CreateOptions{
		Namespace: namespace, Name: name, Version: crVersion, Node: crNode,
		Cores: crCores, MemoryGiB: crMemory, DiskGiB: crDisk, Wait: crWatch, NoCache: crNoCache,
		Snapshot: snap,
	})
	if g == nil || err != nil {
		return err
	}
	if rerr := renderCreated(cmd.OutOrStdout(), g, crWatch, palette(cmd.OutOrStdout())); rerr != nil {
		return rerr
	}
	return nil
}

// checkCreateArgs is the rule for what create needs: an install takes a version,
// a restore takes the snapshot's own name instead.
func checkCreateArgs(name string, fromSnapshot bool, version string) error {
	if fromSnapshot && name == "" {
		return fmt.Errorf("--from-snapshot restores a vSNO under its own name; name it")
	}
	if !fromSnapshot && version == "" {
		return fmt.Errorf("--version is required to install a vSNO (or --from-snapshot to restore one)")
	}
	return nil
}

// restoreFrom is the snapshot --from-snapshot restores name from, or "" when the
// caller didn't ask for a restore.
func restoreFrom(cmd *cobra.Command, name string) (string, error) {
	if !crFromSnapshot {
		return "", nil
	}
	s, err := snapshotOf(cmd, name)
	if err != nil {
		return "", err
	}
	return restoreTarget(s, name)
}

// restoreTarget is the name a restore uses, or why there is nothing to restore.
func restoreTarget(s *snapshot.Entry, name string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("no snapshot named %s; keep one first with `vsno snapshot create %s`. "+
			"A snapshot restores only under the name of the vSNO it was taken from", name, name)
	}
	return s.Name, nil
}

func renderCreated(w io.Writer, g *guest.Guest, ready bool, pal ui.Palette) error {
	if outputFormat == formatJSON {
		return cliout.PrintJSON(w, g)
	}
	fmt.Fprintln(w)
	switch {
	case ready:
		fmt.Fprintf(w, "%s vSNO %s is ready: OpenShift %s, %s\n", pal.Icon("✓"), pal.Bold(g.Name), g.Version, g.API)
	default:
		fmt.Fprintf(w, "vSNO %s is installing in %s (about 45 minutes). Follow it with `vsno -n %s list --watch`.\n", pal.Bold(g.Name), namespace, namespace)
	}
	fmt.Fprintf(w, "Try: vsno oc %s -- get nodes | vsno ssh %s | vsno kubeconfig %s\n", g.Name, g.Name, g.Name)
	return nil
}
