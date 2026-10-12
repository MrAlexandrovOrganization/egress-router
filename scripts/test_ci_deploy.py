"""Exercise the GitHub SSH wrapper and the actual standalone CI deploy Makefile."""

import os
from pathlib import Path
import subprocess
import sys
import tempfile
import textwrap
import unittest


ROOT = Path(__file__).resolve().parents[1]
WORKFLOW = ROOT / ".github/workflows/ci-cd.yml"
CI_DEPLOY = ROOT / "makefiles/ci-deploy.mk"


class CIDeployTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name).resolve()
        self.origin = self.root / "origin"
        self.server = self.root / "server"
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.calls = self.root / "calls"
        self.env = {
            "PATH": str(self.bin) + os.pathsep + os.environ["PATH"],
            "HOME": str(self.root),
            "TMPDIR": "/private/tmp" if sys.platform == "darwin" else "/tmp",
            "GIT_CONFIG_NOSYSTEM": "1",
            "GIT_CONFIG_GLOBAL": os.devnull,
            "GIT_TERMINAL_PROMPT": "0",
            "GIT_AUTHOR_NAME": "CI Test",
            "GIT_AUTHOR_EMAIL": "ci@example.invalid",
            "GIT_COMMITTER_NAME": "CI Test",
            "GIT_COMMITTER_EMAIL": "ci@example.invalid",
            "DEPLOY_TEST_CALLS": str(self.calls),
        }
        self.write_command(
            "flock",
            'test "$*" = "-w 1800 9"\n'
            'test -f .deploy.lock\nprintf "flock\\n" >> "$DEPLOY_TEST_CALLS"',
        )
        self.write_command(
            "docker",
            'test "$*" = "login ghcr.io -u ci-user --password-stdin"\n'
            'token=$(cat)\ntest "$token" = synthetic-token\n'
            'printf "docker-login\\n" >> "$DEPLOY_TEST_CALLS"',
        )
        self.git(self.root, "init", "-b", "main", str(self.origin))
        (self.origin / "makefiles").mkdir()
        (self.origin / "makefiles/ci-deploy.mk").write_bytes(CI_DEPLOY.read_bytes())
        (self.origin / ".gitignore").write_text(".deploy.lock\n")
        (self.origin / "Makefile").write_text(
            "include makefiles/ci-deploy.mk\n\n"
            ".PHONY: image-deploy\n"
            "image-deploy:\n"
            '\t@test "$${IMAGE_DEPLOY_LOCKED:-}" = 1\n'
            '\t@test -z "$${GHCR_TOKEN:-}"\n'
            '\t@printf "image-deploy\\n" >> "$${DEPLOY_TEST_CALLS}"\n'
        )
        self.base = self.commit(self.origin, "base")
        self.git(self.root, "clone", str(self.origin), str(self.server))
        self.tip = self.base
        for index in range(3):
            self.tip = self.commit(self.origin, f"upstream-{index}")

    def write_command(self, name, body):
        path = self.bin / name
        path.write_text("#!/bin/sh\nset -eu\n" + body + "\n")
        path.chmod(0o755)

    def git(self, repo, *args):
        return subprocess.run(
            ["git", *args], cwd=repo, env=self.env, check=True,
            text=True, capture_output=True,
        ).stdout.strip()

    def commit(self, repo, name):
        (repo / name).write_text(name)
        self.git(repo, "add", "-A")
        self.git(repo, "commit", "-m", name)
        return self.git(repo, "rev-parse", "HEAD")

    def deploy(self, sha=None):
        lines = WORKFLOW.read_text().splitlines()
        start = lines.index("          script: |") + 1
        end = start
        while end < len(lines) and (not lines[end].strip() or lines[end].startswith("            ")):
            end += 1
        script = textwrap.dedent("\n".join(lines[start:end]))
        self.assertNotIn("${{", script)
        env = self.env | {
            "PROJECT_PATH": str(self.server),
            "DEPLOY_SHA": sha or self.tip,
            "IMAGE_MANIFEST_B64": "e30=",
            "GHCR_USER": "ci-user",
            "GHCR_TOKEN": "synthetic-token",
        }
        return subprocess.run(["sh", "-c", script], cwd=self.root, env=env,
                              text=True, capture_output=True)

    def assert_deployed(self, result):
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.git(self.server, "symbolic-ref", "--short", "HEAD"), "main")
        self.assertEqual(self.git(self.server, "rev-parse", "HEAD"), self.tip)
        self.assertEqual(self.calls.read_text().splitlines(), [
            "flock", "docker-login", "image-deploy",
        ])

    def assert_refused(self, result, main):
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.calls.read_text().splitlines(), ["flock"])
        self.assertEqual(self.git(self.server, "rev-parse", "main"), main)

    def test_main_behind_three_fast_forwards(self):
        self.assert_deployed(self.deploy())

    def test_non_conflicting_untracked_backup_is_preserved(self):
        backup = self.server / "config.json.before-listen-fix-20260907T230645Z"
        backup.write_text("preserve this backup")
        self.assert_deployed(self.deploy())
        self.assertEqual(backup.read_text(), "preserve this backup")

    def test_untracked_file_obstructing_checkout_is_preserved(self):
        self.tip = self.commit(self.origin, "incoming-file")
        obstruction = self.server / "incoming-file"
        obstruction.write_text("preserve this untracked file")
        self.assert_refused(self.deploy(), self.base)
        self.assertEqual(obstruction.read_text(), "preserve this untracked file")

    def test_detached_checkout_is_preserved(self):
        self.git(self.server, "checkout", "--detach", self.base)
        result = self.deploy()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.git(self.server, "rev-parse", "HEAD"), self.base)
        self.assertEqual(self.calls.read_text().splitlines(), ["flock"])

    def test_main_already_at_tested_commit(self):
        self.git(self.server, "pull", "--ff-only", "origin", "main")
        self.assert_deployed(self.deploy())

    def test_stale_workflow_is_refused(self):
        self.assert_refused(self.deploy(self.base), self.base)

    def test_diverged_main_is_preserved(self):
        local = self.commit(self.server, "local")
        self.assert_refused(self.deploy(), local)

    def test_ahead_main_is_preserved(self):
        self.git(self.server, "pull", "--ff-only", "origin", "main")
        local = self.commit(self.server, "local")
        self.assert_refused(self.deploy(), local)

    def test_dirty_tracked_file_is_preserved(self):
        (self.server / "base").write_text("dirty")
        self.assert_refused(self.deploy(), self.base)
        self.assertEqual((self.server / "base").read_text(), "dirty")

    def test_invalid_tested_commit_is_refused(self):
        result = self.deploy("0" * 40)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.calls.exists())
        self.assertEqual(self.git(self.server, "rev-parse", "HEAD"), self.base)


if __name__ == "__main__":
    unittest.main()
