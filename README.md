# egress-router

sing-box egress router for telemt and local Docker services.

## First deployment

Use a Linux host with Docker Engine, Compose v2, Bash, `flock` (util-linux),
`curl`, `jq`, coreutils, and `sudo`. The deployment user needs Docker access and
sudo permission for the repository's initializer and snapshot file operations.
CI needs these permissions without an interactive password prompt. Docker and
sudo access are privileged; only trusted users should be able to edit this checkout.

From the cloned repository, use this single deployment path:

```bash
# Keep these values in the deployment user's environment, including SSH CI.
export EGRESS_UID=1000 EGRESS_GID=1000
make init
sudoedit config.json
make deploy
make -C subscription-manager add NAME=provider URL=https://example.invalid/subscription
```

Adjust the example's interface/listen addresses to this machine before deploying
(in particular `172.17.0.1` must exist). UID/GID default to 1000 in both scripts
and Compose; set them in the shell, not only in Compose's `.env`. They need not
match the login user. The initializer sets ownership explicitly, even for
existing state. Directories are `0700`; state and base config are `0600`.
Runtime configs/snapshots are `0640`, readable by the router's supplemental
`EGRESS_GID` group even with DAC-bypass capabilities dropped.
Use `sudoedit` when the login user does not own the files. Do not invoke raw
`docker compose up` on an uninitialized checkout: missing file binds can become
directories. Initialization rejects directories and symlinks at config paths.

Initialization creates `config.json` from the base example and seeds a missing
`runtime/config.json` with that base, not an empty file. A missing `active.json`
is seeded from the existing `runtime/config.json` before the deployment starts
or refreshes the manager, preserving legacy generated config on migration.
This legacy generated file may differ from the config mounted by an already
running router: background atomic writes can have replaced the host file without
updating that container's bind-mounted inode. Migration does not capture the live
container config, so its initial rollback target is not necessarily the exact
previously running config. Existing active snapshots are never reseeded.
The writable `./state` directory is bound
to `/data/state`; the manager uses `/data/state/subscriptions.json`. Only when
that new file is absent, initialization copies the legacy
`subscription-manager/subscriptions.json`, or the empty example if no legacy file
exists. Existing new state always wins. Migration does not print or parse secrets;
the legacy file remains untouched. Protect or remove that redundant copy yourself
after verifying migration.

The `direct-eth` outbound is a **urltest candidate**, not merely an emergency
fallback: direct traffic can win even when proxy nodes are available.

## Activation

`make deploy` starts/builds the manager, polls HTTP readiness for at most about
four minutes, then requests a generation. `make add` and `make refresh` use the
same activation path against the running manager. A health response of 503 is
accepted for HTTP readiness so a corrective operation can recover a failed build;
it is not treated as a successful generation. POST must succeed first (five-minute
client timeout). Metadata comments are ignored. Unsupported or malformed nodes
(including VMess and TUIC) are skipped with sanitized warnings and a
`skipped_nodes` counter. A failed fetch or a provider with no usable nodes rejects
the entire refresh with HTTP 400. The previous generated config is preserved on
failure, including validation failure. Duplicate usable nodes across providers
do not make a provider empty.
`/health` returns 503 until a successful build and again after a failed attempt.

All supported manual add/refresh/deploy/rollback commands hold a host `flock` on
`.deploy.lock`. After acquiring the lock, manual commands re-exec the current
script with the inherited lock descriptor, picking up any CI checkout that
finished while they waited. After successful generation, the script saves the old active file
as `runtime/previous.json`, snapshots `runtime/config.json` to `runtime/active.json`,
and force-recreates only the router. The router mounts **active.json**, never the
background output. It must pass container health and a real HTTPS request through
the localhost mixed proxy. Each verification phase has 30 bounded attempts,
with up to ten seconds per curl and two seconds between attempts.

```bash
SMOKE_URL=https://telegram.org/ make deploy
SMOKE_PROXY=http://127.0.0.1:10880 make refresh
make rollback
```

`SMOKE_URL` defaults to `https://cp.cloudflare.com/generate_204`; use an HTTPS
endpoint appropriate for your routing rules. Smoke requires an HTTP localhost
mixed proxy and ignores `NO_PROXY`. It checks one route, not every node or telemt
interception. Container health alone is only sing-box config validation.

If activation fails, the script restores the old active snapshot, recreates the
router and checks it again. Rollback verification failure is reported and requires
operator intervention. On a fresh installation the rollback target is the base
config, not a previously proven deployment. `make rollback` explicitly reapplies
the saved previous config transactionally: it first saves current active config
to a protected temporary recovery file, restores it if activation fails, and
swaps the previous snapshot to the original current config on success. Another
`make rollback` can undo a successful rollback. A failed recovery retains the
temporary `runtime/.rollback.*` file for operator intervention.
Rollback covers **config only**, not the manager image,
subscription state, base config, or Git revision.

Background refreshes generate and validate `runtime/config.json` but are **not
applied**. Atomic replacement of generated files allows a coherent snapshot even
if a background refresh races the host command; it may be the latest validated
generation rather than exactly the POST's generation. The host lock cannot cover
direct API callers or background refresh. Do not use raw API/recreate commands
concurrently with deployment. Power loss/SIGKILL cannot run rollback traps; inspect
active/previous snapshots and use `make rollback` after recovery.

The manager has no Docker socket. Its unauthenticated API listens only on
localhost:19091; do not publish it or expose it to untrusted local services.
Commands suppress subscription payloads and API response bodies and pass add
parameters via environment and `jq`, not shell interpolation. `make ... URL=...`
can still expose a URL in shell history/process arguments: for real credentials,
populate `NAME` and `URL` through a trusted secret mechanism in the environment
and run `make add` without command-line values. Never enable shell tracing or
print state/generated configs; they contain credentials. Both services rotate
Docker JSON logs (three files of 10 MB).

## Telemt interception

Optional interception requires `nft`, `ip`, **`ss`** (iproute2), and systemd.
Set host-specific overrides in `/etc/default/telemt-egress-tproxy` before enabling:
`TELEMT_SOURCE_IP`, `TELEMT_UID`, and `HOST_IFACE`. Defaults assume container IP
`192.168.128.2`, telemt UID `65532`, and interface `eth0`; host-networked telemt
needs its host source IP. Interception targets telemt, Telegram CIDRs and TCP 443.

After a successful router deployment:

```bash
make install-tproxy
# Atomic rule reload after changing overrides:
sudo systemctl reload telemt-egress-tproxy.service
```

The unit requires Docker unit ordering; preserve its Docker dependencies when
customizing it. Docker startup ordering is not router readiness: ensure the
redirect listener is available before installing/reloading interception.
To disable interception and stop the stack:

```bash
sudo systemctl disable --now telemt-egress-tproxy.service
make down
```

## Checks and CI/CD

```bash
make check
make deployment-smoke
```

The smoke script uses a disposable container with a tmpfs, no network and only
public examples plus deployment scripts mounted read-only. It checks fresh init,
permissions, legacy migration, new-state precedence, UID/GID changes, and rejection
of directory binds, including active seeding from legacy generated config.
Mocked Docker/curl/sudo/sleep operations also exercise successful activation,
failed activation recovery, successful reversible rollback, failed explicit
rollback recovery, and generation failure without recreation. The mocks verify
that privileged operations retain the host-style lock through script re-exec.
It never mounts real state or starts services. The Debian test image may need
pulling first. It does not test live proxy connectivity or real Docker recreation;
those need a disposable Linux deployment environment. CI runs this smoke test
in the check job.
The manager's sing-box validator and router use the same pinned image digest;
update both together (and the Makefile validation image).

Pull requests run checks and build the manager image. Pushes to main deploy only
after both jobs pass. Set GitHub Actions secrets `VM_HOST`, `VM_USER`, `VM_SSH_KEY`,
and `VM_PROJECT_PATH` (the absolute checkout path). Main workflows are not canceled
in progress, and production jobs are serialized. SSH takes the same host deployment
lock before fetching/checking out and holds it through activation. It rejects a
dirty tracked worktree, fetches origin/main, verifies the exact `github.sha` commit,
then uses non-destructive `git checkout --detach` of that SHA, not `git pull`.
Ignored config, state and runtime files remain in place; obstructing untracked
files cause checkout to fail rather than being removed. The deployment checkout
will remain detached. Pending GitHub concurrency jobs may be superseded; this is
not a guarantee that every push gets deployed.

## Optional Catalog Shadow

An isolated, opt-in catalog consumer is available in
[`catalog-client/`](catalog-client/README.md). `make shadow-check` runs its local
checks and native build. It writes only a separately validated shadow candidate;
it is not invoked by deployment, Compose, refresh, or subscription-manager and
does not activate configs. See its README for the staged read-only migration.

## Pre-Deployment Backup and Recovery

**Before merging to main or allowing its deployment**, snapshot the current Linux
host. Run this manually on that host, using the reviewed script (it need not be
inside the deployed checkout). Python 3.9+, root, Git, Docker and an existing
project `.deploy.lock` are required. Do not run deployment or direct API writes
concurrently. The script never stops or pauses the router.

```bash
sudo python3 scripts/snapshot-router.py \
  --project /home/maxim/projects/infra/network/egress-router \
  --destination /var/backups/egress-router
```

Those are also the defaults. The destination must be root-owned `0700`; a new
destination is created privately. Project/destination symlink components and
destinations inside the project are rejected. Each run creates a unique dated
`0700` directory with `0600` artifacts. Treat **all artifacts as credentials**:
never commit, publish, attach to CI, or print their contents. Transfer an encrypted
copy to separate protected storage if protection against host/disk loss is needed.

The snapshot holds the existing deployment flock throughout. Before copying it
records Git HEAD/refs/status/file index and a `git bundle --all`, exact container
inspect JSON and immutable image IDs. Git reads run as the checkout owner with
optional index locks disabled. It briefly pauses subscription-manager only if
running and not already paused, copies the router's actually mounted config via
Docker (which may differ from the host file's inode), and archives the complete
worktree except `.git` and `.deploy.lock`. Ignored `.env`, base config, runtime,
new state and legacy subscriptions are included. Internal symlinks are archived
as links, not followed. No application-level transaction guarantee is implied.

The manager is unpaused in `finally`, including when pause/copy/archive commands
fail, **before** saving both images by immutable ID (deduplicated) to `images.tar`.
An already stopped/paused manager is left unchanged. Unpause failure prevents a
successful snapshot. SIGKILL, power loss or a failed Docker daemon can still leave
it paused: inspect its state manually after interruption. Partial backups are
retained, never silently removed. Free-space preflight requires estimated worktree
tar size plus inspected image sizes plus 512 MiB headroom; concurrent disk use or
archive overhead can still cause failure.

Installed tproxy executable, systemd unit and defaults are copied if present;
only `ip telemt_tproxy` is queried via nft JSON, never the full host ruleset.
Failed/unavailable nft queries are recorded as unknown existence, not definitive
absence. Host artifacts are **not automatically reapplied**. External mounts,
Docker writable layers, systemd enablement, other services and remote/unreachable
Git objects are outside scope.

Require `COMPLETE` before proceeding with merge/deployment. It is written only
after reading the tar archives, verifying the Git bundle, parsing mounted config
JSON and generating streaming SHA256 hashes of artifacts. `SHA256SUMS` excludes
itself and the final marker. These are structural checks, **not** sing-box config
validation, live HTTPS verification or a full restore drill. Child output stays
in protected files; stderr is suppressed and failures do not print secret data.

Recovery checklist (full operator instructions are generated as `RESTORE.md`):

1. Take a fresh snapshot first; privately verify `SHA256SUMS` and `COMPLETE`.
2. Hold the original checkout's `.deploy.lock` and stop the writer deliberately.
3. Extract the worktree into a separate protected recovery root, load saved
   images, and review recovered Compose binds, `.env`, ownership and host settings.
4. Use `running-config.json` as authoritative; replace recovered
   `runtime/active.json` safely and adjust legacy router binds if necessary.
5. Validate with the saved local router image, then use recovered base Compose
   plus `image-override.yaml` to recreate **only** the router, without build/pull.
6. Check health, real HTTPS through the proxy and telemt routing. Keep the manager
   stopped until its recovered state is inspected; restore original state only
   deliberately. Review/reapply host systemd/nft settings separately if needed.

Do not use destructive Git reset or run latest `make deploy`/`make refresh` during
recovery: that can replace the configuration and state you are trying to restore.
The script does not perform automatic rollback or change the main deployment flow.

Local mocked tests (no Docker, SSH, production state or root paths accessed):

```bash
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p 'test_snapshot_router.py' -v
```
