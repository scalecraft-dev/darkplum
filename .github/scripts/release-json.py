#!/usr/bin/env python3
"""Write the release manifest that is baked into the darkplum CLI.

Run as `python3 -I .github/scripts/release-json.py ...` by release.yml and
prerelease.yml. The CLI refuses to trust anything that is not in this manifest:
the image digests it deploys, the repository and the workflow identity it
verifies their signatures against. So this script validates its inputs hard and
fails closed; a channel/ref/workflow/prefix combination that does not belong
together never produces a manifest.

Output is canonical JSON (sorted keys, no whitespace). With --base64 it is the
single-line form passed to the linker:
  -X github.com/scalecraft-dev/darkplum/cli/internal/release.encoded=<base64>
"""
import argparse
import base64
import json
import re
import sys

SERVICES = ("gateway", "api", "web")
REGISTRY = "ghcr.io"

# channel -> (workflow file, image prefix, ref pattern, version pattern)
CHANNELS = {
    "release": (
        "release.yml",
        "darkplum",
        re.compile(r"^refs/tags/v([0-9]+\.[0-9]+\.[0-9]+)$"),
        re.compile(r"^[0-9]+\.[0-9]+\.[0-9]+$"),
    ),
    "rc": (
        "prerelease.yml",
        "darkplum-rc",
        re.compile(r"^refs/heads/prerelease/[A-Za-z0-9._/-]+$"),
        re.compile(r"^0\.0\.0-rc\.[0-9]+\+[0-9a-f]{7}$"),
    ),
}

DIGEST = re.compile(r"^sha256:[0-9a-f]{64}$")
COMMIT = re.compile(r"^[0-9a-f]{40}$")
REPO = re.compile(r"^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$")


class Invalid(Exception):
    pass


def build(channel, version, commit, run_id, repo, ref, images_in):
    """images_in: {service: {"digest": "sha256:...", "run_attempt": "N"}}. Each image
    leg of _build-sign.yml signs in its own job, and a re-run of a failed job gets a
    new attempt number while its siblings keep theirs, so the attempt is recorded
    per image and the CLI matches each image's certificate against its own."""
    if channel not in CHANNELS:
        raise Invalid(f"channel {channel!r} is not one of {sorted(CHANNELS)}")
    workflow, prefix, ref_re, ver_re = CHANNELS[channel]
    if not ver_re.fullmatch(version):
        raise Invalid(f"version {version!r} is not valid for the {channel} channel")
    m = ref_re.fullmatch(ref)
    if not m:
        raise Invalid(f"ref {ref!r} is not valid for the {channel} channel")
    if channel == "release" and m.group(1) != version:
        raise Invalid(f"tag {ref!r} does not match version {version!r}")
    if not COMMIT.fullmatch(commit):
        raise Invalid(f"commit {commit!r} is not a 40-hex sha")
    if not re.fullmatch("[0-9]+", run_id):
        raise Invalid("run id must be numeric")
    if not REPO.fullmatch(repo):
        raise Invalid(f"repository {repo!r} is not owner/name")
    owner = repo.split("/")[0].lower()
    if not isinstance(images_in, dict) or sorted(images_in) != sorted(SERVICES):
        raise Invalid(f"images must be exactly {sorted(SERVICES)}, got {sorted(images_in) if isinstance(images_in, dict) else images_in!r}")
    images = {}
    for svc in SERVICES:
        img = images_in[svc]
        if not isinstance(img, dict) or sorted(img) != ["digest", "run_attempt"]:
            raise Invalid(f"{svc} must have exactly digest and run_attempt")
        if not isinstance(img["digest"], str) or not DIGEST.fullmatch(img["digest"]):
            raise Invalid(f"digest for {svc} is not sha256:<64 hex>: {img['digest']!r}")
        if not isinstance(img["run_attempt"], str) or not re.fullmatch("[1-9][0-9]*", img["run_attempt"]):
            raise Invalid(f"run attempt for {svc} is not a positive number: {img['run_attempt']!r}")
        images[svc] = {"repository": f"{REGISTRY}/{owner}/{prefix}-{svc}", "digest": img["digest"],
                       "run_attempt": img["run_attempt"]}
    return {
        "schema": 1,
        "channel": channel,
        "version": version,
        "commit": commit,
        "repository": repo,
        "ref": ref,
        "workflow": f".github/workflows/{workflow}",
        "run_id": run_id,
        "images": images,
    }


def canonical(doc):
    return json.dumps(doc, sort_keys=True, separators=(",", ":"))


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--channel", required=True)
    p.add_argument("--version", required=True)
    p.add_argument("--commit", required=True)
    p.add_argument("--run-id", required=True)
    p.add_argument("--repository", required=True)
    p.add_argument("--ref", required=True)
    p.add_argument("--digests", required=True, help='path to {"gateway":{"digest":"sha256:...","run_attempt":"1"},"api":...,"web":...}')
    p.add_argument("--base64", action="store_true", help="emit the single-line base64 form for the linker")
    a = p.parse_args(argv)
    try:
        with open(a.digests, encoding="utf-8") as f:
            digests = json.load(f)
        doc = build(a.channel, a.version, a.commit, a.run_id, a.repository, a.ref, digests)
    except (Invalid, OSError, ValueError) as e:
        print(f"release-json: {e}", file=sys.stderr)
        return 1
    out = canonical(doc)
    print(base64.b64encode(out.encode()).decode() if a.base64 else out)
    return 0


if __name__ == "__main__":
    sys.exit(main())
