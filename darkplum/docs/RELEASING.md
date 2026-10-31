# Releasing Darkplum

How a version becomes something a customer can install, and what has to be true
before anyone is told about it.

## What the workflows do

| Workflow | Trigger | Produces |
|---|---|---|
| `.github/workflows/release.yml` | a `vX.Y.Z` tag on a commit that is on `main` with green CI | signed images `ghcr.io/scalecraft-dev/darkplum-{gateway,api,web}:X.Y.Z`, the CLI for darwin/linux x arm64/amd64, `checksums.txt` (attested), `release.json`, all as a **draft** release |
| `.github/workflows/prerelease.yml` | a push to `prerelease/**` | release-candidate images `darkplum-rc-*` and an rc-channel CLI (workflow artifacts only) |
| `.github/workflows/_build-sign.yml` | called by the two above only | the image build, and a sigstore bundle (DSSE, v0.3) attached to each image digest as an OCI referrer |

Nothing in these workflows publishes. Publishing a draft is a person's decision after the smoke gate below.

### What the CLI trusts

`release.json` is baked into the CLI at build time (`-ldflags -X`): the version, commit, run id, and one digest per image. At install time the CLI verifies each image's bundle against an exact identity (no regular expressions):

- issuer `https://token.actions.githubusercontent.com`
- certificate workflow (the reusable one that ran the attest step): `.github/workflows/_build-sign.yml` at the release ref
- caller (`BuildConfigURI`): `release.yml` at `refs/tags/vX.Y.Z` for the release channel; `prerelease.yml` at `refs/heads/prerelease/...` for the rc channel
- source repository, ref, trigger (`push`), workflow commit, and run id from `release.json`

A release-channel CLI therefore rejects rc images and an rc CLI rejects release images.

## One-time repository settings (owner)

These cannot be set from code and the workflows assume them:

1. Tag protection (a ruleset) for `v*`: only maintainers can create them, no deletes, no updates.
2. An environment named `release` with at least one required reviewer. The `draft` job waits on it.
3. Actions may create packages; the three `darkplum-*` and three `darkplum-rc-*` GHCR packages are made public after their first push (customers pull from ECR, but the CLI verifies against GHCR and the source images are public by design).
4. Branch protection on `main` requiring the `Go (*)`, `Web`, `Terraform`, `Kubernetes manifests and docs` and `Release plumbing` checks. `release.yml` refuses a tag whose commit lacks any of them.
5. Workflow permissions default to read-only; every job declares what it needs.

## Before the first release is merged: exercise the production identity path

The rc build uses the prerelease identity, so it does not prove the release path. Cut one real signed release from a scratch repository with the same workflow files:

1. The owner creates a scratch repository (for example `scalecraft-dev/darkplum-release-scratch`) and applies the settings above.
2. Push this branch's `.github/`, `darkplum/`, `LICENSE` and `NOTICE` to the scratch repo's `main`, unmodified. Nothing in the workflows hard-codes the repository: the trusted repository is whatever `release.json` records, which is `github.repository` of the run. (The repository must stay in the `scalecraft-dev` organisation; `release-json.py` refuses any other owner.)
3. The scratch repo's `main` has no CI history, so for this exercise only, push a commit status or check run named for each required CI job, or temporarily run the `gate` job's CI check with the scratch repo's own CI. Then tag `v0.0.1`, approve the `release` environment, and let the workflow finish into a draft.
4. Download the draft's artifacts with a clean `HOME`. Run `darkplum version` (channel `release`, tag, commit, run id, digests) and install through the verification and mirror steps. Confirm the verified identity names the scratch repo at `refs/tags/v0.0.1`. A release-channel binary has no runtime override for the trusted repository: it only ever trusts the repository baked into its own `release.json`.
5. Negatives: re-sign an image from a branch in the scratch repo and confirm the release binary refuses it; confirm an rc image is refused by the release binary.
6. Attach logs, digests and the verification output to the PR.

## After merge: smoke gate before anyone is told

The first real tag is cut and left as a **draft**. Until this checklist passes, nothing is published, README install instructions are not switched to the new flow, nothing is announced, and the brew tap is not bumped:

- [ ] Download the draft's artifacts with a clean `HOME` (no `~/.darkplum`, no `~/.terraform.d`, no `~/.kube`).
- [ ] `darkplum version` shows channel `release`, the tag, and three digests.
- [ ] All three images verify against `refs/tags/vX.Y.Z` on the real repository.
- [ ] Mirror to a throwaway ECR; deploy Bedrock-only through the verification step.
- [ ] `checksums.txt` verifies: `gh attestation verify darkplum_*.tar.gz --repo scalecraft-dev/darkplum`.

Then publish the draft, and only then open the follow-up that switches the install docs.

## Revert point

The merge commit of the Makefile-removal change is the revert point: `git revert -m 1 <merge-commit>` restores the Makefile flow in one step. Installs created by the old flow keep working untouched (adoption renames, never deletes), so reverting does not strand anyone. If the smoke gate fails, revert; do not forward-fix under time pressure.
