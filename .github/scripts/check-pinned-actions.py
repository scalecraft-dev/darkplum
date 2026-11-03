#!/usr/bin/env python3
"""Fail when a workflow uses an action that is not pinned to a commit.

The release workflows sign what customers deploy, and CI is what gates a release:
a moved tag on a third-party action would run inside that trust boundary. Every
`uses:` in every workflow must be a local reusable workflow (./...) or
owner/repo[/path]@<40-hex sha>, with the version it was taken from in a comment.
Run: python3 -I .github/scripts/check-pinned-actions.py [workflow.yml ...]
"""
import re
import sys
from pathlib import Path

USES = re.compile(r"^\s*(?:-\s+)?uses:\s*([^\s#]+)")
PINNED = re.compile(r"^[A-Za-z0-9._-]+/[A-Za-z0-9._/-]+@[0-9a-f]{40}$")


def check(text, name):
    bad = []
    for n, line in enumerate(text.splitlines(), 1):
        m = USES.match(line)
        if not m:
            continue
        ref = m.group(1)
        if ref.startswith("./") or PINNED.match(ref):
            continue
        bad.append(f"{name}:{n}: {ref} is not pinned to a 40-hex commit sha")
    return bad


def main(argv):
    root = Path(__file__).resolve().parent.parent / "workflows"
    files = [Path(a) for a in argv] or sorted(root.glob("*.yml"))
    if not argv and len(files) < 6:
        print("fewer workflows than expected: wrong directory?", file=sys.stderr)
        return 1
    bad = []
    for f in files:
        if not f.exists():
            bad.append(f"{f}: workflow is missing")
            continue
        bad += check(f.read_text(encoding="utf-8"), f.name)
    for b in bad:
        print(b, file=sys.stderr)
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
