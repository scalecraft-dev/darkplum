#!/usr/bin/env python3
"""Tests for require-ci-green.sh with a fake `gh` on PATH (no network, no GitHub)."""
import json
import os
import stat
import subprocess
import tempfile
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parent / "require-ci-green.sh"
SHA = "a" * 40
JOBS = ["Go (gateway)", "Go (api)", "Go (cli)", "Web", "Bundle", "Terraform", "Kubernetes manifests and docs", "Release plumbing"]

# gh api <path>: serve the next canned response for the workflow-runs endpoint, and
# a fixed answer for the check-runs endpoint. FAKE_GH_DIR holds runs-N.json, checks.json, count.
FAKE_GH = """#!/bin/sh
d="$FAKE_GH_DIR"
case "$*" in
  *actions/workflows/ci.yml/runs*)
    echo "$*" >> "$d/runs-queries"
    n=$(cat "$d/count" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "$d/count"
    f="$d/runs-$n.json"; [ -f "$f" ] || f=$(ls "$d"/runs-*.json | sort -V | tail -n1)
    cat "$f" ;;
  *check-runs*) cat "$d/checks.json" ;;
  *) echo "unexpected gh $*" >&2; exit 2 ;;
esac
"""


def run_obj(status="completed", conclusion="success", created="2026-10-31T10:00:00Z", attempt=1, sha=SHA, url="https://x/run/1"):
    return {"status": status, "conclusion": conclusion, "created_at": created, "run_attempt": attempt, "head_sha": sha, "html_url": url}


def checks(names=JOBS, conclusion="success", override=None):
    cr = [{"name": n, "status": "completed", "conclusion": (override or {}).get(n, conclusion)} for n in names]
    return {"check_runs": cr}


class RequireCIGreen(unittest.TestCase):
    def go(self, responses, check_runs=None, wait="2", poll="0"):
        with tempfile.TemporaryDirectory() as d:
            d = Path(d)
            for i, r in enumerate(responses, 1):
                (d / f"runs-{i}.json").write_text(json.dumps({"workflow_runs": r}))
            (d / "checks.json").write_text(json.dumps(check_runs or checks()))
            gh = d / "gh"
            gh.write_text(FAKE_GH)
            gh.chmod(gh.stat().st_mode | stat.S_IEXEC)
            env = {**os.environ, "PATH": f"{d}:{os.environ['PATH']}", "FAKE_GH_DIR": str(d), "CI_WAIT_SECONDS": wait, "CI_POLL_SECONDS": poll}
            p = subprocess.run(["bash", str(SCRIPT), "scalecraft-dev/darkplum", SHA], env=env, capture_output=True, text=True, timeout=60)
            queries = (d / "runs-queries").read_text() if (d / "runs-queries").exists() else ""
            return p, queries

    def test_green_ci_passes_and_asks_for_this_commit_on_main_push(self):
        p, q = self.go([[run_obj()]])
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertIn("head_sha=" + SHA, q)
        self.assertIn("event=push", q)
        self.assertIn("branch=main", q)

    def test_waits_for_a_run_that_is_still_going(self):
        p, _ = self.go([[run_obj("in_progress", None)], [run_obj("queued", None)], [run_obj()]])
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertIn("waiting", p.stdout)

    def test_waits_for_a_run_that_has_not_started(self):
        p, _ = self.go([[], [], [run_obj()]])
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertIn("no CI run", p.stdout)

    def test_times_out_when_ci_never_finishes_and_says_how_to_retry(self):
        p, _ = self.go([[run_obj("in_progress", None)]], wait="1", poll="0")
        self.assertEqual(p.returncode, 1)
        self.assertIn("re-run this job", p.stderr)

    def test_times_out_when_ci_never_started(self):
        p, _ = self.go([[]], wait="1")
        self.assertEqual(p.returncode, 1)

    def test_red_ci_fails_at_once_without_waiting(self):
        for conclusion in ("failure", "cancelled", "timed_out", "skipped", "action_required"):
            with self.subTest(conclusion):
                p, _ = self.go([[run_obj(conclusion=conclusion)]], wait="600")
                self.assertEqual(p.returncode, 1)
                self.assertIn(conclusion, p.stderr)

    def test_the_latest_run_decides(self):
        # a failed first attempt, then a green re-run: green
        p, _ = self.go([[run_obj(conclusion="failure", created="2026-10-31T10:00:00Z"),
                         run_obj(created="2026-10-31T10:30:00Z", attempt=2)]])
        self.assertEqual(p.returncode, 0, p.stderr)
        # a green run, then a later red one: red
        p, _ = self.go([[run_obj(created="2026-10-31T10:00:00Z"), run_obj(conclusion="failure", created="2026-10-31T10:30:00Z", attempt=2)]])
        self.assertEqual(p.returncode, 1)

    def test_a_run_for_another_commit_is_not_this_commit(self):
        p, _ = self.go([[run_obj(sha="b" * 40)]], wait="1")
        self.assertEqual(p.returncode, 1)

    def test_every_required_job_must_exist_and_pass(self):
        p, _ = self.go([[run_obj()]], check_runs=checks(names=[j for j in JOBS if j != "Bundle"]))
        self.assertEqual(p.returncode, 1)
        self.assertIn("required job not found", p.stderr)
        self.assertIn("Bundle", p.stderr)
        p, _ = self.go([[run_obj()]], check_runs=checks(override={"Terraform": "failure"}))
        self.assertEqual(p.returncode, 1)
        self.assertIn("Terraform", p.stderr)
        p, _ = self.go([[run_obj()]], check_runs=checks(override={"Web": "skipped"}))
        self.assertEqual(p.returncode, 1)

    def test_required_jobs_match_ci_yml(self):
        ci = (SCRIPT.parent.parent / "workflows" / "ci.yml").read_text()
        for name in ("Web", "Bundle", "Terraform", "Kubernetes manifests and docs", "Release plumbing"):
            self.assertIn(f"name: {name}\n", ci, f"ci.yml has no job named {name!r}")
        self.assertIn("name: Go (${{ matrix.service }})", ci)
        text = SCRIPT.read_text()
        for name in JOBS:
            self.assertIn(f'"{name}"', text)


if __name__ == "__main__":
    unittest.main()
