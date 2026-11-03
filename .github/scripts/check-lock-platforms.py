#!/usr/bin/env python3
"""Fail unless the Terraform lock file covers every platform the CLI ships for.

The darkplum CLI runs Terraform on the customer's machine with
-lockfile=readonly, so a provider whose lock entry has no checksum for that
machine's platform cannot be installed there. `terraform init` on one laptop
records only that laptop's platform; `terraform providers lock` is what adds the
rest. Each provider needs one h1: hash per platform: darwin and linux, arm64 and
amd64.

Run: python3 -I .github/scripts/check-lock-platforms.py [path/to/.terraform.lock.hcl]
"""
import re
import sys
from pathlib import Path

PLATFORMS = 4
DEFAULT = Path(__file__).resolve().parents[2] / "darkplum/terraform/aws/.terraform.lock.hcl"


def short_providers(text):
    """provider name -> number of h1: hashes."""
    counts = {}
    current = None
    for line in text.splitlines():
        m = re.match(r'^provider\s+"([^"]+)"', line)
        if m:
            current = m.group(1)
            counts[current] = 0
        elif current and re.match(r'^\s*"h1:', line):
            counts[current] += 1
    return counts


def check(text):
    counts = short_providers(text)
    if not counts:
        return ["no providers found in the lock file"]
    return [f"{p}: {n} h1 hashes, need {PLATFORMS} (run: terraform providers lock -platform=darwin_arm64 "
            f"-platform=darwin_amd64 -platform=linux_arm64 -platform=linux_amd64)"
            for p, n in sorted(counts.items()) if n < PLATFORMS]


def main(argv):
    path = Path(argv[0]) if argv else DEFAULT
    bad = check(path.read_text(encoding="utf-8"))
    for b in bad:
        print(b, file=sys.stderr)
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
