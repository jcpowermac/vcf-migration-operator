#!/usr/bin/env python3
"""Isolated regression tests: python3 hack/test-sync-main.py."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("sync-main.sh").resolve()


class SyncMainTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        self.repo = root / "work"
        self.repo.mkdir()
        self.env = dict(os.environ, GIT_CONFIG_NOSYSTEM="1",
                        GIT_CONFIG_GLOBAL=os.devnull, GIT_TERMINAL_PROMPT="0")
        self.git("init", "-b", "main")
        self.git("config", "user.name", "Test")
        self.git("config", "user.email", "test@example.invalid")
        (self.repo / "base").write_text("base\n")
        self.git("add", ".")
        self.git("commit", "-m", "base")
        self.git("branch", "v0.1")
        for content in ("first\n", "second\n"):
            (self.repo / "feature").write_text(content)
            self.git("add", ".")
            self.git("commit", "-m", content.strip())
        self.picks = self.git("rev-list", "--reverse", "v0.1..main").split()
        upstream = root / "openshift" / "vcf-migration-operator.git"
        upstream.parent.mkdir()
        self.git("clone", "--bare", str(self.repo), str(upstream))
        fork = root / "fork.git"
        self.git("clone", "--bare", str(self.repo), str(fork))
        self.git("remote", "add", "upstream", str(upstream))
        self.git("remote", "add", "origin", "https://github.com/test/fork.git")
        self.git("config", "url." + str(fork) + ".insteadOf", "https://github.com/test/fork.git")
        self.git("checkout", "-b", "sync-main-to-v0.1", "v0.1")
        self.git("cherry-pick", "-x", *self.picks)
        bin_dir = root / "bin"
        bin_dir.mkdir()
        gh = bin_dir / "gh"
        gh.write_text('''#!/bin/sh
if [ "$1" = api ]; then
    case "$*" in
        *"state=open"*"head=test:sync-main-to-v0.1"*) printf '[{"number":42}]'; exit 0 ;;
    esac
fi
printf 'unexpected gh invocation: %s\\n' "$*" >&2
exit 1
''')
        gh.chmod(0o755)
        # Rewrite only transport; get-url must still expose the GitHub owner.
        git = bin_dir / "git"
        git.write_text('#!/bin/sh\nif [ "$*" = "remote get-url origin" ]; then\n'
                       'echo https://github.com/test/fork.git\nelse\n'
                       f'exec "{shutil.which("git")}" "$@"\nfi\n')
        git.chmod(0o755)
        self.env["PATH"] = str(bin_dir) + os.pathsep + self.env["PATH"]

    def git(self, *args):
        return subprocess.check_output(["git", *args], cwd=self.repo,
                                       env=self.env, stderr=subprocess.PIPE, text=True).strip()

    def run_sync(self, *args):
        result = subprocess.run(["/bin/bash", str(SCRIPT), "v0.1", *args],
                                cwd=self.repo, env=self.env, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        return result.stdout

    def test_merged_sync_is_not_replayed(self):
        self.git("checkout", "v0.1")
        self.git("merge", "--no-ff", "sync-main-to-v0.1", "-m", "merge sync")
        self.git("push", "upstream", "v0.1")
        for keep_branch in (True, False):
            with self.subTest(keep_branch=keep_branch):
                if not keep_branch:
                    self.git("branch", "-D", "sync-main-to-v0.1")
                self.assertIn("nothing new to cherry-pick", self.run_sync("--list"))

    def test_resume_fast_forwards_stale_local_branch(self):
        tip = self.git("rev-parse", "HEAD")
        self.git("push", "origin", "HEAD:sync-main-to-v0.1")
        self.git("reset", "--hard", "HEAD~1")
        self.run_sync("--list")
        self.assertEqual(self.git("rev-parse", "HEAD"), tip)

    def test_resume_updates_branch_without_switching_checkout(self):
        tip = self.git("rev-parse", "HEAD")
        self.git("push", "origin", "HEAD:sync-main-to-v0.1")
        self.git("reset", "--hard", "HEAD~1")
        self.git("checkout", "main")
        self.run_sync("--list")
        self.assertEqual(self.git("rev-parse", "sync-main-to-v0.1"), tip)
        self.assertEqual(self.git("branch", "--show-current"), "main")

    def test_resume_keeps_local_commits_ahead_of_fork(self):
        self.git("push", "origin", "HEAD~1:refs/heads/sync-main-to-v0.1")
        tip = self.git("rev-parse", "HEAD")
        self.run_sync("--list")
        self.assertEqual(self.git("rev-parse", "HEAD"), tip)

    def test_resume_restores_deleted_local_branch(self):
        tip = self.git("rev-parse", "HEAD")
        self.git("push", "origin", "HEAD:sync-main-to-v0.1")
        self.git("checkout", "main")
        self.git("branch", "-D", "sync-main-to-v0.1")
        self.run_sync("--list")
        self.assertEqual(self.git("rev-parse", "sync-main-to-v0.1"), tip)

    def test_resume_rejects_divergence(self):
        self.git("push", "origin", "HEAD:sync-main-to-v0.1")
        self.git("reset", "--hard", "HEAD~1")
        self.git("commit", "--allow-empty", "-m", "local divergence")
        tip = self.git("rev-parse", "HEAD")
        result = subprocess.run(["/bin/bash", str(SCRIPT), "v0.1", "--list"],
                                cwd=self.repo, env=self.env, capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("diverged", result.stderr)
        self.assertEqual(self.git("rev-parse", "HEAD"), tip)

    def test_empty_picks_do_not_push_or_query_prs(self):
        self.git("checkout", "v0.1")
        self.git("cherry-pick", self.picks[0])
        self.git("commit", "--amend", "-m", "already landed without trailer")
        self.git("push", "upstream", "v0.1")
        self.git("branch", "-f", "main", self.picks[0])
        self.git("push", "--force", "upstream", "main")
        self.git("branch", "-D", "sync-main-to-v0.1")
        gh = Path(self.tmp.name) / "bin" / "gh"
        gh.write_text("#!/bin/sh\necho unexpected gh invocation >&2\nexit 1\n")
        output = self.run_sync("--yes")
        self.assertIn("picking ", output)
        self.assertIn("nothing to push", output)
        self.assertEqual(self.git("rev-list", "--count",
                                  "upstream/v0.1..sync-main-to-v0.1"), "0")
        self.assertEqual(self.git("ls-remote", "origin",
                                  "refs/heads/sync-main-to-v0.1"), "")

    def test_new_picks_still_push(self):
        self.git("checkout", "v0.1")
        self.git("branch", "-D", "sync-main-to-v0.1")
        self.assertIn("PR already open", self.run_sync("--yes"))
        remote = self.git("ls-remote", "origin", "refs/heads/sync-main-to-v0.1")
        self.assertEqual(remote.split()[0], self.git("rev-parse", "HEAD"))

    def test_empty_plan_resumes_and_finds_existing_pr(self):
        self.assertIn("PR already open", self.run_sync("--yes"))


if __name__ == "__main__":
    unittest.main()
