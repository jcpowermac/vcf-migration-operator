#!/usr/bin/env python3
"""Isolated regression tests: python3 hack/test-sync-main.py."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("sync-main.sh").resolve()
CONFLICT = "Conflict while cherry-picking"
# Inherited Git redirection must not leak into the fixtures or the script.
GIT_REDIRECT_VARS = (
    "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
    "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_NAMESPACE",
    "GIT_PREFIX", "GIT_QUARANTINE_PATH",
)


class SyncMainTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        self.repo = root / "work"
        self.repo.mkdir()
        self.env = dict(os.environ, GIT_CONFIG_NOSYSTEM="1",
                        GIT_CONFIG_GLOBAL=os.devnull, GIT_TERMINAL_PROMPT="0")
        for var in GIT_REDIRECT_VARS:
            self.env.pop(var, None)
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
        # Picks live on the sync branch, which is never checked out in the
        # invoking checkout (sync-main.sh uses its own worktree).
        self.git("checkout", "-b", "sync-main-to-v0.1", "v0.1")
        self.git("cherry-pick", "-x", *self.picks)
        self.git("checkout", "main")
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

    def run_sync_raw(self, *args):
        return subprocess.run(["/bin/bash", str(SCRIPT), "v0.1", *args],
                              cwd=self.repo, env=self.env, capture_output=True, text=True)

    def run_sync(self, *args):
        result = self.run_sync_raw(*args)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        return result.stdout

    def worktree(self):
        return self.repo / ".git" / "sync-main-to-v0.1"

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
        tip = self.git("rev-parse", "sync-main-to-v0.1")
        self.git("push", "origin", "sync-main-to-v0.1")
        self.git("branch", "-f", "sync-main-to-v0.1", "sync-main-to-v0.1~1")
        self.run_sync("--list")
        self.assertEqual(self.git("rev-parse", "sync-main-to-v0.1"), tip)

    def test_resume_updates_branch_without_switching_checkout(self):
        tip = self.git("rev-parse", "sync-main-to-v0.1")
        self.git("push", "origin", "sync-main-to-v0.1")
        self.git("branch", "-f", "sync-main-to-v0.1", "sync-main-to-v0.1~1")
        self.run_sync("--list")
        self.assertEqual(self.git("rev-parse", "sync-main-to-v0.1"), tip)
        self.assertEqual(self.git("branch", "--show-current"), "main")

    def test_resume_keeps_local_commits_ahead_of_fork(self):
        self.git("push", "origin", "sync-main-to-v0.1~1:refs/heads/sync-main-to-v0.1")
        tip = self.git("rev-parse", "sync-main-to-v0.1")
        self.run_sync("--list")
        self.assertEqual(self.git("rev-parse", "sync-main-to-v0.1"), tip)

    def test_resume_restores_deleted_local_branch(self):
        tip = self.git("rev-parse", "sync-main-to-v0.1")
        self.git("push", "origin", "sync-main-to-v0.1")
        self.git("branch", "-D", "sync-main-to-v0.1")
        self.run_sync("--list")
        self.assertEqual(self.git("rev-parse", "sync-main-to-v0.1"), tip)

    def test_resume_rejects_divergence(self):
        self.git("push", "origin", "sync-main-to-v0.1")
        self.git("branch", "-f", "sync-main-to-v0.1", "sync-main-to-v0.1~1")
        self.git("checkout", "sync-main-to-v0.1")
        self.git("commit", "--allow-empty", "-m", "local divergence")
        self.git("checkout", "main")
        tip = self.git("rev-parse", "sync-main-to-v0.1")
        result = self.run_sync_raw("--list")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("diverged", result.stderr)
        self.assertEqual(self.git("rev-parse", "sync-main-to-v0.1"), tip)

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
        (self.repo / "untracked").write_text("dirty checkouts must not block\n")
        self.assertIn("PR already open", self.run_sync("--yes"))
        remote = self.git("ls-remote", "origin", "refs/heads/sync-main-to-v0.1")
        self.assertEqual(remote.split()[0], self.git("rev-parse", "sync-main-to-v0.1"))
        self.assertEqual(self.git("branch", "--show-current"), "v0.1")

    def test_empty_plan_resumes_and_finds_existing_pr(self):
        self.assertIn("PR already open", self.run_sync("--yes"))
        self.assertFalse(self.worktree().exists())

    def test_ignores_inherited_git_redirection(self):
        bogus = str(self.repo / "nonexistent")
        env = dict(self.env, GIT_DIR=bogus, GIT_WORK_TREE=bogus,
                   GIT_INDEX_FILE=bogus, GIT_COMMON_DIR=bogus)
        result = subprocess.run(["/bin/bash", str(SCRIPT), "v0.1", "--list"],
                                cwd=self.repo, env=env, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("cherry-pick", result.stdout)

    def assert_sync_rejected(self, message, *args):
        worktree = str(self.worktree())
        refs = self.git("for-each-ref", "--format=%(refname) %(objectname)", "refs/heads")
        head = self.git("-C", worktree, "rev-parse", "HEAD")
        status = self.git("-C", worktree, "status", "--porcelain")
        remote = self.git("ls-remote", "origin", "refs/heads/sync-main-to-v0.1")
        result = self.run_sync_raw(*args)
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn(message, result.stderr)
        self.assertEqual(self.git("for-each-ref", "--format=%(refname) %(objectname)", "refs/heads"), refs)
        self.assertEqual(self.git("-C", worktree, "rev-parse", "HEAD"), head)
        self.assertEqual(self.git("-C", worktree, "status", "--porcelain"), status)
        self.assertEqual(self.git("ls-remote", "origin", "refs/heads/sync-main-to-v0.1"), remote)

    def test_rejects_wrong_branch_or_detached_worktree_before_picks_or_fast_forward(self):
        tip = self.git("rev-parse", "sync-main-to-v0.1")
        self.git("branch", "-f", "sync-main-to-v0.1", "v0.1")
        worktree = str(self.worktree())
        self.git("worktree", "add", "--detach", worktree, "v0.1")
        for fork_exists in (False, True):
            if fork_exists:
                self.git("push", "origin", tip + ":refs/heads/sync-main-to-v0.1")
            for detached in (False, True):
                with self.subTest(fork_exists=fork_exists, detached=detached):
                    if detached:
                        self.git("-C", worktree, "checkout", "--detach", "v0.1")
                    else:
                        self.git("-C", worktree, "checkout", "-B", "unrelated", "v0.1")
                    self.assert_sync_rejected("must have sync-main-to-v0.1 checked out", "--yes")

    def test_empty_plan_rejects_dirty_worktree(self):
        self.git("worktree", "add", str(self.worktree()), "sync-main-to-v0.1")
        (self.worktree() / "base").write_text("uncommitted\n")
        for staged in (False, True):
            with self.subTest(staged=staged):
                if staged:
                    self.git("-C", str(self.worktree()), "add", "base")
                self.assert_sync_rejected("uncommitted changes", "--yes")
                self.assertEqual((self.worktree() / "base").read_text(), "uncommitted\n")

    def test_empty_plan_rejects_pending_cherry_pick(self):
        worktree = str(self.worktree())
        self.git("worktree", "add", worktree, "sync-main-to-v0.1")
        result = subprocess.run(["git", "-C", worktree, "cherry-pick", "-x", self.picks[0]],
                                cwd=self.repo, env=self.env, capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.git("-C", worktree, "rev-parse", "CHERRY_PICK_HEAD"), self.picks[0])
        for fork_exists in (False, True):
            if fork_exists:
                self.git("push", "origin", "sync-main-to-v0.1")
            for option in ("--yes", "--list"):
                with self.subTest(fork_exists=fork_exists, option=option):
                    self.assert_sync_rejected("still in progress", option)
                    self.assertEqual(self.git("-C", worktree, "rev-parse", "CHERRY_PICK_HEAD"), self.picks[0])

    def test_dirty_worktree_is_not_fast_forwarded(self):
        self.git("push", "origin", "sync-main-to-v0.1")
        worktree = str(self.worktree())
        self.git("worktree", "add", worktree, "sync-main-to-v0.1")
        self.git("-C", worktree, "reset", "--hard", "HEAD~1")
        (self.worktree() / "base").write_text("uncommitted\n")
        for option in ("--yes", "--list"):
            with self.subTest(option=option):
                self.assert_sync_rejected("uncommitted changes", option)
                self.assertEqual((self.worktree() / "base").read_text(), "uncommitted\n")

    def test_clean_worktree_can_fast_forward_and_publish(self):
        tip = self.git("rev-parse", "sync-main-to-v0.1")
        self.git("push", "origin", "sync-main-to-v0.1")
        worktree = str(self.worktree())
        self.git("worktree", "add", worktree, "sync-main-to-v0.1")
        self.git("-C", worktree, "reset", "--hard", "HEAD~1")
        self.assertIn("PR already open", self.run_sync("--yes"))
        self.assertEqual(self.git("-C", worktree, "rev-parse", "HEAD"), tip)
        self.assertEqual(self.git("-C", worktree, "status", "--porcelain"), "")

    def test_empty_plan_rejects_unregistered_worktree_directory(self):
        self.worktree().mkdir()
        result = self.run_sync_raw("--yes")
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("not a registered worktree", result.stderr)
        self.assertEqual(self.git("ls-remote", "origin", "refs/heads/sync-main-to-v0.1"), "")

    def test_conflict_resumes_in_a_worktree(self):
        self.git("branch", "-D", "sync-main-to-v0.1")
        self.git("checkout", "v0.1")
        (self.repo / "clash").write_text("release\n")
        self.git("add", ".")
        self.git("commit", "-m", "release clash")
        self.git("push", "upstream", "v0.1")
        self.git("checkout", "main")
        (self.repo / "clash").write_text("main\n")
        self.git("add", ".")
        self.git("commit", "-m", "main clash")
        (self.repo / "trailing").write_text("trailing\n")
        self.git("add", ".")
        self.git("commit", "-m", "trailing change")
        self.git("push", "upstream", "main")

        result = self.run_sync_raw("--yes")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(CONFLICT, result.stderr)
        empty_commit = (f'git -C "{self.worktree().resolve()}" -c core.commentChar=\'#\' '
                        'commit --allow-empty --no-edit')
        self.assertIn(empty_commit, result.stderr)
        self.assertIn("git add", result.stderr)
        # the picks ran in a worktree, the invoking checkout is untouched
        self.assertEqual(self.git("branch", "--show-current"), "main")
        worktree = self.worktree()
        self.assertTrue(worktree.is_dir(), "expected worktree at %s" % worktree)

        # rerunning before resolving reports the pending cherry-pick
        again = self.run_sync_raw("--yes")
        self.assertNotEqual(again.returncode, 0)
        self.assertIn("still in progress", again.stderr)
        self.assertIn(empty_commit, again.stderr)

        (worktree / "clash").write_text("resolved\n")
        self.git("-C", str(worktree), "add", "clash")
        self.git("-C", str(worktree), "-c", "core.editor=true", "cherry-pick", "--continue")
        self.assertIn("PR already open", self.run_sync("--yes"))
        self.assertIn("already applied", self.run_sync_raw("--list").stderr)
        self.assertEqual(self.git("log", "--format=%s", "v0.1..sync-main-to-v0.1").splitlines(),
                         ["trailing change", "main clash", "second", "first"])
        self.assertEqual(self.git("branch", "--show-current"), "main")


if __name__ == "__main__":
    unittest.main()
