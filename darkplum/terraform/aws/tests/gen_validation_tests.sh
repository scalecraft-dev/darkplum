#!/usr/bin/env bash
# Generates a .tftest.hcl from variable_cases.json: one run block per INVALID
# case, expecting the variable's validation to reject it. The same file drives
# the Go validators in services/cli (TestVariableCasesMatchGoValidators), so the
# CLI and Terraform cannot drift on what is acceptable.
# Usage: gen_validation_tests.sh > variables.tftest.hcl
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
# Mocked providers: a rejected variable must fail on its own validation, with
# no AWS credentials in reach (CI has none, and these tests must never call AWS).
# Policy documents and ARNs are given well-formed mock values, so the plan's own
# checks on them pass and only the variable under test can fail.
cat "$here/mock_providers.hcl"
echo
jq -r 'to_entries[] | select(.value.valid == false) | .key as $i | .value |
"run \"reject_\(.var)_\($i)\" {\n  command = plan\n  variables {\n\(if .region then "    aws_region = \(.region | tojson)\n" else "" end)\((.extra // {}) | to_entries | map("    \(.key) = \(.value | tojson)\n") | join(""))    \(.var) = \(.value | tojson)\n  }\n  expect_failures = [var.\(.var)]\n}\n"' \
  "$here/variable_cases.json"
