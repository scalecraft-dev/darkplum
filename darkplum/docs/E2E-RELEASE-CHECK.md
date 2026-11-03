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
| 4a | Ctrl-C during the apply | Interrupt `darkplum deploy` during the Terraform apply, about 2 minutes in, while EKS, the NAT gateway and the load balancer are being created; once more (second Ctrl-C); check the lock; then re-run until it converges. Interrupt a second time after the cluster is ACTIVE but before the apply finishes. Expected on the re-runs: with the cluster still CREATING, step 3 prints `waiting for cluster NAME: CREATING (...)` and then `cluster  untainted NAME`; with the cluster ACTIVE and no access entry yet, the preflight shows `! web exposure  the cluster does not accept this identity's credentials yet` (a warning, not a stop) and step 3 shows `kubernetes access  not made yet`; the plan lists the NAT gateway, load balancer and any other half-built resource as `replace ... (tainted: left half-built by an interrupted or failed apply)` and, run without `--confirm-plan`, stops saying this install's first deploy was interrupted, every item is tainted by it and none holds data, and that replacing them is how it finishes (a list that holds a database, or a mix with a change the configuration asked for, does not say that); with `--confirm-plan` it applies and converges. It must never fail with `409 ResourceInUseException: Cluster already exists`. | both logs; the re-run's "infrastructure" row |
| 4a2 | A true second Ctrl-C during CREATING | Start `darkplum deploy` on a fresh install; about 2 minutes into the apply, with EKS CREATING, press Ctrl-C and then Ctrl-C again within the 10 seconds darkplum waits (it kills Terraform and exits). Then re-run. Expected: the kill message names that the lock may be held; the re-run either reports the state lock with the exact `cd ... && env TF_DATA_DIR=... terraform -chdir=... force-unlock <id>` line (paste it, after checking nothing else runs) and succeeds after that, or proceeds as in 4a. Record which, and whether the state lists `aws_eks_cluster.this[0]`: if it does not (the cluster exists in AWS but not in the state), the re-run must stop naming that, with or without `--confirm-plan`, while the cluster is CREATING and once it is ACTIVE, and must never reach a 409: see the known limits below | both logs; `terraform state list`; `aws eks describe-cluster` status |
| 4b | Session expires mid-apply | Use a short session or `aws sso logout` while applying. The next run must stop at preflight naming the session, resume and converge. Capture the `ExpiredToken` text | logs |
| 4c | The api fails | Break the database security group; the failure message must carry the pod events or logs. Fix, re-run | the failure output |
| 4d | A bad image stops early | Deploy an rc image re-signed from a non-prerelease ref, and an unsigned one: deploy must stop at step 2 before Terraform, and nothing is written | the output; `terraform state list` unchanged |
| 5 | A newer release rolls | Deploy rc N, then rc N+1: pods roll to the new digests and nothing references the old digest | `darkplum version`, pod `imageID`s |
| 6 | The earlier survivors, on the live cluster | Headless vLLM Services (`clusterIP: None`); NVIDIA device plugin running on the GPU node; gateway egress policies carry real service and VPC CIDRs (no `PLACEHOLDER`); EKS log group imported on reinstall (row 8); `api-ingress` policy; `Recreate` on the vLLM Deployment; `imagePullPolicy` as intended with digest refs | `kubectl get` output, pinned with `darkplum kubeconfig` |
| 7a | Destroy ignores the ambient context | `E2E_CONFIRM_DESTROY=<cluster> destroy-decoy-context.sh --install NAME --decoy-context CTX` | script output: the decoy's objects are identical before and after, and the destroy log never names it |
| 7b | Destroy with an expired session | `E2E_CONFIRM_DESTROY=<cluster> E2E_CONFIRM_LOGOUT=1 destroy-expired-session.sh --install NAME` | script output: refused, nothing started, RDS deletion protection and workloads identical after login |
| 7c | Destroy when the cluster is already gone | Delete the cluster in the console, then `E2E_CONFIRM_DESTROY=<cluster> destroy-cluster-gone.sh --install NAME` | script output: loud skip of the Kubernetes phase, finishes (if it fails instead, the same listing as 7f is printed) |
| 7d | Destroy with the wrong account | `E2E_CONFIRM_DESTROY=<cluster> destroy-account-mismatch.sh --install NAME` (it works on a copy of the record) | script output: refused before Terraform, cluster unchanged |
| 7e | Full destroy | `darkplum destroy`: finishes; the leftover report lists the snapshot, the audit log group, the state bucket and KMS key; the orphan query (load balancers, ENIs, EBS, security groups tagged for the cluster) returns none; `terraform state list` is empty | the output, the empty state list |
| 7f | Destroy of an interrupted install whose cluster rejects the identity | `darkplum destroy` on (a) an install whose first deploy was interrupted before any output existed, and (b) one whose destroy was interrupted in `terraform destroy` after its Kubernetes phase. Expected: a warning that names what is skipped (the check for load balancers and volumes outside the Darkplum namespaces, the cluster's release record, the deletion of Darkplum's Services and volume claims), the destroy goes on, and if it fails the load balancers and network interfaces tagged for the cluster are listed anyway, along with the install's web load balancer by name (it has no cluster tag). A first deploy interrupted after its outputs exist counts too: when this workspace's deploy clock shows no deploy has ever completed and the state has no access entry, the destroy goes on. An install with outputs whose audit log group is still in the state and no such clock, or one whose state holds the access entry, must STOP; for the first of those the message says `darkplum deploy` is the way out. See `destroy_resume_test.go` | the destroy output |
| 8 | Reinstall after destroy | `init` + `deploy` on the same account: the log group row says "the plan adopts", the plan says "1 to import", the final snapshot untouched, install succeeds. Run it from a clean `HOME` as well as the same workspace: `init` must accept the empty state `destroy` left (`a destroyed install leaves one`), and refuse it if it is locked. This is the real test that leftovers are harmless | logs |
| 9 | Teardown with the private load balancer | Destroy an install with a web address (`modules/web-access`): the NLB does not block VPC deletion | the destroy log |
| 10 | Timings | Collect each step's time and the total from `deploy` and `configure`, the `ExpiredToken` behaviour from 4b, and the time to first chat from `configure` | the numbers, in the PR; use them to set ETAs and timeouts |

### Further rows (never run before)

| # | Row | How | Evidence |
|---|---|---|---|
| 11 | `init --adopt` from an install the Makefile created | One install created from the last Makefile-era commit (S3 backend), then `init --adopt` and `deploy` as an upgrade: `:latest` becomes digests, the plan gate stops on any replacement. A local-backend install is refused with instructions | logs; the plan |
| 12 | The rc refuses release identities and the reverse | The rc binary against a release manifest, the release binary against rc images | both refusals |
| 13 | A real ECR copy | Repeat PUT into an immutable repository; the attestation children are present in the mirrored index; note the time on a laptop | `darkplum deploy` log, `aws ecr describe-images` |
| 14 | vLLM downloads as non-root | The init container runs with the real `snapshot_download` | pod logs |
| 15 | Connectivity right after the first apply and across a roll | api -> RDS and gateway -> api and Bedrock work immediately after the first apply (policies are applied before workloads). Then, through the real load balancer address from inside the VPC, a new connection per request, about 2 chats a second, a 5s client timeout and no retries: during an rc N to N+1 `deploy`, and across 6 restarts each of api, gateway and web. Expected: no 5xx and no timeouts. A clean run bounds the failure rate only roughly (0 in 500 is under about 0.6%). Long streams are row 15b, and node loss is out of scope | the probe's output, per restart |
| 15b | A reply cut mid-stream is not presented as finished | A streamed reply of about 20s through the real load balancer from inside the VPC, with a pod force-killed under it (gateway, api, web), then rolled (`rollout restart`). Expected, as a client sees it: a kill of the gateway ends in a `stream_interrupted` frame, a kill of the api or web in a broken transfer (curl exit 18), a roll in a whole reply. Never a clean end with no `finish_reason` or `[DONE]`. Then in a real browser, through `darkplum configure`'s address, for the gateway and the api: the card reads "The reply was cut off", what arrived stays above it; Try again sends one user turn and no assistant turn; a reload shows the question and neither the partial nor the card. Web pods cannot be tested this way, since the port-forward dies with them | the curl results per case; a screenshot of the card |
| 16 | Exit 3 (platform healthy, no GPU) | Deploy with a GPU request the account cannot satisfy: exit status 3, `configure` still works with Bedrock | exit status, output |
| 17 | `inference_region` is always set | The fresh apply of row 1 prints the output non-empty, with and without a Bedrock model allowed | `terraform output inference_region` |
| 18 | Providers on a customer machine | `init -lockfile=readonly` works on darwin/arm64, darwin/amd64 and linux/amd64 with a clean plugin cache | init logs |
| 19 | The README path from nothing | Follow the README from a clean machine, once with Homebrew and once with the manual download and its attestation check | the transcript |
| 20 | Production identity | A scratch-organisation `v0.0.1` release, including the negatives and the Homebrew round trip ([RELEASING.md](RELEASING.md), "Before the first release is merged") | logs, digests |
| 21 | Draft smoke before anyone is told | [RELEASING.md](RELEASING.md), "After merge", then approve the `tap` job and `brew install` + `brew test` | the checklist, ticked |

Rows 0 to 10 need only the rc. Rows 20 and 21 are the only ones that need a real
release; until they have run, the install instructions in the READMEs say the
release does not exist yet.

## Known limits of the interrupted-install handling

These are filed, not fixed. A run that meets one must stop with a message, not guess.

- **Hard kill during CREATING.** If darkplum or Terraform is killed outright (a second Ctrl-C, SIGKILL, power loss, a lost machine) while EKS is being created, the cluster exists in AWS but is not in the state, the lock is still held, and other half-made objects (the load balancer) can be outside the state too. Seen on rc.5: the lock is reported with its release command, and after that `deploy` stops with `exists in AWS ... state does not hold it`, before any apply. The cluster may belong to someone else (same name, same account), so darkplum does not delete it. A person reads what it is and, if it is this install's, deletes it. `destroy` then fails on the VPC until the load balancer `<cluster>-web` is deleted too, and names it (seen on rc.5, which did not name it). Whether `deploy` can continue past a load balancer left outside the state has not been observed. **Importing it does not work:** the import cannot carry `bootstrap_self_managed_addons`, and the plan then replaces the cluster, which is refused.
- **Ctrl-C between the access entry and its policy association.** The entry exists in the state, so the 401 is "real" to darkplum, but the identity has no policy and every Kubernetes read is answered 403 (Forbidden), which is never waited out. The re-run stops at the preflight each time. Today the way out is to let Terraform finish (`terraform apply` of the install's saved root, by hand) or to remove the entry; darkplum does not do either.
- **A finished install with no deploy clock in this workspace.** The clock is per workspace. A finished install re-deployed from another machine, killed after its access entry has left the state, looks like a first deploy that never finished: `destroy` warns and skips the Kubernetes phase, so Darkplum's load balancer Services and volume claims are not deleted. It takes all three at once, and the warning names what was skipped.
- **A stream still running when its pod stops.** The servers finish in-flight work for 10s after SIGTERM, then exit, so a long streaming answer can be cut by a roll. The person sees it was cut (row 15b): what arrived stays on screen above a failure card, and is neither saved nor sent back to a model. An API client gets a `stream_interrupted` error frame or a broken transfer, and one cut partway through a line probably gets a JSON decode error first. Other mid-stream errors, such as a rate limit after text has arrived, still drop what arrived.
- **A tainted log group, subnet, security-group rule or CIDR reservation** is refused with a command to untaint it by hand; only the cluster is untainted automatically.

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
