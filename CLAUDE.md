# CLAUDE.md

Guidance for working in this repository.

## Requirements

- Go 1.27+

## Build

```bash
make build      # binary with version from git tags
make install    # build, then copy to $GOPATH/bin (or ~/go/bin)
make test       # all tests, verbose
make lint       # golangci-lint (config in .golangci.yml; gocyclo limit 15)
make vet
make fmt        # gofmt + goimports
make coverage
```

## Architecture

Cobra CLI, same layout as `sebrandon1/succulent-cli`.

- `main.go`: passes the build version (`-ldflags "-X main.version=..."`) to `cmd.SetVersion`.
- `cmd/`: one file per command, plus `_test.go`.
  - `root.go`: global flags `--kubeconfig`, `--context`, `-n/--namespace` (default `vsno`),
    `-o/--output table|json`, `--no-color` (sets `NO_COLOR`). `palette(w)` colors table output
    on a terminal; each renderer takes a `ui.Palette`, and tests pass `plain` or `colored`.
  - `precheck.go`: renders `precheck.Report` as a table or JSON, and supports
    `--write-config`.
  - `setup.go`: flags and `--config` into `setup.Options`, the terminal disk confirmation, and
    the step table or dry-run YAML.
  - `status.go`, `teardown.go`: render `status.Report` and `teardown.Result`. Teardown reuses
    `confirmOnTerminal` from `setup.go`.
  - `progress.go`: `newProgress()` returns a `ui.Progress` on stderr, disabled for `-o json`,
    plus the adapter to the `precheck.Progress` interface.
  - `version.go`: `vsno version`.
- `lib/kube`: builds client-go typed and dynamic clients from a kubeconfig (explicit path, then
  `$KUBECONFIG`, then `~/.kube/config`).
- `lib/precheck`: the cluster checks.
- `lib/manifests`: embedded `templates/*.yaml.tmpl` rendered with `text/template` (`quote` for every
  value). Golden files are in `testdata/`; regenerate them with `go test ./lib/manifests -update` and
  review the diff.
- `lib/pki`: registry CA, server cert (`Covers` decides whether to reissue), htpasswd.
- `lib/apply`: server-side apply (field manager `vsno`); it reports created, updated or unchanged
  for each object.
- `lib/state`: environment state in ConfigMap `<ns>/vsno-state`.
- `lib/setup`: `Run` resolves settings from the precheck, then runs the steps: operators, HyperConverged,
  storage, namespace, support services. Each step either reuses what it finds or applies the manifests
  and waits for them. In a dry run a recording Applier replaces SSA and the waits are skipped.
- `lib/cluster`: names of the host objects (operator namespaces, HyperConverged, LVMCluster,
  support Deployments), extra GVRs, and helpers shared by setup, status and teardown: `CSVs`,
  `SucceededCSV`, `Condition`, `RolledOut`, `Ready`.
- `lib/status`: `Collect` reads the host components, support services, VMs and state. A broken
  component becomes a Fail or Warn row; only namespace or state read errors fail it.
- `lib/teardown`: `Run` plans the targets in order, checks the safety rules, confirms, then
  deletes each target and waits for those marked `wait`.
- `lib/ui/color.go`: `Palette` (ANSI colors when `On`; status icons via `Icon`), `ColorEnabled`
  (terminal, no `NO_COLOR`, `TERM` not `dumb`), and `Table`, which aligns columns by visible width.
- `lib/ui`: step progress. On a terminal it shows a spinner, elapsed time and an optional bar
  (`Bar`, `FormatElapsed`). Elsewhere it prints one line when a step starts and one when it ends.

### `lib/precheck` files

| File | Role |
|---|---|
| `types.go` | `Status`, `Group`, `Result` (per-node rows set `Node`; `Name()` is `<node>: <check>`), `Report` (with `Nodes` verdicts), `Suggested`, and `Requirements` (defaults: 2 VMs × 16 cores / 32 GiB, a 600 GB disk, guest CIDRs). Also the `Progress`/`ProgressStep` interfaces, with no-op defaults. |
| `probe.sh` | Embedded node probe script. It runs via `chroot /host` and prints `KEY=VALUE` lines (`VIRT`, `KVM`, `NPROC`, `MEM_*`, `LSBLK`, `BYID`). It must stay read-only. |
| `probe.go` | `ParseProbe` turns that output into `NodeHardware`/`Disk`. `Disk.PreferredByID` prefers `wwn-*`. |
| `prober.go` | `PodProber` creates a temporary privileged namespace, one pod per node, reads the logs, then deletes the namespace. It reports each node as it finishes (`onDone`). |
| `checks.go` | Individual checks over `Env` (Kube, Dyn, `Prober`, `Progress`). `EvaluateNode` is pure and tested with fixtures. |
| `run.go` | `Run` orders the checks, evaluates workers, picks the best node, and fills `Suggested`. |
| `cidr.go`, `auth.go` | Pure helpers: CIDR overlap, and registry credential lookup |

### Behaviour to preserve

- Memory requested by pods in the environment namespace (`-n`) counts as **available**, because
  an existing environment would be reused or replaced. Memory used by other namespaces, including
  other vsno environments, doesn't, so a second environment on a busy node needs `--node`.
- The "StorageClass `lvms-vg-vsno` already exists" shortcut only applies to nodes whose CSINode
  lists the `topolvm.io` driver.
- When at least one node is eligible, failures on the other nodes are demoted to `info` with a
  `not usable:` prefix. The table groups rows into Cluster / Operators, storage and registry /
  Workers, with a heading per node that states the verdict, so it drops that prefix.
- `Report.add` assigns each row's `Group` (`groupOf`); add new checks there.
- A redrawn progress line never exceeds the terminal width (it would wrap and leave a copy on
  every redraw). The detail is shortened first, so the elapsed time stays visible.
- Without `--seed-repo`, the "Seed registry" row is `info` (the in-cluster registry is the
  default) and no auth files are read.
- Progress goes to stderr and is turned off for `-o json`, so stdout stays machine-readable.
- Tables with colored cells use `ui.Table`, not `text/tabwriter`, which counts color codes as width.
  With the codes stripped, colored output must equal plain output (`TestColorKeepsLayout`).
- dnsmasq publishes only `api`, `api-int` and `apps` for each guest, never a wildcard on the cluster
  domain.
- Guests reach the in-cluster registry by its ClusterIP, which is also an IP SAN on the cert. The
  guest's own `cluster.local` hides the host's Service names. Trusting the CA with
  `additionalTrustedCA` doesn't reboot the guest (spike in #3).
- setup reuses generated credentials: S3 keys and the registry CA and login are read back from their
  Secrets. The registry cert is reissued only when `pki.Covers` fails, for example because the
  Service IP changed. Reruns must report every object `unchanged`.
- setup never creates a second LVMCluster (LVMS supports one), and asks before it takes over a disk.
- A support pod in CrashLoopBackOff for 3 or more restarts ends the rollout wait early (`errStuck`).
- The builder installs RPMs with `tsflags=nocaps`, because the anyuid SCC can't set file
  capabilities.
- setup waits for an existing HyperConverged that isn't Available instead of reusing it as is.
- teardown only deletes a namespace with the `app.kubernetes.io/managed-by=vsno` label or a
  `vsno-state` ConfigMap. `--host` refuses while any of these exist: VMs elsewhere, PVCs on a
  TopoLVM StorageClass elsewhere, or an LVMCluster vsno didn't create. PVCs in
  `openshift-virtualization-os-images` don't count; teardown deletes that namespace before the
  LVMCluster. `--force` skips all of these checks, but they are still reported: as warnings
  marked `(ignored: --force)`, and as `forced` in the JSON result.
- When the confirmation prompt has shown the plan and nothing was deleted, teardown doesn't print
  the plan again.
- This repo is public. Docs, fixtures and tests must contain no lab hostnames, hardware models,
  real disk IDs or credentials.

## Tests

- `lib/precheck/testdata/probe-*.txt` hold synthetic probe output in the real format (product
  `Example Server`, IDs like `wwn-0x5000000000000001`):
  - a bare-metal worker with an empty disk
  - the same worker with its disk already taken by LVMS
  - a VM worker with nested KVM
- `run_test.go` builds a fake cluster with `k8s.io/client-go/kubernetes/fake` and
  `dynamic/fake`, plus a `fixtureProber`.

## Plan

Milestones 1 (`precheck`) and 2 (`setup`, `status`, `teardown`, #3) are done. The rest are GitHub issues labeled `roadmap`:

- #4, milestone 3: `create seed|target` (agent ISO, VM, install wait) and the guest helpers
- #5, milestone 4: `prepare`, `seed generate`, `upgrade`
- #6, milestone 5: test an LCA branch or commit (built in the cluster) or a prebuilt image
- #7, milestone 6: `vsno run` (kubeconfig only) and the first release

The manifests for the later milestones come from the manual procedure verified on 2026-11-18. The
author has them locally; ask before assuming their location.
