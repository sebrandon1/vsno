package cmd

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sebrandon1/vsno/lib/cache"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/kube"
	"github.com/sebrandon1/vsno/lib/ui"
	"github.com/spf13/cobra"
)

var cacheCmd = &cobra.Command{
	Use:   "cache",
	Short: "Manage the OpenShift releases mirrored into the environment's registry",
	Long: `cache lists the OpenShift releases vsno has mirrored into the environment's
registry, which vSNOs then install from instead of pulling 15-20 GB from the
internet. create mirrors a release the first time it is used; --no-cache turns
that off.`,
}

var cacheListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List the mirrored releases, their size, and the vSNOs using them",
	Args:    cobra.NoArgs,
	RunE:    runCacheList,
}

func init() {
	cacheCmd.AddCommand(cacheListCmd)
	rootCmd.AddCommand(cacheCmd)
}

// cacheEnv is the cache and the vSNOs, which say which release each uses.
func cacheEnv(cmd *cobra.Command) (cache.Env, guest.Env, error) {
	c, err := kube.New(kubeconfig, kubeContext)
	if err != nil {
		return cache.Env{}, guest.Env{}, err
	}
	if err := useEnvironment(cmd.Context(), c.Kube); err != nil {
		return cache.Env{}, guest.Env{}, err
	}
	genv, err := guestEnv(cmd.Context(), c, 0)
	if err != nil {
		return cache.Env{}, guest.Env{}, err
	}
	env := cache.Env{Kube: c.Kube, Dyn: c.Dyn, Config: c.Config, Builder: genv.Builder}
	return env, genv, nil
}

func runCacheList(cmd *cobra.Command, _ []string) error {
	env, genv, err := cacheEnv(cmd)
	if err != nil {
		return err
	}
	users := map[string][]string{}
	gs, err := guest.List(cmd.Context(), genv, namespace)
	if err != nil {
		return err
	}
	for _, g := range gs {
		if g.Image != "" {
			users[g.Image] = append(users[g.Image], g.Name)
		}
	}
	rep, err := cache.List(cmd.Context(), env, namespace, users)
	if err != nil {
		return err
	}
	return renderCache(cmd.OutOrStdout(), rep, time.Now(), palette(cmd.OutOrStdout()))
}

func renderCache(w io.Writer, rep *cache.Report, now time.Time, pal ui.Palette) error {
	if outputFormat == formatJSON {
		if rep.Entries == nil {
			rep.Entries = []cache.Entry{}
		}
		return writeJSON(w, rep)
	}
	if len(rep.Entries) == 0 {
		fmt.Fprintf(w, "No releases cached in %s. `vsno create` mirrors one the first time it needs it.\n", namespace)
		return nil
	}
	var t ui.Table
	t.Row(pal.Bold("VERSION"), pal.Bold("IMAGE"), pal.Bold("SIZE"), pal.Bold("MIRRORED"), pal.Bold("LAST USED"), pal.Bold("VSNOS"))
	for _, e := range rep.Entries {
		t.Row(dash(e.Version), e.Image, bytesText(e.SizeBytes), age(now.Sub(e.Mirrored)), age(now.Sub(e.LastUsed)), joinItems(e.Users))
	}
	t.Write(w)
	if rep.Total > 0 {
		fmt.Fprintf(w, "\nregistry volume: %s of %s used, %s free\n",
			bytesText(rep.Used), bytesText(rep.Total), bytesText(rep.Total-rep.Used))
	}
	return nil
}

// joinItems is items joined by ", ", or "-" when there are none.
func joinItems(items []string) string {
	if len(items) == 0 {
		return "-"
	}
	return strings.Join(items, ", ")
}
