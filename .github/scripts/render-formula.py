#!/usr/bin/env python3
"""Render the Homebrew formula for a release from its checksums.txt.

Run by release.yml (job "tap") after the release is published:
  python3 -I .github/scripts/render-formula.py --version 1.2.3 \
    --repository scalecraft-dev/darkplum --checksums checksums.txt \
    --template .github/packaging/homebrew/darkplum.rb.tmpl > darkplum.rb

Fails closed: all four archives must be in checksums.txt with a 64-hex SHA-256,
nothing else may be in it, and no placeholder may survive the render. The
formula carries the sha256, which is what lets Homebrew check the download.
"""
import argparse
import re
import sys

PLATFORMS = ("darwin_arm64", "darwin_amd64", "linux_arm64", "linux_amd64")
VERSION = re.compile(r"^[0-9]+\.[0-9]+\.[0-9]+$")
REPO = re.compile(r"^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$")
LINE = re.compile(r"^([0-9a-f]{64})  (darkplum_([0-9.]+)_([a-z]+_[a-z0-9]+)\.tar\.gz)$")


class Invalid(Exception):
    pass


def parse_checksums(text, version):
    found = {}
    for n, line in enumerate(text.splitlines(), 1):
        m = LINE.match(line)
        if not m:
            raise Invalid(f"checksums.txt:{n}: not a '<sha256>  darkplum_<version>_<os>_<arch>.tar.gz' line")
        digest, _, v, plat = m.groups()
        if v != version:
            raise Invalid(f"checksums.txt:{n}: archive is for {v}, not {version}")
        if plat not in PLATFORMS:
            raise Invalid(f"checksums.txt:{n}: unexpected platform {plat}")
        if plat in found:
            raise Invalid(f"checksums.txt:{n}: {plat} appears twice")
        found[plat] = digest
    missing = [p for p in PLATFORMS if p not in found]
    if missing:
        raise Invalid(f"checksums.txt has no archive for {', '.join(missing)}")
    return found


def render(template, version, repo, checksums_text):
    if not VERSION.fullmatch(version):
        raise Invalid(f"version {version!r} is not X.Y.Z")
    if not REPO.fullmatch(repo):
        raise Invalid(f"repository {repo!r} is not owner/name")
    sums = parse_checksums(checksums_text, version)
    out = template.replace("@VERSION@", version).replace("@REPO@", repo)
    for plat, digest in sums.items():
        out = out.replace(f"@SHA256_{plat.upper()}@", digest)
    left = re.findall(r"@[A-Z0-9_]+@", out)
    if left:
        raise Invalid(f"unreplaced placeholders: {sorted(set(left))}")
    return out


def main(argv):
    ap = argparse.ArgumentParser()
    ap.add_argument("--version", required=True)
    ap.add_argument("--repository", required=True)
    ap.add_argument("--checksums", required=True)
    ap.add_argument("--template", required=True)
    a = ap.parse_args(argv)
    try:
        with open(a.template, encoding="utf-8") as f:
            tmpl = f.read()
        with open(a.checksums, encoding="utf-8") as f:
            sums = f.read()
        sys.stdout.write(render(tmpl, a.version, a.repository, sums))
    except (Invalid, OSError) as e:
        print(f"render-formula: {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
