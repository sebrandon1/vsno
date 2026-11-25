package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/sebrandon1/vsno/lib/cache"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/kube"
	"github.com/sebrandon1/vsno/lib/reconcile"
	"github.com/sebrandon1/vsno/lib/registry"
	"github.com/sebrandon1/vsno/lib/seed"
	"github.com/sebrandon1/vsno/lib/ui"
	"github.com/spf13/cobra"
)

var (
	rcPrune   bool
	rcDryRun  bool
	rcGrace   time.Duration
	rcTimeout time.Duration
)

var reconcileCmd = &cobra.Command{
	Use:   "reconcile [<vsno>...]",
	Short: "Settle unfinished bookkeeping and find what a dead create left",
	Long: `reconcile sweeps an environment for the states nothing else watches:

  - releases whose mirror finished while no command was looking
  - Services left by a create that died: no VM, and older than any ISO build
  - dnsmasq entries for names that are neither a VM nor a Service
  - vsno.io/seeding marks whose generation the Lifecycle Agent has ended
  - the environment's own checks: a Released volume, a full registry

By default it only reports, and it never asks a question, so it is safe to run
from cron. --prune performs the removals; it refuses to touch a builder-side
artifact while the builder is not ready. It exits 0 whenever the sweep ran,
whether or not it found anything, and non-zero only when a read failed or a
--prune action did.

  */5 * * * * vsno -n vsno reconcile --prune -o json >> /var/log/vsno-reconcile.log 2>&1`,
	Args:              cobra.ArbitraryArgs,
	ValidArgsFunction: completeVSNOs,
	RunE:              runReconcile,
}

func init() {
	f := reconcileCmd.Flags()
	f.BoolVar(&rcPrune, "prune", false, "remove what is orphaned; without it the sweep only reports")
	f.BoolVar(&rcDryRun, "dry-run", false, "report only (the default), and an error with --prune")
	f.DurationVar(&rcGrace, "grace", guest.AbandonGrace(), "how long a Service may wait for its VM before it is abandoned")
	f.DurationVar(&rcTimeout, "timeout", 5*time.Minute, "limit for the whole sweep")
	rootCmd.AddCommand(reconcileCmd)
}

func runReconcile(cmd *cobra.Command, args []string) error {
	if rcPrune && rcDryRun {
		return fmt.Errorf("--prune and --dry-run contradict each other")
	}
	env, err := reconcileEnv(cmd)
	if err != nil {
		return err
	}
	rep, err := reconcile.Sweep(cmd.Context(), env, reconcile.Options{
		Namespace: namespace, Names: args, Prune: rcPrune, Grace: rcGrace, Timeout: rcTimeout,
	})
	if err != nil {
		return err
	}
	if err := renderReconcile(cmd, rep); err != nil {
		return err
	}
	if rep.Failed() {
		return fmt.Errorf("the sweep could not finish everything it planned; see above")
	}
	return nil
}

// reconcileEnv wires one environment to the sweep: the guest/session side for
// Services, disks and access, and the cache side for mirror records.
func reconcileEnv(cmd *cobra.Command) (reconcile.Env, error) {
	c, err := kube.New(kubeconfig, kubeContext)
	if err != nil {
		return reconcile.Env{}, err
	}
	if err := useEnvironment(cmd.Context(), c.Kube); err != nil {
		return reconcile.Env{}, err
	}
	genv, err := guestEnvFor(c, namespace, 0)
	if err != nil {
		return reconcile.Env{}, err
	}
	senv := seed.Env{Env: genv, Volume: func(ctx context.Context, ns string) (int64, int64, error) {
		return registry.Usage(ctx, c.Config, c.Kube, ns)
	}}
	return reconcile.Env{
		Env:   senv,
		Cache: cache.Env{Kube: c.Kube, Dyn: c.Dyn, Config: c.Config, Builder: genv.Builder},
	}, nil
}

// renderReconcile prints the sweep. A healthy environment prints nothing: a
// cron job's log should be empty until something needs attention.
func renderReconcile(cmd *cobra.Command, rep *reconcile.Report) error {
	w, pal := cmd.OutOrStdout(), palette(cmd.OutOrStdout())
	if outputFormat == formatJSON {
		return writeJSON(w, rep)
	}
	if len(rep.Items) == 0 {
		return nil
	}
	var t ui.Table
	t.Row(pal.Bold("KIND"), pal.Bold("NAME"), pal.Bold("STATUS"), pal.Bold("DETAIL"), pal.Bold("ACTION"))
	for _, it := range rep.Items {
		t.Row(it.Kind, pal.Bold(it.Name), doctorStatus(pal, it.Status), it.Detail, actionLabel(pal, it))
	}
	t.Write(w)
	return nil
}

// actionLabel is the past tense of what was done; an action that was only
// planned stays in the present.
func actionLabel(pal ui.Palette, it reconcile.Item) string {
	if !it.Done {
		return pal.Dim(string(it.Action))
	}
	switch it.Action {
	case reconcile.Delete:
		return pal.Green("deleted")
	case reconcile.Settle:
		return pal.Green("settled")
	}
	return pal.Green(string(it.Action))
}
