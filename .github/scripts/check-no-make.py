#!/usr/bin/env python3
"""Fail when the repository tells anyone to run make, or a Makefile comes back.

Darkplum is operated with the `darkplum` CLI and developed with scripts/dev.sh.
Two things are rejected in tracked files:

  1. a file named Makefile, makefile, GNUmakefile or *.mk;
  2. text that instructs running make: `make <target>`, `make -X`, `make VAR=...`,
     `$(MAKE)` or a bare `make` in code ticks.

Prose such as "make sure" or "make it" is not matched: only the shapes above are.
History is allowlisted explicitly (ALLOW): database migrations, the semantic
router values files kept as snapshots of what earlier releases shipped, and this
script with its test. Do not widen the list to silence a real instruction.

Usage: check-no-make.py [files...]   (default: every tracked file)
"""
import fnmatch
import os
import re
import subprocess
import sys

# Words that follow `make` when it is an instruction. A closed list on purpose:
# an open one would flag English.
TARGETS = (
    r"deploy|destroy|status|test|cli|help|init|configure|expose|unexpose|verify|build|push|check|semantic|dev|lint"
)
INSTRUCTION = re.compile(
    r"(?<![A-Za-z0-9_])make (?:-[A-Za-z]|(?:" + TARGETS + r")(?![a-z])|[a-z]+-[a-z][a-z-]*|[A-Z][A-Z_]*=)"
    r"|\$\(MAKE\)|`make`"
)
FILENAME = re.compile(r"(?:^|/)(?:Makefile|makefile|GNUmakefile|[^/]+\.mk)$")

ALLOW = [
    "*.sql",
    "darkplum/services/cli/internal/semrouter/testdata/*",
    ".github/scripts/check-no-make.py",
    ".github/scripts/test_check_no_make.py",
]


def allowed(path):
    return any(fnmatch.fnmatch(path, pat) for pat in ALLOW)


def scan_text(path, text):
    hits = []
    for n, line in enumerate(text.splitlines(), 1):
        if INSTRUCTION.search(line):
            hits.append((path, n, line.strip()[:160]))
    return hits


def tracked():
    root = subprocess.check_output(["git", "rev-parse", "--show-toplevel"], text=True).strip()
    out = subprocess.check_output(["git", "-C", root, "ls-files", "-z"], text=True)
    return root, [f for f in out.split("\0") if f]


def main():
    root, files = tracked()
    if len(sys.argv) > 1:
        files = sys.argv[1:]
    failures = []
    for rel in files:
        if allowed(rel):
            continue
        if FILENAME.search(rel):
            failures.append(f"{rel}: a Makefile must not come back (use the darkplum CLI or scripts/dev.sh)")
            continue
        full = rel if os.path.isabs(rel) else os.path.join(root, rel)
        try:
            with open(full, "rb") as f:
                data = f.read()
        except OSError:
            continue
        if b"\0" in data[:4096]:
            continue
        for path, n, line in scan_text(rel, data.decode("utf-8", "replace")):
            failures.append(f"{path}:{n}: tells the reader to run make: {line!r}")
    for f in failures:
        print(f)
    if failures:
        print("\nOperators use `darkplum <command>`; contributors use `scripts/dev.sh`. "
              "If a hit is history that must stay verbatim, add it to ALLOW in "
              ".github/scripts/check-no-make.py with the reason.")
        sys.exit(1)


if __name__ == "__main__":
    main()
