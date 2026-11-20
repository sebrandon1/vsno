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
