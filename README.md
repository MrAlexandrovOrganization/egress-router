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
client timeout). ANY invalid node fails the entire refresh with HTTP 400, including
unsupported VMess nodes. The previous generated config is preserved on failure.
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
