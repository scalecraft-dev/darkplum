# End-to-end check of a release candidate

The deploy, destroy and release path has unit tests with fakes. It has not, until
this list is run, executed against a real cloud and a real signed release. This is
that list. Run it on an rc build (`prerelease.yml` on a `prerelease/**` branch,
channel `rc`, images `ghcr.io/scalecraft-dev/darkplum-rc-*`) against a **throwaway
AWS account** with a clean `HOME`, before the change that touches the install
path is merged, and again on the real release's draft (see
[RELEASING.md](RELEASING.md)).

A row is done when its evidence is attached to the pull request. "It worked" is
not evidence: attach the log files from `~/.darkplum/installs/<name>/logs/`,
timestamps, the script output, and hashes where the row says so.

## Setup

- A fresh AWS account (no prior ECR repositories, KMS keys or Terraform state), an
  SSO profile with a session longer than an hour, and `aws`, `terraform`,
  `kubectl`, `jq` and `gh` on `PATH`.
- The rc `darkplum` binary from the workflow artifacts, a clean `HOME` (no
  `~/.darkplum`, `~/.terraform.d`, `~/.kube`), and `darkplum version` showing
  channel `rc` and three digests.
- For the destroy rows, a **decoy cluster**: a second kind cluster (or a scratch
  EKS cluster) with a few workloads in it. Never a cluster that matters.
- `darkplum deploy` and `darkplum configure` print the elapsed time of every step
  and the total, and `configure` prints the time from the start of the deploy to
  the first chat. Row 10 collects those numbers; nobody needs a stopwatch.

The scripts live in `darkplum/scripts/e2e/`. Each exits non-zero on the first
failed assertion, is read-only except for the one `darkplum` action its row
requires, takes the cluster only from `darkplum kubeconfig` (never from
`~/.kube/config` or the current context), and refuses destructive work unless you
restate the cluster name in `E2E_CONFIRM_DESTROY`. They are linted with shellcheck
and exercised against stand-ins in CI (`scripts/e2e/selftest.sh`), which proves
that they fail when they should; it does not prove Darkplum.

## The list

| # | Row | How | Evidence |
|---|---|---|---|
| 0 | The rc builds, signs and verifies | Push `prerelease/<name>`; the `prerelease.yml` run's verify job passes. First, because every other row depends on it | the run URL |
| 1 | Fresh path, Bedrock-only | `darkplum init` (Bedrock only) -> `deploy` -> `configure` on the empty account. Signature verified, images mirrored, apply, api migrated, gateway `/health` shows routing, web up, `darkplum status` green, test chat answered | logs of all three; the printed total times |
| 1b | Fresh path, GPU | The same with a self-hosted model: deploy returns before the model is ready, `configure --wait` waits, `darkplum models test` returns 200. The one long run | logs; model load time |
| 2 | No outputs read before apply | On the fresh install of row 1, the deploy log shows no `terraform output` before the apply | the log |
| 3 | Re-run is a no-op | `noop-rerun.sh --install NAME` (add `--expect-vllm` after `darkplum models add`, `--expect-oidc` after `darkplum sso configure`). Also cycle the semantic router off and on and re-run | script output: secret hashes, pod uids, restart counts, image digests, `VLLM_UPSTREAMS`, OIDC all identical |
| 4a | Ctrl-C during the apply | Interrupt `darkplum deploy` during the Terraform apply; once more (second Ctrl-C); check the lock; re-run to converge | both logs; the re-run's "infrastructure" row |
| 4b | Session expires mid-apply | Use a short session or `aws sso logout` while applying. The next run must stop at preflight naming the session, resume and converge. Capture the `ExpiredToken` text | logs |
| 4c | The api fails | Break the database security group; the failure message must carry the pod events or logs. Fix, re-run | the failure output |
| 4d | A bad image stops early | Deploy an rc image re-signed from a non-prerelease ref, and an unsigned one: deploy must stop at step 2 before Terraform, and nothing is written | the output; `terraform state list` unchanged |
| 5 | A newer release rolls | Deploy rc N, then rc N+1: pods roll to the new digests and nothing references the old digest | `darkplum version`, pod `imageID`s |
| 6 | The earlier survivors, on the live cluster | Headless vLLM Services (`clusterIP: None`); NVIDIA device plugin running on the GPU node; gateway egress policies carry real service and VPC CIDRs (no `PLACEHOLDER`); EKS log group imported on reinstall (row 8); `api-ingress` policy; `Recreate` on the vLLM Deployment; `imagePullPolicy` as intended with digest refs | `kubectl get` output, pinned with `darkplum kubeconfig` |
| 7a | Destroy ignores the ambient context | `E2E_CONFIRM_DESTROY=<cluster> destroy-decoy-context.sh --install NAME --decoy-context CTX` | script output: the decoy's objects are identical before and after, and the destroy log never names it |
| 7b | Destroy with an expired session | `E2E_CONFIRM_DESTROY=<cluster> E2E_CONFIRM_LOGOUT=1 destroy-expired-session.sh --install NAME` | script output: refused, nothing started, RDS deletion protection and workloads identical after login |
| 7c | Destroy when the cluster is already gone | Delete the cluster in the console, then `E2E_CONFIRM_DESTROY=<cluster> destroy-cluster-gone.sh --install NAME` | script output: loud skip of the Kubernetes phase, finishes |
| 7d | Destroy with the wrong account | `E2E_CONFIRM_DESTROY=<cluster> destroy-account-mismatch.sh --install NAME` (it works on a copy of the record) | script output: refused before Terraform, cluster unchanged |
| 7e | Full destroy | `darkplum destroy`: finishes; the leftover report lists the snapshot, the audit log group, the state bucket and KMS key; the orphan query (load balancers, ENIs, EBS, security groups tagged for the cluster) returns none; `terraform state list` is empty | the output, the empty state list |
| 8 | Reinstall after destroy | `init` + `deploy` on the same account: log group adopted, the final snapshot untouched, install succeeds. This is the real test that leftovers are harmless | logs |
| 9 | Teardown with the private load balancer | Destroy an install with a web address (`modules/web-access`): the NLB does not block VPC deletion | the destroy log |
| 10 | Timings | Collect each step's time and the total from `deploy` and `configure`, the `ExpiredToken` behaviour from 4b, and the time to first chat from `configure` | the numbers, in the PR; use them to set ETAs and timeouts |

### Further rows (never run before)

| # | Row | How | Evidence |
|---|---|---|---|
| 11 | `init --adopt` from an install the Makefile created | One install created from the last Makefile-era commit (S3 backend), then `init --adopt` and `deploy` as an upgrade: `:latest` becomes digests, the plan gate stops on any replacement. A local-backend install is refused with instructions | logs; the plan |
| 12 | The rc refuses release identities and the reverse | The rc binary against a release manifest, the release binary against rc images | both refusals |
| 13 | A real ECR copy | Repeat PUT into an immutable repository; the attestation children are present in the mirrored index; note the time on a laptop | `darkplum deploy` log, `aws ecr describe-images` |
| 14 | vLLM downloads as non-root | The init container runs with the real `snapshot_download` | pod logs |
| 15 | Connectivity right after the first apply and across a roll | api -> RDS and gateway -> api and Bedrock work immediately after the first apply (policies are applied before workloads), and no connections drop on a re-deploy roll | a chat loop during the roll |
| 16 | Exit 3 (platform healthy, no GPU) | Deploy with a GPU request the account cannot satisfy: exit status 3, `configure` still works with Bedrock | exit status, output |
| 17 | `inference_region` is always set | The fresh apply of row 1 prints the output non-empty, with and without a Bedrock model allowed | `terraform output inference_region` |
| 18 | Providers on a customer machine | `init -lockfile=readonly` works on darwin/arm64, darwin/amd64 and linux/amd64 with a clean plugin cache | init logs |
| 19 | The README path from nothing | Follow the README from a clean machine, once with Homebrew and once with the manual download and its attestation check | the transcript |
| 20 | Production identity | A scratch-organisation `v0.0.1` release, including the negatives and the Homebrew round trip ([RELEASING.md](RELEASING.md), "Before the first release is merged") | logs, digests |
| 21 | Draft smoke before anyone is told | [RELEASING.md](RELEASING.md), "After merge", then approve the `tap` job and `brew install` + `brew test` | the checklist, ticked |

Rows 0 to 10 need only the rc. Rows 20 and 21 are the only ones that need a real
release; until they have run, the install instructions in the READMEs say the
release does not exist yet.

## Pull request checklist

Copy into a pull request that changes `deploy`, `destroy`, `init`, `configure`,
the bundle, the release workflows or the install path. It is also in
`.github/pull_request_template.md`.

- [ ] The rows of this list that the change can affect were run on an rc, and the
      evidence is attached (rows 3 and 7a to 7d are the ones with scripts)
- [ ] `scripts/e2e/selftest.sh` passes, and any new assertion script has a case
      that proves it fails
- [ ] Timings from rows 1 and 10 are in the description if a step got slower
- [ ] Anything not run is listed here with the reason; "not run" is allowed,
      "assumed" is not
