package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/sebrandon1/go-toolbox/confkit"
	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/kube"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"k8s.io/client-go/tools/remotecommand"
)

var shotFile string

var kubeconfigCmd = &cobra.Command{
	Use:   "kubeconfig <name>",
	Short: "Print a vSNO's admin kubeconfig",
	Long: `kubeconfig prints the vSNO's admin kubeconfig. Its server,
https://api.<name>.vsno.lab:6443, resolves only inside the environment (through
its dnsmasq), so use it from a pod there, or use vsno oc from anywhere.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		env, err := accessEnv(cmd.Context())
		if err != nil {
			return err
		}
		return guest.Kubeconfig(cmd.Context(), env, namespace, args[0], cmd.OutOrStdout(), cmd.ErrOrStderr())
	},
}

var ocCmd = &cobra.Command{
	Use:   "oc <name> [oc arguments...]",
	Short: "Run oc against a vSNO",
	Long: `oc runs oc with the given arguments against the vSNO, as kubeadmin, from the
builder pod. Flags after the name go to oc, for example:

  vsno oc seed get nodes -o wide
  vsno oc seed -- get clusterversion`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAccess(cmd, args, guest.Oc)
	},
}

var sshCmd = &cobra.Command{
	Use:   "ssh <name> [command...]",
	Short: "SSH to a vSNO as core",
	Long: `ssh opens a shell on the vSNO as user core (or runs the command), from the
builder pod with the environment's SSH key.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAccess(cmd, args, guest.SSH)
	},
}

var screenshotCmd = &cobra.Command{
	Use:   "screenshot <name>",
	Short: "Save a PNG of a vSNO's console",
	Long: `screenshot saves the vSNO's VGA console as a PNG: useful when the guest is
unreachable, for example stuck in emergency mode after a reboot.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		env, err := accessEnv(cmd.Context())
		if err != nil {
			return err
		}
		png, err := guest.Screenshot(cmd.Context(), env, namespace, args[0])
		if err != nil {
			return err
		}
		file := shotFile
		if file == "" {
			file = args[0] + ".png"
		}
		if err := confkit.AtomicWriteFile(file, png, 0o600); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", file)
		return nil
	},
}

func init() {
	for _, c := range []*cobra.Command{ocCmd, sshCmd} {
		c.Flags().SetInterspersed(false) // everything after the name belongs to oc / ssh
	}
	screenshotCmd.Flags().StringVarP(&shotFile, "file", "f", "", "output file (default <name>.png)")
	rootCmd.AddCommand(kubeconfigCmd, ocCmd, sshCmd, screenshotCmd)
}

func accessEnv(ctx context.Context) (guest.Env, error) {
	c, err := kube.New(kubeconfig, kubeContext)
	if err != nil {
		return guest.Env{}, err
	}
	return guestEnv(ctx, c, 0)
}

// runAccess runs an oc or ssh helper with the terminal attached. A terminal
// on stdin gets a TTY (an interactive ssh shell needs one).
func runAccess(cmd *cobra.Command, args []string, fn func(context.Context, guest.Env, string, string, []string, builder.IO) error) error {
	env, err := accessEnv(cmd.Context())
	if err != nil {
		return err
	}
	rest := args[1:]
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	s := builder.IO{Stdin: os.Stdin, Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr()}
	fd := int(os.Stdin.Fd()) // #nosec G115 -- fd fits in int
	if term.IsTerminal(fd) {
		w, h, err := term.GetSize(fd)
		if err == nil {
			s.TTY, s.Resize = true, &fixedSize{size: &remotecommand.TerminalSize{Width: uint16(w), Height: uint16(h)}} // #nosec G115 -- terminal sizes fit
		}
		old, err := term.MakeRaw(fd)
		if err == nil {
			defer func() { _ = term.Restore(fd, old) }()
		}
	}
	return fn(cmd.Context(), env, namespace, args[0], rest, s)
}

// fixedSize reports the terminal size once.
type fixedSize struct{ size *remotecommand.TerminalSize }

func (f *fixedSize) Next() *remotecommand.TerminalSize {
	s := f.size
	f.size = nil
	return s
}
