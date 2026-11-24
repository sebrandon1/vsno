package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/kube"
	"github.com/sebrandon1/vsno/lib/ui"
	"github.com/spf13/cobra"
)

var (
	wtFor     string
	wtTimeout time.Duration
)

var waitCmd = &cobra.Command{
	Use:   "wait <name>",
	Short: "Wait for a vSNO to reach a state",
	Long: `wait polls until the vSNO is ready (the default), stopped or failed, then
exits 0. It is what a script should use instead of watching the table, since
vsno create can return before the install is done:

  vsno create target --version 4.22
  vsno wait target --for ready --timeout 45m

A vSNO still building its ISO is followed too: until its VM exists, the
Service create adds first says that it is building.

--for failed returns as soon as the vSNO is clearly not failed either, and
exits non-zero saying so, rather than waiting out the timeout.`,
	Args:              oneArg("the vSNO, as `vsno list` shows it"),
	ValidArgsFunction: completeVSNOFirst,
	RunE:              trimmed(runWait),
}

func init() {
	f := waitCmd.Flags()
	f.StringVar(&wtFor, "for", string(guest.Ready), "state to wait for: ready, stopped or failed")
	f.DurationVar(&wtTimeout, "timeout", 90*time.Minute, "how long to wait")
	rootCmd.AddCommand(waitCmd)
}

// waitResult is what wait reports, and what -o json prints.
type waitResult struct {
	VSNO   string      `json:"vsno"`
	For    guest.State `json:"for"`
	State  guest.State `json:"state"`
	Detail string      `json:"detail,omitempty"`
}

func runWait(cmd *cobra.Command, args []string) error {
	want, err := waitTarget(wtFor)
	if err != nil {
		return err
	}
	c, err := kube.New(kubeconfig, kubeContext)
	if err != nil {
		return err
	}
	env, err := guestEnv(cmd.Context(), c, wtTimeout)
	if err != nil {
		return err
	}
	name := args[0]
	if err := waitExists(cmd.Context(), c, name); err != nil {
		return err
	}
	got, detail, err := guest.WaitStateDetail(cmd.Context(), env, namespace, name, waitAppetite(want)...)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("vSNO %s did not reach %s within %s (last: %s %s)", name, want, wtTimeout, got, detail)
	case err != nil:
		return err
	}
	res := waitResult{VSNO: name, For: want, State: got, Detail: detail}
	w := cmd.OutOrStdout()
	if outputFormat == formatJSON {
		if err := writeJSON(w, res); err != nil {
			return err
		}
	} else {
		waitRender(w, res, palette(w))
	}
	return waitStatus(res)
}

// waitTarget is the state --for names.
func waitTarget(s string) (guest.State, error) {
	switch st := guest.State(strings.ToLower(strings.TrimSpace(s))); st {
	case guest.Ready, guest.Stopped, guest.Failed:
		return st, nil
	}
	return "", fmt.Errorf("--for takes ready, stopped or failed, not %q", s)
}

// waitAppetite is when to stop polling: the state asked for, and a failure,
// which no wait should sit through. Waiting for failure stops on the states
// that mean it is not failing either.
func waitAppetite(want guest.State) []guest.State {
	out := []guest.State{want, guest.Failed, guest.Abandoned}
	if want == guest.Failed {
		out = append(out, guest.Ready, guest.Stopped)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// waitExists fails fast on a name that is not there, rather than waiting out
// the timeout for it. An unreadable cluster is left to the wait to report.
func waitExists(ctx context.Context, c *kube.Clients, name string) error {
	names, err := guest.Names(ctx, c.Kube, c.Dyn, namespace)
	if err != nil {
		return nil //nolint:nilerr // the wait reports the cluster's problem itself
	}
	if !slices.Contains(names, name) {
		return noVSNO(name, namespace)
	}
	return nil
}

// waitRender prints the success line; a wait that ended in the wrong state is
// reported by waitStatus.
func waitRender(w io.Writer, res waitResult, pal ui.Palette) {
	if res.State != res.For {
		return
	}
	fmt.Fprintf(w, "%s vSNO %s is %s", pal.Icon("✓"), pal.Bold(res.VSNO), res.State)
	if res.Detail != "" {
		fmt.Fprintf(w, " — %s", pal.Dim(res.Detail))
	}
	fmt.Fprintln(w)
}

// waitStatus is the exit status: nil only when the vSNO reached the state
// asked for.
func waitStatus(res waitResult) error {
	if res.State == res.For {
		return nil
	}
	switch res.State {
	case guest.Failed, guest.Abandoned:
		return fmt.Errorf("vSNO %s is %s: %s", res.VSNO, res.State, res.Detail)
	}
	return fmt.Errorf("vSNO %s is %s, not %s", res.VSNO, res.State, res.For)
}
