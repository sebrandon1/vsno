// Package cmd implements the vsno command line.
package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/sebrandon1/go-toolbox/cliout"
	"github.com/sebrandon1/go-toolbox/cobrautil"

	"io"
	"os"

	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/sebrandon1/vsno/lib/ui"

	"github.com/spf13/cobra"
)

// Output formats for -o.
const (
	formatTable = "table"
	formatWide  = "wide"
	formatJSON  = "json"
)

var (
	cliVersion   = "dev"
	kubeconfig   string
	kubeContext  string
	namespace    string
	outputFormat string
	noColor      bool
	verbose      bool
)

var rootCmd = &cobra.Command{
	Use:   "vsno",
	Short: "Create and manage virtual SNOs inside an existing OpenShift cluster",
	Long: `vsno creates, lists and deletes virtual single-node OpenShift clusters (vSNOs)
as OpenShift Virtualization VMs on a bare-metal worker of a cluster you already
have. It can also drive a lifecycle-agent Image Based Upgrade (IBU) between two
of them.

Start with:  vsno precheck`,
	SilenceUsage: true,
	PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
		if noColor {
			// One switch for everything: the tables and the progress on stderr.
			if err := os.Setenv("NO_COLOR", "1"); err != nil {
				return err
			}
		}
		if err := cliout.ValidateFormat(outputFormat, formatTable, formatWide, formatJSON); err != nil {
			return err
		}
		outputFormat = strings.ToLower(outputFormat)
		if err := checkWide(cmd); err != nil {
			return err
		}
		if err := checkJSON(cmd); err != nil {
			return err
		}
		if verbose {
			trace.Enable(cmd.ErrOrStderr())
		} else {
			trace.Disable()
		}
		return nil
	},
}

func init() {
	pf := rootCmd.PersistentFlags()
	pf.StringVar(&kubeconfig, "kubeconfig", "", "path to the host cluster kubeconfig (default: $KUBECONFIG, then ~/.kube/config)")
	pf.StringVar(&kubeContext, "context", "", "kubeconfig context to use")
	pf.StringVarP(&namespace, "namespace", "n", "vsno", "namespace that holds the vSNO environment")
	pf.StringVarP(&outputFormat, "output", "o", formatTable, "output format: table, wide or json")
	pf.BoolVar(&noColor, "no-color", false, "don't color the output (also: NO_COLOR=1)")
	// -v is verbose, so cobra registers --version without a shorthand.
	pf.BoolVarP(&verbose, "verbose", "v", false, "trace the cluster requests and builder commands on stderr")
}

// commandsWithWide have columns beyond the default view. Elsewhere wide would be
// table under another name, so it is refused rather than silently ignored.
var commandsWithWide = map[string]bool{
	"vsno list":     true,
	"vsno iso":      true, // iso alone lists
	"vsno iso list": true,
}

// checkWide refuses -o wide on a command that has no wide view.
func checkWide(cmd *cobra.Command) error {
	if outputFormat != formatWide || commandsWithWide[cmd.CommandPath()] {
		return nil
	}
	return fmt.Errorf("%s has no wide output; use -o table or -o json", cmd.CommandPath())
}

// commandsWithoutJSON hand over a stream (oc, ssh, a kubeconfig, a PNG) or only
// say they are done, so -o json would be silently ignored. Refusing says so.
var commandsWithoutJSON = map[string]bool{
	"vsno oc":         true,
	"vsno ssh":        true,
	"vsno kubeconfig": true,
	"vsno screenshot": true,
	"vsno iso eject":  true,
	"vsno cache gc":   true,
}

// checkJSON refuses -o json where the command has nothing to put in it.
func checkJSON(cmd *cobra.Command) error {
	if outputFormat != formatJSON || !commandsWithoutJSON[cmd.CommandPath()] {
		return nil
	}
	return fmt.Errorf("%s has no json output; drop -o json", cmd.CommandPath())
}

// palette colors table output written to a terminal.
func palette(w io.Writer) ui.Palette {
	return ui.Palette{On: outputFormat != formatJSON && ui.ColorEnabled(w)}
}

// SetVersion is called from main with the build-time version.
func SetVersion(v string) {
	cliVersion = v
	cobrautil.SetVersion(rootCmd, v)
}

// Execute runs the root command. Ctrl+C or SIGTERM cancels the command's
// context, so a command stopped mid-wait still cleans up after itself (the LCA
// build pod, say) rather than dying where it stands.
func Execute() error {
	// The last collapsed count would otherwise be lost on the way out.
	defer trace.Flush()
	return cobrautil.Execute(rootCmd)
}

// useEnvironment picks the environment for commands that work in one, when -n
// isn't given: the namespace vsno setup labeled, if there is exactly one (the
// default name may hold something else, such as an environment built by
// hand). With none, the default stays; with several, the user must choose.
func useEnvironment(ctx context.Context, kube kubernetes.Interface) error {
	if rootCmd.PersistentFlags().Changed("namespace") {
		return nil
	}
	l, err := kube.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: cluster.ManagedByLabel + "=vsno"})
	if err != nil {
		return nil //nolint:nilerr // keep the default; the command reports access problems itself
	}
	names := make([]string, 0, len(l.Items))
	for _, n := range l.Items {
		names = append(names, n.Name)
	}
	switch {
	case slices.Contains(names, namespace):
	case len(names) == 1:
		namespace = names[0]
	case len(names) > 1:
		slices.Sort(names)
		return fmt.Errorf("there are several vsno environments (%s); pick one with -n", strings.Join(names, ", "))
	}
	return nil
}

// envNote is the line a command that acts on one environment prints so the
// reader knows of the others: two environments can exist, and an output that
// silently describes one of them reads as describing whichever the reader had
// in mind. Empty when ns is the only one, or the list fails.
func envNote(ctx context.Context, kube kubernetes.Interface, ns string) string {
	l, err := kube.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: cluster.ManagedByLabel + "=vsno"})
	if err != nil {
		return ""
	}
	var others []string
	for _, n := range l.Items {
		if n.Name != ns {
			others = append(others, n.Name)
		}
	}
	if len(others) == 0 {
		return ""
	}
	slices.Sort(others)
	return fmt.Sprintf("note: %s is one of several vsno environments (also: %s); pass -n to choose another\n", ns, strings.Join(others, ", "))
}
