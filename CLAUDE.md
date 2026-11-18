# CLAUDE.md

Guidance for working in this repository.

## Requirements

- Go 1.27+

## Build

```bash
make build      # binary with version from git tags
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
    `-o/--output table|json`.
  - `precheck.go`: renders `precheck.Report` as a table or JSON, and supports
    `--write-config`.
  - `progress.go`: `newProgress()` returns a `ui.Progress` on stderr, disabled for `-o json`,
    plus the adapter to the `precheck.Progress` interface.
  - `version.go`: `vsno version`.
- `lib/kube`: builds client-go typed and dynamic clients from a kubeconfig (explicit path, then
  `$KUBECONFIG`, then `~/.kube/config`).
- `lib/precheck`: the cluster checks.
- `lib/ui`: step progress. On a terminal it shows a spinner, elapsed time and an optional bar
  (`Bar`, `FormatElapsed`). Elsewhere it prints one line when a step starts and one when it ends.

### `lib/precheck` files

| File | Role |
|---|---|
| `types.go` | `Status`, `Result`, `Report`, `Suggested`, and `Requirements` (defaults: 2 VMs × 16 cores / 32 GiB, a 600 GB disk, guest CIDRs). Also the `Progress`/`ProgressStep` interfaces, with no-op defaults. |
| `probe.sh` | Embedded node probe script. It runs via `chroot /host` and prints `KEY=VALUE` lines (`VIRT`, `KVM`, `NPROC`, `MEM_*`, `LSBLK`, `BYID`). It must stay read-only. |
| `probe.go` | `ParseProbe` turns that output into `NodeHardware`/`Disk`. `Disk.PreferredByID` prefers `wwn-*`. |
| `prober.go` | `PodProber` creates a temporary privileged namespace, one pod per node, reads the logs, then deletes the namespace. It reports each node as it finishes (`onDone`). |
| `checks.go` | Individual checks over `Env` (Kube, Dyn, `Prober`, `Progress`). `EvaluateNode` is pure and tested with fixtures. |
| `run.go` | `Run` orders the checks, evaluates workers, picks the best node, and fills `Suggested`. |
| `cidr.go`, `auth.go` | Pure helpers: CIDR overlap, and registry credential lookup |

### Behaviour to preserve

- Memory requested by pods in the vsno namespace counts as **available**, because an existing
  environment would be reused or replaced.
- The "StorageClass `lvms-vg-vsno` already exists" shortcut only applies to nodes whose CSINode
  lists the `topolvm.io` driver.
- When at least one node is eligible, failures on the other nodes are demoted to `info` with a
  `not usable:` prefix.
- Without `--seed-repo`, the "Seed registry" row is `info` (the in-cluster registry is the
  default) and no auth files are read.
- Progress goes to stderr and is turned off for `-o json`, so stdout stays machine-readable.
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

Milestone 1, `precheck`, is done. The rest are GitHub issues labeled `roadmap`:

- #3, milestone 2: `setup` / `teardown` / `status`, plus the in-cluster registry
- #4, milestone 3: `create seed|target` (agent ISO, VM, install wait) and the guest helpers
- #5, milestone 4: `prepare`, `seed generate`, `upgrade`
- #6, milestone 5: test an LCA branch or commit (built in the cluster) or a prebuilt image
- #7, milestone 6: `vsno run` (kubeconfig only) and the first release

The manifests for the later milestones come from the manual procedure verified on 2026-11-18. The
author has them locally; ask before assuming their location.
