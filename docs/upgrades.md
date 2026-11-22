# Image Based Upgrades

vsno makes seed images from vSNOs and drives lifecycle-agent (LCA) Image Based Upgrades with them:
`prepare` gets a vSNO ready, `seed create` makes the seed image, `upgrade` runs the upgrade on a
target, and `vsno run` does the lot from a kubeconfig. Every flag is in the
[command reference](commands.md).

## Seed images

```bash
vsno seed precheck a               # is a ready? what would create wait on?
vsno seed precheck a --install     # ...and set up LCA and registry trust
vsno seed create a                 # make seeds/a:4.20.46 from vSNO a
vsno seed create a --stop          # ...and stop a afterwards, to free its memory
vsno seed list                     # or ls, or -o json
vsno seed delete a:4.20.46 --dry-run
vsno seed delete a                 # all of a's seeds
```

`seed precheck` reports everything `seed create` needs before it starts the 15–30 minute generation:
the vSNO is ready, the environment's registry is usable, the vSNO's API answers and its cluster is
settled, the Lifecycle Agent is installed (or the vSNO's catalog has it), the vSNO trusts the
registry's CA, and the seed image can be pushed. It only reads, and exits non-zero if anything is
missing. `--install` sets up the two things vsno can — subscribing to LCA from the vSNO's own catalog
and making the vSNO trust the registry — then reports again. `create` runs the same checks first, so
the two cannot drift.

`seed create` turns a ready vSNO into an Image Based Upgrade seed image. Seed generation has to run
on the vSNO itself, so the cluster is stopped while the Lifecycle Agent builds the image and comes
back when it's done. `create`:
1. waits for the cluster's operators to settle
2. installs the Lifecycle Agent from the vSNO's own catalog if it's missing
3. makes the vSNO trust the environment's registry
4. runs the seed generation, following it to the end

The image lands in the environment's registry as `seeds/<vsno>:<version>`. `--image` pushes elsewhere
(the host cluster's pull secret must have credentials for that registry). It takes about 15–30
minutes; the vSNO's API is down for most of it.

When the install finishes, `create` ejects the ISO: the CD-ROM and its volume leave the VM, the
`<name>-iso` DataVolume is deleted, and the builder's copy goes. The vSNO boots from its root disk
from then on, which frees about 4 GB. `vsno iso eject <vsno>` does the same for a vSNO from an
older vsno.

`seed list` shows each seed image's reference, the vSNO it came from, its OpenShift version, size and
age. `seed delete` removes tags by full reference, `<vsno>:<version>` or `<vsno>`. It asks you to
type the arguments first unless `--yes`, and `--dry-run` only lists. The registry frees the disk
space only when it is garbage collected, so deletion does not shrink it immediately.

## upgrade, prepare and rollback

```bash
vsno seed create seed-421                        # a seed image from a ready vSNO (see above)
vsno upgrade target --seed seed-421              # Prep, then Upgrade, to the seed's version
vsno upgrade target --seed seed-421 --finalize   # ...and finalize, dropping the old version
vsno upgrade target --seed seed-421 --to prep    # stop after Prep
vsno prepare target                              # only get a vSNO ready, ahead of time
```

`upgrade` runs an Image Based Upgrade on the target with the Lifecycle Agent (LCA). `--seed` is a
seed image (`vsno seed list`), `<vsno>:<version>`, or a vSNO, meaning its newest seed. For an image
outside the environment's registry, pass its reference and `--seed-version`.

1. It prepares the target, like `vsno prepare`: LCA and OADP from the vSNO's own catalog, trust in
   the environment's registry, a backup location in the environment's S3 (reached by ClusterIP)
   that OADP reports Available, and the `seed-pull-secret` LCA pulls the seed with.
2. It records the target's identity: version, clusterID, infrastructureName, node and API URL. When
   the seed's vSNO answers, its LCA must be the same version as the target's.
3. **Prep** pulls the seed's images ahead, shown as a progress bar.
4. **Upgrade** reboots the node into the seed's version. The API is down for about 15 minutes; if
   LCA rolls the upgrade back by itself, upgrade says so and stops.
5. It prints the identity before and after, which must differ only in version, and how long each
   stage and phase took.

`--finalize` then moves to Idle, dropping the old version; `--rollback` goes back to it. Run again
with the same seed, upgrade carries on from where the target is, so an upgrade can be finalized or
rolled back later. A target part way through with another seed is refused. `vsno list` shows a
vSNO's IBU stage in its detail, such as `IBU Upgrade done`.

`prepare <vsno>... --role seed` sets up only what a seed needs (LCA, OADP, registry trust).

### Testing an LCA branch

```bash
vsno prepare seed-421 target --lca-ref my-fix --lca-repo https://github.com/me/lifecycle-agent
vsno seed create seed-421                                 # the seed is made with that LCA
vsno upgrade target --seed seed-421 --finalize            # and the upgrade runs with it
vsno prepare target --lca-image quay.io/me/lca:test       # a prebuilt image instead of a build
```

`--lca-ref` (on `prepare` and `upgrade`) builds LCA from a branch, tag or SHA without building
anything on your laptop. A privileged buildah pod in the environment namespace, next to the builder,
clones the repository at the ref and builds its `Dockerfile` for `linux/amd64`, using the host
cluster's pull secret for the base images. It pushes the result to the environment's registry as
`lca:<sha>`, and skips the build when that tag is already there. `--lca-repo` names a fork (default
`openshift-kni/lifecycle-agent`), and `--lca-image` deploys a prebuilt image instead of building one.

Each vSNO then runs that LCA instead of its catalog's:
- the OLM Subscription and CSV are removed; the CRDs and the ImageBasedUpgrade stay
- the ref's `config/default` manifests are applied with the controller image replaced, as
  `make deploy` does
- the registry's login is added to the vSNO's pull secret
- vsno waits for the controller to run the new image

It warns when the ref's `VERSION` isn't the vSNO's OpenShift minor. The source is recorded on the
vSNO and shown in the `LCA` column of `vsno list -o wide`. Later `prepare`, `upgrade` and
`seed create` runs keep an LCA deployed from a ref rather than replacing it with the catalog's.
Prepare seed and target from the same ref: `upgrade` refuses a seed made with a different LCA.

## run: the whole IBU from a kubeconfig

```bash
vsno run                                        # shows the plan, asks, then does it all
vsno run --yes --finalize                       # for CI: no question, finalize at the end
vsno run --target-version 4.20.45 --seed-version 4.20.46
vsno run --seed-image registry/seeds/a:4.21.42  # no seed vSNO; upgrade to an existing seed
vsno run -f vsno.yaml                           # the same settings from a file
```

`run` does an Image Based Upgrade with only a kubeconfig:
1. **setup**, as `vsno setup`
2. **vsnos**: a seed and a target vSNO, both started before waiting for either. On a cold cache
   their releases are mirrored into the environment's registry **at the same time**, so the two
   ~8 minute mirrors overlap rather than run back to back (`vsno cache list` shows what is there)
3. **seed**: a seed image from the seed vSNO, which is stopped afterwards
4. **upgrade**: the target upgraded with it, as `vsno upgrade`

By default the seed is the newest stable release of the host's OpenShift minor and the target the
release before it. `--seed-version` and `--target-version` take anything `vsno create` accepts, and
the target must be older. The plan (versions, names, LCA source and what happens afterwards) is
shown and confirmed first, unless `--yes`.

Each stage reuses what's already there: a vSNO of that name at the right version, the seed image an
earlier run made, an upgrade part way. So when a run is interrupted, running it again carries on;
the plan says which stages an earlier run finished. Stages are recorded in the environment's state
(`vsno status`). Ctrl+C stops a run cleanly.

A vSNO the run has to create is **restored from its snapshot** when the environment keeps one at the
plan's version (see [Snapshots](performance.md)): a minute instead of an install, and the plan says so
(`target   vSNO target at 4.22.21, restored from snapshot target`). A snapshot is only usable under
its own name and version, so a run naming other vSNOs, or asking for another version, installs as
before. `vsno run --delete` leaves the snapshots behind, so the next run starts from them again.

`--restart` runs every stage again instead of skipping the ones this plan already finished, which is
what repeating a test wants. With the seed image kept (`--seed-image`) and a rollback at the end, a
target can be upgraded over and over without installing a vSNO:

```bash
vsno run --yes --rollback --restart --seed-image registry.vsno.svc.cluster.local:5000/seeds/seed:4.22.22
```

`-f vsno.yaml` takes the same settings as the flags, by their JSON names, and flags override it:

```yaml
targetVersion: "4.21.1"
seedVersion: "4.21.42"
lca: {ref: my-fix, repo: https://github.com/me/lifecycle-agent}
finalize: true
```
