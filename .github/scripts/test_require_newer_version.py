#!/usr/bin/env python3
"""Tests for require-newer-version.sh against throwaway git repositories."""
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parent / "require-newer-version.sh"
REPOS = []


def repo_with_tags(tags):
    """A throwaway repository with one commit and the given tags; removed with the test case."""
    d = tempfile.mkdtemp()
    REPOS.append(d)
    env = {**os.environ, "GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@x", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@x"}
    subprocess.run(["git", "init", "-q", d], check=True, env=env)
    subprocess.run(["git", "-C", d, "commit", "-q", "--allow-empty", "-m", "x"], check=True, env=env)
    for t in tags:
        subprocess.run(["git", "-C", d, "tag", t], check=True, env=env)
    return d


def run(tags, version):
    d = repo_with_tags(tags)
    return subprocess.run(["bash", str(SCRIPT), version], cwd=d, capture_output=True, text=True, timeout=30)


class RequireNewerVersion(unittest.TestCase):
    def tearDown(self):
        while REPOS:
            shutil.rmtree(REPOS.pop(), ignore_errors=True)

    def test_the_first_tag_in_a_repository_passes(self):
        p = run(["v0.0.1"], "0.0.1")
        self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
        self.assertIn("first version tag", p.stdout)

    def test_a_newer_version_passes(self):
        p = run(["v0.1.0", "v0.2.0", "v0.3.0"], "0.3.0")
        self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
        self.assertIn("newer than v0.2.0", p.stdout)

    def test_an_older_version_is_refused_and_names_the_highest(self):
        p = run(["v0.1.0", "v0.2.0", "v0.1.5"], "0.1.5")
        self.assertEqual(p.returncode, 1)
        self.assertIn("v0.1.5 is not newer than v0.2.0", p.stdout)

    def test_versions_sort_numerically_not_textually(self):
        self.assertEqual(run(["v1.9.0", "v1.10.0"], "1.10.0").returncode, 0)
        self.assertEqual(run(["v1.10.0", "v1.9.0"], "1.9.0").returncode, 1)

    def test_tags_that_are_not_exact_versions_are_ignored(self):
        p = run(["v0.0.1", "v0.3.0-rc1", "v0.9.9x", "rc-1", "v0.0.1-junk"], "0.0.1")
        self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
        self.assertIn("first version tag", p.stdout)

    def test_a_malformed_version_is_refused(self):
        p = run(["v0.1.0"], "0.2")
        self.assertEqual(p.returncode, 1)
        self.assertIn("not an X.Y.Z version", p.stdout)


if __name__ == "__main__":
    unittest.main()
