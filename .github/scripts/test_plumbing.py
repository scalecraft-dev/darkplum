#!/usr/bin/env python3
"""Tests for the release plumbing scripts. Run: python3 -I -m unittest discover -s .github/scripts -p 'test_*.py'"""
import base64
import importlib.util
import json
import re
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent


def load(name, file):
    spec = importlib.util.spec_from_file_location(name, HERE / file)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


rj = load("release_json", "release-json.py")
pin = load("check_pinned", "check-pinned-actions.py")

SHA = "a" * 40
D = {s: "sha256:" + c * 64 for s, c in zip(("gateway", "api", "web"), "123")}


def good_release(**over):
    kw = dict(channel="release", version="1.2.3", commit=SHA, run_id="42", run_attempt="1",
              repo="scalecraft-dev/darkplum", ref="refs/tags/v1.2.3", digests=dict(D))
    kw.update(over)
    return kw


class ReleaseJSON(unittest.TestCase):
    def test_release_manifest_shape(self):
        doc = rj.build(**good_release())
        self.assertEqual(doc["workflow"], ".github/workflows/release.yml")
        self.assertEqual(doc["images"]["gateway"]["repository"], "ghcr.io/scalecraft-dev/darkplum-gateway")
        self.assertEqual(doc["images"]["web"]["digest"], D["web"])

    def test_rc_manifest_uses_rc_repositories_and_prerelease_workflow(self):
        doc = rj.build(**good_release(channel="rc", version="0.0.0-rc.7+abcdef0", ref="refs/heads/prerelease/makefile-removal"))
        self.assertEqual(doc["workflow"], ".github/workflows/prerelease.yml")
        self.assertEqual(doc["images"]["api"]["repository"], "ghcr.io/scalecraft-dev/darkplum-rc-api")

    def test_refuses_mismatched_inputs(self):
        cases = {
            "unknown channel": dict(channel="beta"),
            "tag does not match version": dict(ref="refs/tags/v1.2.4"),
            "release from a branch": dict(ref="refs/heads/main"),
            "rc from a tag": dict(channel="rc", version="0.0.0-rc.1+abcdef0", ref="refs/tags/v1.2.3"),
            "rc from main": dict(channel="rc", version="0.0.0-rc.1+abcdef0", ref="refs/heads/main"),
            "rc version on release channel": dict(version="0.0.0-rc.1+abcdef0"),
            "short commit": dict(commit="abc123"),
            "foreign owner": dict(repo="evil/darkplum"),
            "non-numeric run id": dict(run_id="4x"),
            "tag digest": dict(digests={**D, "api": "latest"}),
            "short digest": dict(digests={**D, "api": "sha256:abc"}),
            "missing service": dict(digests={k: v for k, v in D.items() if k != "web"}),
            "extra service": dict(digests={**D, "extra": D["web"]}),
        }
        for label, over in cases.items():
            with self.subTest(label):
                with self.assertRaises(rj.Invalid):
                    rj.build(**good_release(**over))

    def test_cli_base64_round_trips_and_is_canonical(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d, "digests.json")
            p.write_text(json.dumps(D))
            argv = ["--channel", "release", "--version", "1.2.3", "--commit", SHA, "--run-id", "42", "--run-attempt", "1",
                    "--repository", "scalecraft-dev/darkplum", "--ref", "refs/tags/v1.2.3", "--digests", str(p)]
            import contextlib
            import io
            buf = io.StringIO()
            with contextlib.redirect_stdout(buf):
                self.assertEqual(rj.main(argv + ["--base64"]), 0)
            raw = base64.b64decode(buf.getvalue().strip()).decode()
            self.assertEqual(raw, rj.canonical(rj.build(**good_release())))
            self.assertNotIn(" ", raw)

    def test_missing_digest_file_fails_closed(self):
        self.assertEqual(rj.main(["--channel", "release", "--version", "1.2.3", "--commit", SHA, "--run-id", "1",
                                  "--run-attempt", "1", "--repository", "scalecraft-dev/darkplum",
                                  "--ref", "refs/tags/v1.2.3", "--digests", "/nonexistent"]), 1)


class PinnedActions(unittest.TestCase):
    def test_flags_tag_and_branch_refs(self):
        text = "steps:\n  - uses: actions/checkout@v4\n  - uses: foo/bar@main\n  - uses: ok/ok@" + SHA + "\n  - uses: ./.github/workflows/x.yml\n"
        bad = pin.check(text, "w.yml")
        self.assertEqual(len(bad), 2)
        self.assertIn("actions/checkout@v4", bad[0])

    def test_shipped_workflows_are_pinned(self):
        self.assertEqual(pin.main([]), 0)

    def test_every_pinned_action_carries_a_version_comment(self):
        for f in pin.PLUMBING:
            for n, line in enumerate((HERE.parent / "workflows" / f).read_text().splitlines(), 1):
                if re.search(r"uses:\s*\S+@[0-9a-f]{40}", line):
                    self.assertRegex(line, r"#\s*v\d", f"{f}:{n} pinned action needs a '# vX.Y.Z' comment")


if __name__ == "__main__":
    unittest.main()
