#!/usr/bin/env python3
"""Fail when documentation tells a user to copy or hand-edit terraform.tfvars.

Configuration comes from `darkplum init`. Any README, docs/*.md or Makefile text
that instructs copying or editing terraform.tfvars (cp, copy, edit, vi, vim,
nano, "open ... terraform.tfvars", ...) is rejected, including when the sentence
wraps across lines. The advanced/GitOps path stays documented, but only inside an
explicit block:

    <!-- tfvars-manual:begin -->   (any comment syntax; Makefile: # tfvars-manual:begin)
    ...
    <!-- tfvars-manual:end -->

Usage: check-tfvars-docs.py [files...]   (default: every tracked *.md and Makefile)
"""
import re
import subprocess
import sys

VERBS = r"(?:cp|copy|edit|hand-edit|modify|vi|vim|nano|emacs|open)"
FORWARD = re.compile(r"\b" + VERBS + r"\b[^.;]{0,80}?terraform\.tfvars", re.I)
REVERSE = re.compile(r"terraform\.tfvars\b[^.;]{0,60}?\b(?:edit|vi|vim|nano|emacs)\b", re.I)
BEGIN, END = "tfvars-manual:begin", "tfvars-manual:end"


def strip_allowed(lines):
    out, inside = [], False
    for line in lines:
        if BEGIN in line:
            inside = True
            continue
        if END in line:
            inside = False
            continue
        if not inside:
            out.append(line)
    if inside:
        raise SystemExit("unterminated tfvars-manual block")
    return out


def paragraphs(lines):
    block = []
    for line in lines + [""]:
        text = re.sub(r"^\s*(?:#|//|>|[-*])?\s*", "", line.strip("\t ")).strip()
        if text:
            block.append(text)
        elif block:
            yield " ".join(block)
            block = []


def check(path):
    with open(path, encoding="utf-8") as f:
        lines = f.read().splitlines()
    bad = []
    for para in paragraphs(strip_allowed(lines)):
        # Drop line-wrap artefacts and markdown code ticks before matching.
        flat = re.sub(r"\s+", " ", para.replace("`", ""))
        if FORWARD.search(flat) or REVERSE.search(flat):
            bad.append(flat[:160])
    return bad


def main():
    files = sys.argv[1:]
    if not files:
        root = subprocess.check_output(["git", "rev-parse", "--show-toplevel"], text=True).strip()
        out = subprocess.check_output(["git", "-C", root, "ls-files", "*.md", "*Makefile"], text=True)
        files = [f"{root}/{f}" for f in out.split() if "node_modules" not in f]
    failed = False
    for path in files:
        for text in check(path):
            failed = True
            print(f"{path}: tells the reader to copy/edit terraform.tfvars: {text!r}")
    if failed:
        print("Use ./bin/darkplum init instead, or wrap a deliberate GitOps/advanced section in\n"
              "tfvars-manual:begin / tfvars-manual:end markers.")
        sys.exit(1)


if __name__ == "__main__":
    main()
