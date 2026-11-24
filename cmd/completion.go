package cmd

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/sebrandon1/vsno/lib/config"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/kube"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/seed"
	"github.com/sebrandon1/vsno/lib/snapshot"
	"github.com/spf13/cobra"
)

// Cobra has no completion timeout, so each helper takes its own: a shell that
// waits on a slow cluster is worse than one that offers nothing.
const (
	completionReadTimeout = 2 * time.Second
	completionExecTimeout = 5 * time.Second // listing seeds and snapshots runs in the builder
)

// completeVSNOs offers the vSNO names in the environment. It is for commands
// that take more than one name.
func completeVSNOs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return completeFrom(completionReadTimeout, vsnoNames, args, toComplete)
}

// completeVSNOFirst offers the names only for a command's first argument: what
// follows it belongs to oc or ssh.
func completeVSNOFirst(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return completeVSNOs(cmd, args, toComplete)
}

// completionClients is the cluster the environment-backed completions read.
func completionClients() (*kube.Clients, error) {
	return kube.New(kubeconfig, kubeContext)
}

// completeNodes offers the workers that can host a vSNO.
func completeNodes(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return completeFrom(completionReadTimeout, func(ctx context.Context) ([]string, error) {
		c, err := completionClients()
		if err != nil {
			return nil, err
		}
		return precheck.WorkerNames(ctx, c.Kube)
	}, args, toComplete)
}

// completeNamespaces offers the vsno environments, for -n.
func completeNamespaces(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return completeFrom(completionReadTimeout, func(ctx context.Context) ([]string, error) {
		c, err := completionClients()
		if err != nil {
			return nil, err
		}
		return guest.Environments(ctx, c.Kube)
	}, args, toComplete)
}

// completeOutput offers the formats a command can produce. wide is only what
// commandsWithWide lists.
func completeOutput(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	vals := []string{formatTable, formatJSON}
	if commandsWithWide[cmd.CommandPath()] {
		vals = append(vals, formatWide)
	}
	return prefixOf(vals, toComplete), cobra.ShellCompDirectiveNoFileComp
}

// completeStates offers the vSNO states for --state.
func completeStates(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return prefixOf(stateNames(guest.States()), toComplete), cobra.ShellCompDirectiveNoFileComp
}

// completeConfigKeys offers the setting names; for set, a path setting after
// its key falls back to file completion.
func completeConfigKeys(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		if cmd.Name() == "set" && len(args) == 1 {
			switch config.Key(args[0]) {
			case config.SeedSyncAuthFile, config.SeedSyncCA:
				return nil, cobra.ShellCompDirectiveDefault
			}
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	keys := make([]string, 0, len(config.Settings))
	for _, s := range config.Settings {
		keys = append(keys, string(s.Key))
	}
	return prefixOf(keys, toComplete), cobra.ShellCompDirectiveNoFileComp
}

// completeSeedImages offers the seed images in the environment's registry.
func completeSeedImages(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return completeFrom(completionExecTimeout, func(ctx context.Context) ([]string, error) {
		ns, env, err := completionEnv(ctx)
		if err != nil {
			return nil, err
		}
		imgs, err := seed.List(ctx, seed.Env{Env: env}, ns)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(imgs))
		for _, im := range imgs {
			if im.Ref != "" {
				out = append(out, im.Ref)
			}
		}
		return out, nil
	}, args, toComplete)
}

// completeSnapshots offers the kept snapshots.
func completeSnapshots(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return completeFrom(completionExecTimeout, func(ctx context.Context) ([]string, error) {
		ns, env, err := completionEnv(ctx)
		if err != nil {
			return nil, err
		}
		entries, err := snapshot.List(ctx, snapshot.Env{Env: env}, ns)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(entries))
		for _, e := range entries {
			if e.Name != "" {
				out = append(out, e.Name)
			}
		}
		return out, nil
	}, args, toComplete)
}

// completionEnv is the environment a completion reads, and its namespace. Its
// progress is off: a completion must not draw.
func completionEnv(ctx context.Context) (string, guest.Env, error) {
	c, err := completionClients()
	if err != nil {
		return "", guest.Env{}, err
	}
	ns := completionNamespace(ctx, c)
	env, err := guestEnvFor(c, ns, 0)
	if err != nil {
		return "", guest.Env{}, err
	}
	env.Progress = nil
	return ns, env, nil
}

// vsnoNames is the names in the environment the flags and settings point at.
func vsnoNames(ctx context.Context) ([]string, error) {
	c, err := completionClients()
	if err != nil {
		return nil, err
	}
	return guest.Names(ctx, c.Kube, c.Dyn, completionNamespace(ctx, c))
}

// completeFrom reads the candidates under a timeout and filters them. Every
// failure is reported as "nothing to offer", because a completion must not
// print or fail.
func completeFrom(d time.Duration, read func(context.Context) ([]string, error), args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	items, err := read(ctx)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return candidates(items, args, toComplete), cobra.ShellCompDirectiveNoFileComp
}

// candidates drops what was already given and what does not match the prefix.
func candidates(items, args []string, toComplete string) []string {
	out := make([]string, 0, len(items))
	seen := map[string]struct{}{}
	for _, it := range items {
		if it == "" || slices.Contains(args, it) || !strings.HasPrefix(it, toComplete) {
			continue
		}
		if _, ok := seen[it]; ok {
			continue
		}
		seen[it] = struct{}{}
		out = append(out, it)
	}
	slices.Sort(out)
	return out
}

// prefixOf filters flag values, which are not positional arguments.
func prefixOf(items []string, toComplete string) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		if strings.HasPrefix(it, toComplete) {
			out = append(out, it)
		}
	}
	return out
}

// completionNamespace is useEnvironment's answer for a completion: an explicit
// -n, else the configured namespace, else the one environment when there is
// exactly one. It never fails and never chooses between several.
func completionNamespace(ctx context.Context, c *kube.Clients) string {
	ns := namespace
	if !rootCmd.PersistentFlags().Changed("namespace") {
		if s, ok := config.SettingFor(config.Namespace); ok {
			if v := config.Resolve(s, "", configFor(), false); v != "" {
				ns = v
			}
		}
	}
	envs, err := guest.Environments(ctx, c.Kube)
	if err != nil || len(envs) == 0 {
		return ns
	}
	switch {
	case slices.Contains(envs, ns):
		return ns
	case len(envs) == 1:
		return envs[0]
	}
	return ns
}
