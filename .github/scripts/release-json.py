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
OWNER = "scalecraft-dev"

# channel -> (workflow file, image prefix, ref pattern, version pattern)
CHANNELS = {
    "release": (
        "release.yml",
        "darkplum",
        re.compile(r"^refs/tags/v(\d+\.\d+\.\d+)$"),
        re.compile(r"^\d+\.\d+\.\d+$"),
    ),
    "rc": (
        "prerelease.yml",
        "darkplum-rc",
        re.compile(r"^refs/heads/prerelease/[A-Za-z0-9._/-]+$"),
        re.compile(r"^0\.0\.0-rc\.\d+\+[0-9a-f]{7}$"),
    ),
}

DIGEST = re.compile(r"^sha256:[0-9a-f]{64}$")
COMMIT = re.compile(r"^[0-9a-f]{40}$")
REPO = re.compile(r"^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$")


class Invalid(Exception):
    pass


def build(channel, version, commit, run_id, run_attempt, repo, ref, digests):
    if channel not in CHANNELS:
        raise Invalid(f"channel {channel!r} is not one of {sorted(CHANNELS)}")
    workflow, prefix, ref_re, ver_re = CHANNELS[channel]
    if not ver_re.match(version):
        raise Invalid(f"version {version!r} is not valid for the {channel} channel")
    m = ref_re.match(ref)
    if not m:
        raise Invalid(f"ref {ref!r} is not valid for the {channel} channel")
    if channel == "release" and m.group(1) != version:
        raise Invalid(f"tag {ref!r} does not match version {version!r}")
    if not COMMIT.match(commit):
        raise Invalid(f"commit {commit!r} is not a 40-hex sha")
    if not (run_id.isdigit() and run_attempt.isdigit()):
        raise Invalid("run id and attempt must be numeric")
    if not REPO.match(repo) or repo.split("/")[0] != OWNER:
        raise Invalid(f"repository {repo!r} is not owned by {OWNER}")
    if sorted(digests) != sorted(SERVICES):
        raise Invalid(f"digests must be exactly {sorted(SERVICES)}, got {sorted(digests)}")
    images = {}
    for svc in SERVICES:
        if not DIGEST.match(digests[svc]):
            raise Invalid(f"digest for {svc} is not sha256:<64 hex>: {digests[svc]!r}")
        images[svc] = {"repository": f"{REGISTRY}/{OWNER}/{prefix}-{svc}", "digest": digests[svc]}
    return {
        "schema": 1,
        "channel": channel,
        "version": version,
        "commit": commit,
        "repository": repo,
        "ref": ref,
        "workflow": f".github/workflows/{workflow}",
        "run_id": run_id,
        "run_attempt": run_attempt,
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
    p.add_argument("--run-attempt", required=True)
    p.add_argument("--repository", required=True)
    p.add_argument("--ref", required=True)
    p.add_argument("--digests", required=True, help='path to {"gateway":"sha256:...","api":...,"web":...}')
    p.add_argument("--base64", action="store_true", help="emit the single-line base64 form for the linker")
    a = p.parse_args(argv)
    try:
        with open(a.digests, encoding="utf-8") as f:
            digests = json.load(f)
        doc = build(a.channel, a.version, a.commit, a.run_id, a.run_attempt, a.repository, a.ref, digests)
    except (Invalid, OSError, ValueError) as e:
        print(f"release-json: {e}", file=sys.stderr)
        return 1
    out = canonical(doc)
    print(base64.b64encode(out.encode()).decode() if a.base64 else out)
    return 0


if __name__ == "__main__":
    sys.exit(main())
