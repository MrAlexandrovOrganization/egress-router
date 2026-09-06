"""Exercise the actual SSH script with local Git repos and fake make/flock."""

import os
from pathlib import Path
import shlex
import subprocess
import tempfile
import textwrap
import unittest


WORKFLOW = Path(__file__).resolve().parents[1] / ".github/workflows/ci-cd.yml"


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
            "GIT_CONFIG_NOSYSTEM": "1",
            "GIT_CONFIG_GLOBAL": os.devnull,
            "GIT_TERMINAL_PROMPT": "0",
            "GIT_AUTHOR_NAME": "CI Test",
            "GIT_AUTHOR_EMAIL": "ci@example.invalid",
            "GIT_COMMITTER_NAME": "CI Test",
            "GIT_COMMITTER_EMAIL": "ci@example.invalid",
        }
        for name, body in {
            "flock": 'test "$*" = "-x 9"\ntest -e .deploy.lock\nprintf "flock\\n"',
            "make": 'printf "make %s locked=%s branch=%s head=%s\\n" "$*" '
                    '"${DEPLOY_LOCKED:-}" "$(git symbolic-ref --short HEAD)" "$(git rev-parse HEAD)"',
        }.items():
            path = self.bin / name
            path.write_text("#!/bin/sh\nset -eu\n{\n" + body + "\n} >> "
                            + shlex.quote(str(self.calls)) + "\n")
            path.chmod(0o755)
        self.git(self.root, "init", "-b", "main", str(self.origin))
        self.base = self.commit(self.origin, "base")
        self.git(self.root, "clone", str(self.origin), str(self.server))
        for index in range(3):
            self.tip = self.commit(self.origin, f"upstream-{index}")

    def git(self, repo, *args):
        return subprocess.run(
            ["git", *args], cwd=repo, env=self.env, check=True,
            text=True, capture_output=True,
        ).stdout.strip()

    def commit(self, repo, name):
        (repo / name).write_text(name)
        self.git(repo, "add", name)
        self.git(repo, "commit", "-m", name)
        return self.git(repo, "rev-parse", "HEAD")

    def deploy(self, sha=None):
        lines = WORKFLOW.read_text().splitlines()
        start = lines.index("          script: |") + 1
        end = start
        while end < len(lines) and (not lines[end].strip() or lines[end].startswith("            ")):
            end += 1
        script = textwrap.dedent("\n".join(lines[start:end]))
        script = script.replace("${{ secrets.VM_PROJECT_PATH }}", str(self.server))
        script = script.replace("${{ github.sha }}", sha or self.tip)
        self.assertNotIn("${{", script)
        return subprocess.run(["sh", "-c", script], cwd=self.root, env=self.env,
                              text=True, capture_output=True)

    def assert_deployed(self, result):
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.git(self.server, "symbolic-ref", "--short", "HEAD"), "main")
        self.assertEqual(self.git(self.server, "rev-parse", "HEAD"), self.tip)
        self.assertEqual(self.calls.read_text().splitlines(), [
            "flock",
            f"make deploy locked=1 branch=main head={self.tip}",
            f"make status locked= branch=main head={self.tip}",
        ])

    def assert_refused(self, result, main):
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.calls.read_text(), "flock\n")
        self.assertEqual(self.git(self.server, "rev-parse", "main"), main)

    def test_main_behind_three_fast_forwards(self):
        self.assert_deployed(self.deploy())

    def test_detached_checkout_returns_to_main(self):
        self.git(self.server, "checkout", "--detach", self.base)
        self.assert_deployed(self.deploy())

    def test_main_already_at_tested_commit(self):
        self.git(self.server, "pull", "--ff-only", "origin", "main")
        self.assert_deployed(self.deploy())

    def test_stale_run_skips_without_switching_or_deploying(self):
        self.git(self.server, "checkout", "--detach", self.base)
        result = self.deploy(self.base)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Skipping deployment: origin/main no longer matches the tested commit.", result.stdout)
        self.assertEqual(self.calls.read_text(), "flock\n")
        self.assertEqual(self.git(self.server, "rev-parse", "HEAD"), self.base)
        self.assertEqual(self.git(self.server, "rev-parse", "main"), self.base)
        self.assertEqual(self.git(self.server, "rev-parse", "--abbrev-ref", "HEAD"), "HEAD")

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
        self.assert_refused(self.deploy("0" * 40), self.base)


if __name__ == "__main__":
    unittest.main()
