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
lock = load("check_lock", "check-lock-platforms.py")

SHA = "a" * 40
# one image per service, each with the run attempt of the job that signed it
D = {s: {"digest": "sha256:" + c * 64, "run_attempt": a} for s, c, a in zip(("gateway", "api", "web"), "123", ("1", "2", "1"))}


def good_release(**over):
    kw = dict(channel="release", version="1.2.3", commit=SHA, run_id="42",
              repo="scalecraft-dev/darkplum", ref="refs/tags/v1.2.3", images_in=dict(D))
    kw.update(over)
    return kw


class ReleaseJSON(unittest.TestCase):
    def test_release_manifest_shape(self):
        doc = rj.build(**good_release())
        self.assertEqual(doc["workflow"], ".github/workflows/release.yml")
        self.assertEqual(doc["images"]["gateway"]["repository"], "ghcr.io/scalecraft-dev/darkplum-gateway")
        self.assertEqual(doc["images"]["web"]["digest"], D["web"]["digest"])
        # the attempt is per image: a re-run of one failed job does not change the others
        self.assertEqual({k: v["run_attempt"] for k, v in doc["images"].items()}, {"gateway": "1", "api": "2", "web": "1"})
        self.assertNotIn("run_attempt", doc)

    def test_rc_manifest_uses_rc_repositories_and_prerelease_workflow(self):
        doc = rj.build(**good_release(channel="rc", version="0.0.0-rc.7+abcdef0", ref="refs/heads/prerelease/makefile-removal"))
        self.assertEqual(doc["workflow"], ".github/workflows/prerelease.yml")
        self.assertEqual(doc["images"]["api"]["repository"], "ghcr.io/scalecraft-dev/darkplum-rc-api")

    def test_images_live_under_the_building_repositorys_owner(self):
        # A release cut from a scratch organisation names that organisation's images;
        # whether the CLI TRUSTS that repository is decided in the CLI (its baked
        # trusted repository), never here.
        doc = rj.build(**good_release(repo="Scratch-Org/darkplum-release"))
        self.assertEqual(doc["repository"], "Scratch-Org/darkplum-release")
        self.assertEqual(doc["images"]["api"]["repository"], "ghcr.io/scratch-org/darkplum-api")

    def test_refuses_mismatched_inputs(self):
        cases = {
            "unknown channel": dict(channel="beta"),
            "tag does not match version": dict(ref="refs/tags/v1.2.4"),
            "release from a branch": dict(ref="refs/heads/main"),
            "rc from a tag": dict(channel="rc", version="0.0.0-rc.1+abcdef0", ref="refs/tags/v1.2.3"),
            "rc from main": dict(channel="rc", version="0.0.0-rc.1+abcdef0", ref="refs/heads/main"),
            "rc version on release channel": dict(version="0.0.0-rc.1+abcdef0"),
            "short commit": dict(commit="abc123"),
            "repository that is not owner/name": dict(repo="darkplum"),
            "non-numeric run id": dict(run_id="4x"),
            "tag digest": dict(images_in={**D, "api": {**D["api"], "digest": "latest"}}),
            "short digest": dict(images_in={**D, "api": {**D["api"], "digest": "sha256:abc"}}),
            "missing service": dict(images_in={k: v for k, v in D.items() if k != "web"}),
            "extra service": dict(images_in={**D, "extra": D["web"]}),
            "missing attempt": dict(images_in={**D, "api": {"digest": D["api"]["digest"]}}),
            "zero attempt": dict(images_in={**D, "api": {**D["api"], "run_attempt": "0"}}),
            "non-numeric attempt": dict(images_in={**D, "api": {**D["api"], "run_attempt": "1x"}}),
            "extra image field": dict(images_in={**D, "api": {**D["api"], "extra": "x"}}),
            "string instead of image": dict(images_in={**D, "api": "sha256:" + "2" * 64}),
        }
        for label, over in cases.items():
            with self.subTest(label):
                with self.assertRaises(rj.Invalid):
                    rj.build(**good_release(**over))

    def test_cli_base64_round_trips_and_is_canonical(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d, "digests.json")
            p.write_text(json.dumps(D))
            argv = ["--channel", "release", "--version", "1.2.3", "--commit", SHA, "--run-id", "42",
                    "--repository", "scalecraft-dev/darkplum", "--ref", "refs/tags/v1.2.3", "--digests", str(p)]
            import contextlib
            import io
            buf = io.StringIO()
            with contextlib.redirect_stdout(buf):
                self.assertEqual(rj.main(argv + ["--base64"]), 0)
            raw = base64.b64decode(buf.getvalue().strip()).decode()
            self.assertEqual(raw, rj.canonical(rj.build(**good_release())))
            self.assertNotIn(" ", raw)

    def test_matches_the_golden_manifests_the_go_cli_parses(self):
        # services/cli/internal/release parses these same files; if either side's
        # rules or field names drift, one of the two tests breaks.
        golden = HERE / "testdata"
        rel = rj.build("release", "1.2.3", "a" * 40, "42", "scalecraft-dev/darkplum", "refs/tags/v1.2.3", D)
        self.assertEqual((golden / "release.golden.json").read_text().strip(), rj.canonical(rel))
        rc = rj.build("rc", "0.0.0-rc.7+abcdef0", "b" * 40, "43", "scalecraft-dev/darkplum",
                      "refs/heads/prerelease/makefile-removal", D)
        self.assertEqual((golden / "rc.golden.json").read_text().strip(), rj.canonical(rc))

    def test_trailing_newline_and_unicode_digits_are_not_accepted(self):
        for over in (dict(version="1.2.3\n"), dict(commit=SHA + "\n"), dict(run_id="42\n"), dict(run_id="٤٢"),
                     dict(ref="refs/tags/v1.2.3\n")):
            with self.subTest(over):
                with self.assertRaises(rj.Invalid):
                    rj.build(**good_release(**over))

    def test_missing_digest_file_fails_closed(self):
        self.assertEqual(rj.main(["--channel", "release", "--version", "1.2.3", "--commit", SHA, "--run-id", "1",
                                  "--repository", "scalecraft-dev/darkplum",
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
        for path in sorted((HERE.parent / "workflows").glob("*.yml")):
            for n, line in enumerate(path.read_text().splitlines(), 1):
                if re.search(r"uses:\s*\S+@[0-9a-f]{40}", line):
                    self.assertRegex(line, r"#\s*v\d", f"{path.name}:{n} pinned action needs a '# vX.Y.Z' comment")


class LockPlatforms(unittest.TestCase):
    def test_shipped_lock_covers_four_platforms(self):
        self.assertEqual(lock.main([]), 0)

    def test_single_platform_lock_is_rejected(self):
        text = 'provider "registry.terraform.io/hashicorp/aws" {\n  hashes = [\n    "h1:abc=",\n    "zh:def",\n  ]\n}\n'
        bad = lock.check(text)
        self.assertEqual(len(bad), 1)
        self.assertIn("1 h1 hashes", bad[0])

    def test_empty_lock_is_rejected(self):
        self.assertTrue(lock.check("# nothing"))


if __name__ == "__main__":
    unittest.main()
