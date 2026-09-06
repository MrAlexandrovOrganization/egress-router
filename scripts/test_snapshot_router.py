"""No real Docker, host files, root privileges or backup commands are used."""

import contextlib
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import stat
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import patch


SPEC = importlib.util.spec_from_file_location("snapshot_router", Path(__file__).with_name("snapshot-router.py"))
assert SPEC is not None and SPEC.loader is not None
snapshot = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(snapshot)


class SnapshotTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.project = self.root / "project"
        self.backup = self.root / "backup"
        self.project.mkdir()
        self.backup.mkdir(mode=0o700)
        for name in (".git", "runtime", "state", "subscription-manager"):
            (self.project / name).mkdir()
        for name in (".env", "config.json", "runtime/active.json", "state/subscriptions.json",
                     "subscription-manager/subscriptions.json", ".git/secret", ".deploy.lock"):
            (self.project / name).write_text("private-test-fixture")
        (self.project / "link").symlink_to("config.json")
        self.calls = []
        self.fail_command = None
        self.running = True
        self.paused = False
        self.same_image = False
        self.invalid_config = False
        self.nft_ok = False
        self.output = io.StringIO()
        self.patches = [
            patch.object(snapshot.subprocess, "run", side_effect=self.child),
            patch.object(snapshot, "HOST_FILES", (str(self.root / "missing-host-file"),)),
            patch.object(snapshot.shutil, "disk_usage", return_value=type("Disk", (), {"free": 10**12})()),
        ]
        for mocked in self.patches:
            mocked.start()
            self.addCleanup(mocked.stop)

    def image(self, index):
        return "sha256:" + ("a" if index == 0 or self.same_image else "b") * 64

    def child(self, args, **kwargs):
        self.calls.append(args)
        self.assertIs(kwargs["stderr"], subprocess.DEVNULL)
        self.assertIs(kwargs["stdin"], subprocess.DEVNULL)
        out = kwargs["stdout"]
        self.assertEqual(stat.S_IMODE(os.fstat(out.fileno()).st_mode), 0o600)
        command = " ".join(args)
        if self.fail_command and self.fail_command in command:
            out.write(b"private-child-failure")
            return subprocess.CompletedProcess(args, 1)
        if args[0] == "git":
            self.assertIn("--no-optional-locks", args)
            self.assertIn(f"safe.directory={self.project}", args)
            if "verify" not in args:
                self.assertEqual(kwargs["user"], self.project.stat().st_uid)
            if "rev-parse" in args:
                out.write(b"c" * 40 + b"\n")
            else:
                out.write(b"private-git-output")
        elif args[:3] == ["docker", "inspect", "--type"]:
            index = snapshot.SERVICES.index(args[-1])
            out.write(json.dumps([dict(Name="/" + args[-1], Id=f"container{index}",
                                      Image=self.image(index),
                                      State=dict(Running=True if index == 0 else self.running,
                                                 Paused=False if index == 0 else self.paused))]).encode())
        elif args[:3] == ["docker", "image", "inspect"]:
            out.write(json.dumps([dict(Id=image, Size=1024) for image in args[3:]]).encode())
        elif args[:2] == ["docker", "cp"] or args[:3] == ["docker", "image", "save"]:
            with tarfile.open(fileobj=out, mode="w") as archive:
                data = b"not-json" if self.invalid_config else b'{"private": "mounted-inode"}'
                member = tarfile.TarInfo("config.json")
                member.size = len(data)
                archive.addfile(member, io.BytesIO(data))
        elif args[0] == "nft":
            if self.nft_ok:
                out.write(b'{"nftables": []}')
            else:
                return subprocess.CompletedProcess(args, 1)
        return subprocess.CompletedProcess(args, 0)

    def capture(self):
        with contextlib.redirect_stdout(self.output):
            snapshot.capture(self.project, self.backup, "test-timestamp")

    def test_success_private_artifacts_and_order(self):
        self.capture()
        self.assertTrue((self.backup / "COMPLETE").exists())
        self.assertNotIn("private", self.output.getvalue())
        self.assertNotIn("mounted-inode", self.output.getvalue())
        self.assertEqual(json.loads((self.backup / "running-config.json").read_text()),
                         {"private": "mounted-inode"})
        with tarfile.open(self.backup / "worktree.tar") as archive:
            names = archive.getnames()
            for name in (".env", "config.json", "runtime/active.json", "state/subscriptions.json",
                         "subscription-manager/subscriptions.json"):
                self.assertIn(name, names)
            self.assertNotIn(".git", names)
            self.assertNotIn(".git/secret", names)
            self.assertNotIn(".deploy.lock", names)
            self.assertTrue(archive.getmember("link").issym())
        pause = self.calls.index(["docker", "pause", "container1"])
        copy = self.calls.index(["docker", "cp", "container0:/etc/sing-box/config.json", "-"])
        unpause = self.calls.index(["docker", "unpause", "container1"])
        save = next(i for i, c in enumerate(self.calls) if c[:3] == ["docker", "image", "save"])
        self.assertLess(pause, copy)
        self.assertLess(copy, unpause)
        self.assertLess(unpause, save)
        for path in self.backup.iterdir():
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
        for line in (self.backup / "SHA256SUMS").read_text().splitlines():
            digest, name = line.split("  ")
            self.assertEqual(digest, hashlib.sha256((self.backup / name).read_bytes()).hexdigest())
        self.assertIsNone(json.loads((self.backup / "metadata.json").read_text())["nft_table_exists"])
        self.assertFalse(any(c[:2] in (["docker", "stop"], ["docker", "run"]) for c in self.calls))

    def test_cp_failure_unpauses_and_retains_partial(self):
        self.fail_command = "docker cp"
        with self.assertRaises(RuntimeError):
            self.capture()
        self.assertIn(["docker", "unpause", "container1"], self.calls)
        self.assertTrue((self.backup / "repository.bundle").exists())
        self.assertFalse((self.backup / "COMPLETE").exists())
        self.assertNotIn("private", self.output.getvalue())

    def test_pause_failure_still_attempts_unpause(self):
        self.fail_command = "docker pause"
        with self.assertRaises(RuntimeError):
            self.capture()
        self.assertIn(["docker", "unpause", "container1"], self.calls)
        self.assertFalse((self.backup / "COMPLETE").exists())

    def test_unpause_failure_blocks_completion(self):
        self.fail_command = "docker unpause"
        with self.assertRaises(RuntimeError):
            self.capture()
        self.assertFalse((self.backup / "COMPLETE").exists())
        self.assertFalse(any(c[:3] == ["docker", "image", "save"] for c in self.calls))

    def test_tar_failure_unpauses(self):
        original = snapshot.tarfile.open

        def tar_open(*args, **kwargs):
            if kwargs.get("mode") == "w" and "fileobj" in kwargs and str(kwargs["fileobj"].name).endswith("worktree.tar"):
                raise OSError("private-error")
            return original(*args, **kwargs)

        with patch.object(snapshot.tarfile, "open", side_effect=tar_open):
            with self.assertRaises(OSError):
                self.capture()
        self.assertIn(["docker", "unpause", "container1"], self.calls)

    def test_already_paused_manager_untouched(self):
        self.paused = True
        self.capture()
        self.assertFalse(any(c[:2] in (["docker", "pause"], ["docker", "unpause"]) for c in self.calls))

    def test_stopped_manager_untouched(self):
        self.running = False
        self.capture()
        self.assertFalse(any(c[:2] in (["docker", "pause"], ["docker", "unpause"]) for c in self.calls))

    def test_image_deduplication(self):
        self.same_image = True
        self.capture()
        save, = [c for c in self.calls if c[:3] == ["docker", "image", "save"]]
        self.assertEqual(save[3:], [self.image(0)])

    def test_preflight_before_pause(self):
        with patch.object(snapshot.shutil, "disk_usage", return_value=type("Disk", (), {"free": 0})()):
            with self.assertRaises(RuntimeError):
                self.capture()
        self.assertFalse(any(c[:2] == ["docker", "pause"] for c in self.calls))

    def test_invalid_json_has_no_complete(self):
        self.invalid_config = True
        with self.assertRaises(ValueError):
            self.capture()
        self.assertIn(["docker", "unpause", "container1"], self.calls)
        self.assertFalse((self.backup / "COMPLETE").exists())

    def test_image_save_failure_after_unpause(self):
        self.fail_command = "docker image save"
        with self.assertRaises(RuntimeError):
            self.capture()
        self.assertIn(["docker", "unpause", "container1"], self.calls)
        self.assertFalse((self.backup / "COMPLETE").exists())

    def test_bundle_verification_failure(self):
        self.fail_command = "bundle verify"
        with self.assertRaises(RuntimeError):
            self.capture()
        self.assertFalse((self.backup / "COMPLETE").exists())

    def test_rejects_symlinks_and_nested_destination(self):
        link = self.root / "project-link"
        link.symlink_to(self.project, target_is_directory=True)
        for project, dest in ((link, self.backup), (self.project, link / "child"),
                              (self.project, self.project / "backups")):
            with self.assertRaises(RuntimeError):
                snapshot.snapshot(project, dest)
        self.assertEqual(self.calls, [])

    def test_rejects_insecure_destination(self):
        self.backup.chmod(0o755)
        with self.assertRaises(RuntimeError):
            snapshot.snapshot(self.project, self.backup)
        self.assertEqual(self.calls, [])

    def test_nft_success_and_host_file(self):
        self.nft_ok = True
        host = self.root / "installed-unit"
        host.write_text("private-unit")
        with patch.object(snapshot, "HOST_FILES", (str(host),)):
            self.capture()
        metadata = json.loads((self.backup / "metadata.json").read_text())
        self.assertTrue(metadata["nft_table_exists"])
        self.assertEqual((self.backup / "host-0").read_text(), "private-unit")

    def root_destination(self):
        original = Path.stat

        def path_stat(path, *args, **kwargs):
            attrs = original(path, *args, **kwargs)
            if path == self.backup:
                fields = list(attrs)
                fields[4] = 0
                return os.stat_result(fields)
            return attrs

        return patch.object(Path, "stat", path_stat)

    def test_existing_lock_and_unique_directories(self):
        locks = []

        def flock(fd, operation):
            self.assertEqual(operation, snapshot.fcntl.LOCK_EX)
            self.assertEqual(os.fstat(fd.fileno()).st_ino, (self.project / ".deploy.lock").stat().st_ino)
            locks.append(fd)

        with self.root_destination(), patch.object(snapshot.fcntl, "flock", side_effect=flock):
            with contextlib.redirect_stdout(self.output):
                first = snapshot.snapshot(self.project, self.backup)
                second = snapshot.snapshot(self.project, self.backup)
        self.assertNotEqual(first, second)
        self.assertEqual(len(locks), 2)
        self.assertEqual(stat.S_IMODE(first.stat().st_mode), 0o700)
        self.assertTrue((first / "COMPLETE").exists())

    def test_lock_missing_or_symlink_rejected(self):
        lock = self.project / ".deploy.lock"
        lock.unlink()
        with self.root_destination(), contextlib.redirect_stderr(self.output):
            with self.assertRaises(OSError):
                snapshot.snapshot(self.project, self.backup)
            lock.symlink_to(self.project / "config.json")
            with self.assertRaises(OSError):
                snapshot.snapshot(self.project, self.backup)
        self.assertEqual(self.calls, [])

    def test_nonroot_destination_rejected(self):
        original = Path.stat

        def path_stat(path, *args, **kwargs):
            attrs = original(path, *args, **kwargs)
            if path == self.backup:
                fields = list(attrs)
                fields[4] = 12345
                return os.stat_result(fields)
            return attrs

        with patch.object(Path, "stat", path_stat):
            with self.assertRaises(RuntimeError):
                snapshot.snapshot(self.project, self.backup)

    def test_interruption_during_copy_attempts_unpause(self):
        child = self.child

        def interrupt(args, **kwargs):
            if args[:2] == ["docker", "cp"]:
                raise KeyboardInterrupt()
            return child(args, **kwargs)

        with patch.object(snapshot.subprocess, "run", side_effect=interrupt):
            with self.assertRaises(KeyboardInterrupt):
                self.capture()
        self.assertIn(["docker", "unpause", "container1"], self.calls)
        self.assertFalse((self.backup / "COMPLETE").exists())

    def test_installed_host_symlink_rejected(self):
        host = self.root / "installed-link"
        host.symlink_to(self.project / "config.json")
        with patch.object(snapshot, "HOST_FILES", (str(host),)):
            with self.assertRaises(RuntimeError):
                self.capture()
        self.assertFalse((self.backup / "COMPLETE").exists())


if __name__ == "__main__":
    unittest.main()
