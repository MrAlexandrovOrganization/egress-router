#!/usr/bin/env python3
"""Offline-verifiable, secret-safe pre-deployment snapshot. Linux/root only."""

import argparse
import datetime
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import stat
import subprocess
import sys
import tarfile
import tempfile


HOST_FILES = (
    "/usr/local/sbin/telemt-egress-tproxy",
    "/etc/systemd/system/telemt-egress-tproxy.service",
    "/etc/default/telemt-egress-tproxy",
)
SERVICES = ("egress-router", "subscription-manager")


def checked_path(value):
    path = Path(os.path.abspath(value))
    for part in (*reversed(path.parents), path):
        if part.is_symlink():
            raise RuntimeError("Symlink path rejected")
    return path


def write(path, data):
    with path.open("xb") as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write(data.encode() if isinstance(data, str) else data)


def run(args, output, *, optional=False, owner=None):
    kwargs = {}
    if owner is not None:
        kwargs.update(user=owner.st_uid, group=owner.st_gid, extra_groups=[])
    with output.open("xb") as stream:
        os.fchmod(stream.fileno(), 0o600)
        result = subprocess.run(
            args, stdout=stream, stderr=subprocess.DEVNULL, stdin=subprocess.DEVNULL,
            env={"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
                 "HOME": "/", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null",
                 "GIT_TERMINAL_PROMPT": "0"}, **kwargs,
        )
    if result.returncode and not optional:
        raise RuntimeError("Child command failed; output retained privately")
    return result.returncode == 0


def tree_entries(project):
    for root, dirs, files in os.walk(project, followlinks=False):
        if Path(root) == project:
            dirs[:] = [name for name in dirs if name != ".git"]
            files = [name for name in files if name not in (".git", ".deploy.lock")]
        for name in dirs + files:
            yield Path(root) / name


def restore_text():
    return """# Manual Recovery

This directory contains credentials. Keep it root-only, off Git and public storage.
COMPLETE means archive/JSON/hash checks passed, NOT sing-box or network validation.
Keep a fresh snapshot of the current deployment before making any recovery changes.
Never run git reset, make deploy, make refresh, or a latest-image pull to recover.

1. Verify `sha256sum -c SHA256SUMS` privately; require COMPLETE. Read metadata.json
   and container inspect files privately for original image IDs, mounts, users,
   manager running/paused state, and host parameters. Git refs/status are in git-*.
2. Hold the ORIGINAL project's lock in a root shell for the entire recovery:
   `exec 9<>/original/project/.deploy.lock; flock -x 9`.
   Stop subscription-manager (unpause first if necessary), not the router yet.
   Keep all other writers and deployment/API callers quiescent.
3. Create a separate root-only recovery directory outside the original checkout.
   Set BACKUP and RECOVERY to absolute paths in this root shell; set umask 077.
   `mkdir -m 700 "$RECOVERY"`
   `tar -xpf "$BACKUP/worktree.tar" -C "$RECOVERY"`
   This restores ignored .env, base config, runtime, state and legacy subscriptions,
   including original tar ownership/modes. Inspect symlinks before accessing files.
   Git history is separate: clone repository.bundle elsewhere if needed; no reset.
4. `docker image load -i "$BACKUP/images.tar"` (keep output private).
   Inspect recovered docker-compose.yaml/.env and saved inspect JSON. Correct
   absolute binds, UID/GID/group access and host-specific settings manually.
   The saved running-config.json is authoritative, not a newer host runtime file.
   Replace recovered runtime/active.json with its contents, retaining the intended
   owner/group and readable-by-router mode (normally 0640). Reject symlink targets.
   For a legacy Compose file, change its router config bind to this active.json.
   Inspect merged Compose privately: it must use recovered paths and saved image
   IDs, never build/pull. Do not blindly replay inspect JSON as container options.
5. Validate running-config.json with the SAVED router image, not a floating tag:
   `docker run --rm --pull never --network none --entrypoint sing-box -v "$BACKUP/running-config.json:/config.json:ro" SAVED_ROUTER_IMAGE_ID check -c /config.json`
   This may fail if validation needs DNS or other mounts; investigate, do not
   claim successful validation or activate unverified configuration.
6. Recreate ONLY the router after reviewing mounts and image overrides:
   `docker compose -p egress-router -f "$RECOVERY/docker-compose.yaml" -f "$BACKUP/image-override.yaml" up -d --no-build --pull never --no-deps --force-recreate egress-router`
   Verify container health and real HTTPS through its proxy, e.g.
   `curl --fail --silent --show-error --max-time 15 --noproxy '' --proxy http://127.0.0.1:10880 https://cp.cloudflare.com/generate_204 -o /dev/null`.
   Check telemt routing separately. If this fails, keep the manager stopped and
   use the fresh pre-recovery snapshot; do not generate replacement config.
7. Keep the manager stopped until recovered state, binds and credentials have
   been inspected. If originally running, optionally recreate ONLY that service
   with the same two Compose files and --no-build --pull never --no-deps. Restore
   its original paused state if applicable. Startup can refresh generated state.
   Decide which checkout will own future deploys; do not leave two active stacks.
8. host-* are installed host files, not automatically applied. Review/reinstall
   deliberately with original ownership/modes from metadata, then daemon-reload
   and reload the unit only after router readiness. nft-telemt.json covers ONLY
   table ip telemt_tproxy. A failed query means absent OR unavailable, not proof
   of absence; metadata records this uncertainty. Never flush the host ruleset.

Scope: no Docker writable layers, external mounts/volumes, other services, full
host firewall, systemd enablement state or remote Git objects. Git --all captures
local refs, not reflog-only/unreachable objects. Pausing stops writer execution
but is not an application transaction checkpoint; inspect state before restart.
SIGKILL/power loss cannot unpause: check subscription-manager manually after any
interruption. Partial snapshots are retained and must not be treated as COMPLETE.
"""


def snapshot(project, destination):
    project = checked_path(project)
    destination = checked_path(destination)
    if not project.is_dir() or destination == project or project in destination.parents:
        raise RuntimeError("Invalid project/destination")
    destination.mkdir(mode=0o700, parents=True, exist_ok=True)
    dst = destination.stat()
    if dst.st_uid != 0 or stat.S_IMODE(dst.st_mode) != 0o700:
        raise RuntimeError("Destination must be root-owned mode 0700")
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
    backup = Path(tempfile.mkdtemp(prefix=stamp + "-", dir=destination))
    os.chmod(backup, 0o700)
    # No exception text or child output reaches the terminal (may contain secrets).
    try:
        lock = os.open(project / ".deploy.lock", os.O_RDWR | os.O_NOFOLLOW)
        with os.fdopen(lock, "r+") as stream:
            if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
                raise RuntimeError("Invalid lock")
            fcntl.flock(stream, fcntl.LOCK_EX)
            capture(project, backup, stamp)
    except BaseException:
        print(f"Snapshot failed; partial directory retained: {backup}", file=sys.stderr)
        raise
    return backup


def capture(project, backup, stamp):
    owner = project.stat()
    git = ["git", "--no-optional-locks", "-c", f"safe.directory={project}", "-C", str(project)]
    for name, args in (
        ("head", ["rev-parse", "--verify", "HEAD"]),
        ("branches", ["show-ref", "--head"]),
        ("status", ["status", "--porcelain=v2", "--branch", "--untracked-files=all"]),
        ("files", ["ls-files", "--stage", "-z"]),
    ):
        run(git + args, backup / f"git-{name}.txt", owner=owner)
    sha = (backup / "git-head.txt").read_text().strip()
    if not re.fullmatch(r"[0-9a-f]{40,64}", sha):
        raise RuntimeError("Invalid revision")
    # stdout streaming allows the unprivileged Git owner no access to the backup.
    run(git + ["bundle", "create", "-", "--all"], backup / "repository.bundle", owner=owner)
    containers = []
    for service in SERVICES:
        path = backup / f"inspect-{service}.json"
        run(["docker", "inspect", "--type", "container", service], path)
        info, = json.loads(path.read_bytes())
        if info["Name"] != "/" + service:
            raise RuntimeError("Unexpected container")
        containers.append(info)
    if not containers[0]["State"]["Running"]:
        raise RuntimeError("Router must be running")
    images = list(dict.fromkeys(info["Image"] for info in containers))
    if any(not re.fullmatch(r"sha256:[0-9a-f]{64}", image) for image in images):
        raise RuntimeError("Invalid image ID")
    run(["docker", "image", "inspect", *images], backup / "inspect-images.json")
    image_info = json.loads((backup / "inspect-images.json").read_bytes())
    if {item["Id"] for item in image_info} != set(images):
        raise RuntimeError("Image inspection mismatch")
    tree_size = sum(((p.lstat().st_size + 511) // 512 + 4) * 512 for p in tree_entries(project))
    required = sum(item["Size"] for item in image_info) + tree_size + 512 * 1024**2
    if shutil.disk_usage(backup).free < required:
        raise RuntimeError("Insufficient free space")
    manager = containers[1]
    pause = manager["State"]["Running"] and not manager["State"]["Paused"]
    try:
        # Arm cleanup BEFORE pause: a failing client may have paused the daemon.
        if pause:
            run(["docker", "pause", manager["Id"]], backup / "manager-pause.txt")
        run(["docker", "cp", containers[0]["Id"] + ":/etc/sing-box/config.json", "-"],
            backup / "running-config.tar")
        with tarfile.open(backup / "running-config.tar", "r:") as archive:
            members = archive.getmembers()
            if len(members) != 1 or not members[0].isfile():
                raise RuntimeError("Unexpected mounted config archive")
            source = archive.extractfile(members[0])
            if source is None:
                raise RuntimeError("Missing config contents")
            with source:
                with (backup / "running-config.json").open("xb") as target:
                    os.fchmod(target.fileno(), 0o600)
                    shutil.copyfileobj(source, target)
        with (backup / "worktree.tar").open("xb") as target:
            os.fchmod(target.fileno(), 0o600)
            with tarfile.open(fileobj=target, mode="w", dereference=False) as archive:
                for path in tree_entries(project):
                    if not (path.is_symlink() or path.is_file() or path.is_dir()):
                        raise RuntimeError("Unsupported worktree special file")
                    archive.add(path, arcname=str(path.relative_to(project)), recursive=False)
    finally:
        if pause:
            # Delay further termination signals until the unpause attempt finishes.
            old = signal.pthread_sigmask(signal.SIG_BLOCK, {signal.SIGINT, signal.SIGTERM, signal.SIGHUP})
            try:
                run(["docker", "unpause", manager["Id"]], backup / "manager-unpause.txt")
            finally:
                signal.pthread_sigmask(signal.SIG_SETMASK, old)
    run(["docker", "image", "save", *images], backup / "images.tar")
    host = {}
    for index, name in enumerate(HOST_FILES):
        source = checked_path(name)
        if source.exists():
            attrs = source.stat()
            if not stat.S_ISREG(attrs.st_mode):
                raise RuntimeError("Invalid host file")
            artifact = f"host-{index}"
            with source.open("rb") as src, (backup / artifact).open("xb") as dst:
                os.fchmod(dst.fileno(), 0o600)
                shutil.copyfileobj(src, dst)
            host[name] = dict(artifact=artifact, uid=attrs.st_uid, gid=attrs.st_gid,
                              mode=stat.S_IMODE(attrs.st_mode))
        else:
            host[name] = None
    try:
        nft_ok = run(["nft", "-j", "list", "table", "ip", "telemt_tproxy"],
                     backup / "nft-telemt.json", optional=True)
    except FileNotFoundError:
        nft_ok = False
    if nft_ok:
        json.loads((backup / "nft-telemt.json").read_bytes())
    write(backup / "image-override.yaml", "services:\n" + "".join(
        f"  {service}:\n    image: {info['Image']}\n    pull_policy: never\n"
        for service, info in zip(SERVICES, containers)))
    write(backup / "RESTORE.md", restore_text())
    for name in ("worktree.tar", "images.tar", "running-config.tar"):
        with tarfile.open(backup / name, "r:") as archive:
            for member in archive:
                if member.isfile():
                    content = archive.extractfile(member)
                    if content is None:
                        raise RuntimeError("Missing archive contents")
                    with content:
                        while content.read(1024 * 1024):
                            pass
    json.loads((backup / "running-config.json").read_bytes())
    run(git + ["bundle", "verify", str(backup / "repository.bundle")], backup / "bundle-verify.txt")
    metadata = dict(started=stamp, finished=datetime.datetime.now(datetime.timezone.utc).isoformat(),
                    original_sha=sha, image_ids=images, host_files=host,
                    nft_table_exists=True if nft_ok else None, nft_query_succeeded=nft_ok,
                    manager_running=manager["State"]["Running"], manager_paused=manager["State"]["Paused"],
                    checks=["tar-readable", "git-bundle-verify", "running-config-json", "sha256"])
    write(backup / "metadata.json", json.dumps(metadata, indent=2) + "\n")
    hashes = []
    for path in sorted(backup.iterdir()):
        digest = hashlib.sha256()
        with path.open("rb") as source:
            while chunk := source.read(1024 * 1024):
                digest.update(chunk)
        hashes.append(f"{digest.hexdigest()}  {path.name}\n")
    write(backup / "SHA256SUMS", "".join(hashes))
    write(backup / "COMPLETE", "Verified archive/JSON/bundle checks; SHA256SUMS records artifacts.\n")
    print(f"Backup directory: {backup}\nOriginal SHA: {sha}\nImage count: {len(images)}\n"
          "Checks passed: tar readability, Git bundle, JSON, SHA256 manifest")


def interrupted(signum, frame):
    raise RuntimeError("Interrupted")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--project", default="/home/maxim/projects/infra/network/egress-router")
    parser.add_argument("--destination", default="/var/backups/egress-router")
    args = parser.parse_args()
    if sys.platform != "linux" or os.geteuid() != 0:
        print("Requires Linux and root; no snapshot created.", file=sys.stderr)
        return 1
    os.umask(0o077)
    for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        signal.signal(sig, interrupted)
    try:
        snapshot(args.project, args.destination)
    except BaseException:
        print("Snapshot not complete. Check manager pause state before deploying.", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
