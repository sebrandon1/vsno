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
  for each object. `Plan` (the `Planner` interface) only reads, and says create, reapply (vsno's
  field manager owns fields in it) or take over (it exists, but vsno doesn't manage it). When the API
  rejects an apply with 422 because another manager's fields collide with ours (two `readinessProbe`
  handlers, say), `Apply` replaces the object, carrying the Service addresses, PVC binding and
  Namespace finalizers a replace must keep. Other errors are reported, never retried as a replace.
- `lib/state`: environment state in ConfigMap `<ns>/vsno-state`.
- `lib/setup`: `Run` resolves settings from the precheck, then runs the steps: operators, HyperConverged,
  storage, namespace, support services. Each step either reuses what it finds or applies the manifests
  and waits for them. In a dry run a recording Applier replaces SSA, asks the SSA's `Plan` what
  each object would do, and the waits are skipped. Before the steps, `offerUpdates` asks about the
  operators `cluster.OperatorUpdate` finds behind (a newer CSV in the channel, a Manual InstallPlan,
  or a channel older than the precheck's): accepted ones are applied by the operator step, which
  patches the Subscription channel, approves the InstallPlan, then waits for the new CSV.
- `lib/cluster`: names of the host objects (operator namespaces, HyperConverged, LVMCluster,
  support Deployments), the OLM GVRs, and helpers shared by precheck, setup, status and teardown:
  `CSVs`, `SucceededCSV`, `CSVPhase`, `Condition`, `RolledOut`, `Ready`, and the operator update
  detection (`OperatorUpdate`, `Subscription`, `Approve`, `SetChannel`). It imports no other vsno
  package but `state`, so precheck can use it.
- `lib/status`: `Collect` reads the host components, support services, VMs and state. A broken
  component becomes a Fail or Warn row; only namespace or state read errors fail it. An operator
  with an update available is a Warn row, so status still exits 0.
- `lib/teardown`: `Run` plans the targets in order, checks the safety rules, confirms, then
  deletes each target and waits for those marked `wait`. `plan.go` mirrors setup for the report:
  `manifests.Steps` in reverse, each step's rendered objects (reversed; `Planner` says delete or
  absent) plus that step's extra targets. `markDeleted` marks a step deleted once all its targets
  are; `support` follows `namespace`.
- `lib/manifests`: `Steps` (with `Step*` name constants) is setup's step list and is the single
  source teardown mirrors. `NamesConfig(ns)` renders object names without real settings.
- `lib/release`: `Resolver.Resolve` turns `--version` into a release image (update graph for releases
  and minors, the CI release controller for `X.Y-nightly`; nightly names and pullspecs as given).
  Tests use `httptest`.
- `lib/builder`: `Runner` runs a bash script in the builder pod (`Exec`: pod exec, WebSocket with
  SPDY fallback). Without a stderr writer it captures stderr into the error; exec needs at least
  one output stream.
- `lib/guest`: vSNOs. `templates/` (install-config, agent-config, the `/var/lib/containers`
  MachineConfig, and the Service, ISO DataVolume and VM) and `scripts/` (`iso.sh`, `probe.sh`,
  `rm.sh`, run in the builder) are embedded. `Create`, `List`, `ListAll`, `Delete`, `SetRunning`,
  `WaitState`, and the `Oc`/`SSH`/`Kubeconfig`/`Screenshot` helpers. `Environments` finds the
  environments for `ListAll`: namespaces labeled `app.kubernetes.io/managed-by=vsno`, plus any
  holding the `app=builder` pod, so hand-built environments are included. `Classify` turns
  observations (VM status, ISO DataVolume, probe) into a `State`; create's wait and list share it.
  Golden files in `testdata/` (`go test ./lib/guest -update`).
- `lib/seed`: Image Based Upgrade seed images. `Precheck` reports what a seed needs (vSNO state,
  registry, guest API and cluster settling, LCA, registry trust, the image and its push credentials)
  and with `Install` sets up LCA and trust; `Create` runs it first, then drives the Lifecycle Agent
  (`waitStable`, `ensureLCA`, `ensureTrust`, `generate`, `finish`); while the guest API is
  down it reads progress from the node's `lca-generate-seed-image` unit. `List`/`Delete` talk to the
  environment's registry through `scripts/registry.sh` (skopeo + curl, credentials on stdin) from the
  builder. Templates and the script are embedded; goldens in `testdata/`.
- `lib/registry`: the environment's in-cluster OCI registry. `Registry{Host,User,Password,CA}`,
  `Load` (from `registry-auth` and `registry-ca`), `Address` (the ClusterIP form guests use, because
  their `cluster.local` hides the host's Services), `AddAuth` (merging credentials into a pull
  secret), `Usage` (df in the registry pod), and `scripts/registry.sh` (list/delete/delete-manifests
  via curl and skopeo, credentials on stdin). `lib/seed` re-exports it.
- `lib/cache`: mirrored OpenShift releases. `Ensure` looks a payload's digest up in ConfigMap
  `vsno-cache`, mirrors on a miss with `oc adm release mirror` in the builder (`scripts/mirror.sh`,
  one repository per payload under `ocp/release/<sha16>`), and returns the install-config's
  `imageDigestSources`, the CA, and a pull secret with the registry added. `Prune` deletes a
  release's manifests and runs `GarbageCollect`, which stops the registry, runs its collector in a
  Job over the same volume, and starts it again.
- `lib/isos`: what creates leave behind. `List` runs `scripts/isos.sh` in the builder (served ISOs,
  build directories, cached `openshift-install` binaries with their `.ref` release image, the base
  ISO cache, volume usage) and classifies each against the namespace's VMs and ISO DataVolumes;
  `Prune` removes what no vSNO uses and `Delete` removes the ones named, both through the shared
  `remove`. `guest.EjectISO` drops a finished vSNO's CD-ROM and ISO
  DataVolume; `create` calls it, with `rm.sh`, once the install is ready.
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
- `manifests.Render` labels every object `app.kubernetes.io/managed-by=vsno`. Owning that label
  keeps vsno's field manager on objects whose other fields belong to controllers (a ServiceAccount),
  which `Plan` relies on to tell vsno's objects from someone else's.
- setup never creates a second LVMCluster (LVMS supports one), and asks before it takes over a disk.
- A support pod in CrashLoopBackOff for 3 or more restarts ends the rollout wait early (`errStuck`).
- The builder installs RPMs with `tsflags=nocaps`, because the anyuid SCC can't set file
  capabilities.
- setup waits for an existing HyperConverged that isn't Available instead of reusing it as is.
- Without `-n`, the vSNO commands and `status` call `useEnvironment` (`cmd/root.go`): they use the
  one namespace labeled `managed-by=vsno`, because the default name may hold something else.
  `setup` and `teardown` keep the plain default.
- The VM labeled `vsno.io/name` is the vSNO inventory; there is no separate list. Its annotations
  hold the requested version, the release image and the installed version.
- Guests reach each other's names only through dnsmasq. setup's `dnsmasq.conf` loads
  `/etc/vsno-guests` (ConfigMap `vsno-guests`, one key per vSNO, written by create and delete).
  dnsmasq restarts itself when those files change. Never add guest names to setup's ConfigMap.
- vsno reaches guests only through the builder pod: their Service ClusterIP, plus
  `--tls-server-name api.<name>.vsno.lab`. Install progress before the first reboot comes from the
  agent API on port 8090, using the token in `.openshift_install_state.json`.
- `iso.sh` builds one ISO at a time (`flock`: parallel builds hit a `libnmstate.so` race). It caches
  `openshift-install` per release image in `/work/bin`, and the installer's base ISO under
  `/work/cache`.
- setup creates the builder's SSH key (Secret `vsno-ssh`) once and reuses it; create reads the
  cluster pull secret directly.
- setup's step names must match `manifests.Steps` (`TestRunDryRun` checks the order).
- setup asks before taking over (and teardown only deletes) a namespace that `cluster.CreatedByVsno`
  accepts: it has the `app.kubernetes.io/managed-by=vsno` label or a `vsno-state` ConfigMap. setup
  makes you type the namespace name to confirm; `--force` skips the prompt. A foreign namespace that
  holds vSNOs is refused without a prompt, so only `--force` overrides it. setup checks this first,
  before precheck. Per-object "take over" in the dry-run plan is
  only reported, not enforced: vsno objects from before every object was labeled can lack vsno's
  field manager, so enforcing it would refuse old environments.
- teardown `--host` refuses while any of these exist: VMs elsewhere, PVCs on a TopoLVM StorageClass
  elsewhere, or an LVMCluster vsno didn't create. PVCs in
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

Milestones 1 (`precheck`), 2 (`setup`, `status`, `teardown`, #3) and 3 (vSNO lifecycle, #4) are done. The rest are GitHub issues labeled `roadmap`:

- #16, milestone 4: fast creates. Done: the tools are cached per release, and releases are mirrored
  into the in-cluster registry with `vsno cache`.
- #5, milestone 5: IBU on any two vSNOs (`prepare`, `seed generate`, `upgrade`)
- #6, milestone 6: test an LCA branch or commit (built in the cluster) or a prebuilt image
- #7, milestone 7: `vsno run` (kubeconfig only) and the first release; image-based install as a
  stretch goal

vsno is a general vSNO lifecycle tool; IBU is one workflow on top of it. Don't build features that
assume exactly two guests named seed and target.

The manifests for the later milestones come from the manual procedure verified on 2026-11-18. The
author has them locally; ask before assuming their location.
