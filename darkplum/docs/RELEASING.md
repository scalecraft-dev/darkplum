# Releasing Darkplum

How a version becomes something a customer can install, and what has to be true
before anyone is told about it.

## What the workflows do

| Workflow | Trigger | Produces |
|---|---|---|
| `.github/workflows/release.yml` | a `vX.Y.Z` tag on a commit that is on `main` | signed images `ghcr.io/scalecraft-dev/darkplum-{gateway,api,web}:X.Y.Z`, the CLI for darwin/linux x arm64/amd64, `checksums.txt` (attested), `release.json`, all as a **draft** release |
| `.github/workflows/prerelease.yml` | a push to `prerelease/**` | release-candidate images `darkplum-rc-*` and an rc-channel CLI (workflow artifacts only) |
| `.github/workflows/_build-sign.yml` | called by the two above only | the image build, and a sigstore bundle (DSSE, v0.3) attached to each image digest as an OCI referrer |

Nothing in these workflows publishes. Publishing a draft is a person's decision after the smoke gate below.

### The order of a release run

1. **Gate** (`environment: release`, so a reviewer approves first, before anything is built or any identity token for the release workflow is issued). It checks the tag is a plain `vX.Y.Z` on a commit that is on `main`, then **waits up to an hour for the CI run of that exact commit and requires it green**, plus every job the release relies on. Tag after CI is green; a tag pushed a little early simply waits. If the gate fails (red CI, a timeout, a rejected approval) nothing has been built or signed, so **no version is burned**: fix or wait, then use **Re-run failed jobs**, which retries the gate alone.
2. **Images**: build, push, attach the sigstore bundle. Each image records the run attempt of its own job.
3. **CLI** for four platforms with the release manifest baked in.
4. **Verify**: the freshly built linux CLI runs `darkplum verify-release`, anonymously, against the images just pushed. This is the same verifier a customer's machine runs, so what CI accepts is what customers will accept.
5. **Draft release**.

If an image job has to be re-run, that job gets a new attempt number and the others keep theirs. `release.json` carries the attempt per image and the CLI matches each image's signing certificate against its own, so a partial re-run still verifies.

### What the CLI trusts

`release.json` is baked into the CLI at build time (`-ldflags -X`): the version, commit, run id, and per image a digest and the run attempt of the job that signed it. At install time the CLI verifies each image's bundle against an exact identity (no regular expressions):

- issuer `https://token.actions.githubusercontent.com`
- certificate workflow (the reusable one that ran the attest step): `.github/workflows/_build-sign.yml` at the release ref
- caller (`BuildConfigURI`): `release.yml` at `refs/tags/vX.Y.Z` for the release channel; `prerelease.yml` at `refs/heads/prerelease/...` for the rc channel
- source repository, ref, trigger (`push`), workflow commit, and the run id and attempt of the job that signed that image
- the signed statement is a SLSA build provenance attestation

A release-channel CLI therefore rejects rc images and an rc CLI rejects release images.

The CLI also **only trusts one repository**, compiled in, and checks `release.json` against it. The workflows build the CLI with `-X ...release.trustedRepository=$GITHUB_REPOSITORY`, so in `scalecraft-dev/darkplum` it is the product's repository (the compiled-in default) and in a fork or a scratch organisation it is that repository: a fork's binary trusts only the fork's own images. `darkplum version` prints `NON-DEFAULT TRUST` whenever the trusted repository is not the product's, the release `verify` job asserts that banner is absent in the product repository (and present anywhere else), and nothing at run time can change it.

## One-time repository settings (owner)

These cannot be set from code and the workflows assume them:

1. Tag protection (a ruleset) for `v*`: only maintainers can create them, no deletes, no updates.
2. Environments:
   - `release` with at least one required reviewer. The gate job waits on it.
   - `prerelease`, with deployment branches restricted to `prerelease/**`.
3. Actions may create packages, and the six GHCR packages (`darkplum-{gateway,api,web}`, `darkplum-rc-{gateway,api,web}`) are made **public after their first push**. The anonymous verify job, and every customer's verification, read the signatures without credentials; a private package fails verification, which is the right outcome until it is public.
4. Branch protection on `main` requiring the `Go (*)`, `Web`, `Bundle`, `Terraform`, `Kubernetes manifests and docs` and `Release plumbing` checks. The gate refuses a tag whose commit lacks any of them, and it reads the CI workflow's own run for the commit as the authority.
5. Workflow permissions default to read-only; every job declares what it needs.

## Verifying a release by hand

Anyone can check what the pipeline claims, with no Darkplum tooling:

```bash
# the CLI archive: signed by release.yml, from the tag
gh attestation verify darkplum_X.Y.Z_darwin_arm64.tar.gz --repo scalecraft-dev/darkplum \
  --signer-workflow scalecraft-dev/darkplum/.github/workflows/release.yml
sha256sum -c checksums.txt

# an image: the SLSA provenance attached to its digest
gh attestation verify oci://ghcr.io/scalecraft-dev/darkplum-api@sha256:<digest> --repo scalecraft-dev/darkplum \
  --signer-workflow scalecraft-dev/darkplum/.github/workflows/_build-sign.yml

# or with the CLI you downloaded: all three images, exact identity
darkplum verify-release
```

`gh attestation verify` matches the repository and the signing workflow; `darkplum verify-release` matches the full identity above (ref, caller, trigger, commit, run and attempt per image).

### Software bill of materials

There is no SBOM attestation yet, on purpose. buildx would produce it (and its own provenance) as extra attestation manifests, turning each image into an index; the mirror into ECR of such an index is not exercised yet (the rc end-to-end covers it first). The signed SLSA provenance is the build record today. Enable SBOMs after the rc e2e shows the mirror handling the extra manifests.

## Before the first release is merged: exercise the production identity path

The rc build uses the prerelease identity, so it does not prove the release path. Cut one real signed release from a scratch repository **in a separate GitHub organisation** with the same workflow files, so nothing can be mistaken for a product artifact: its images live under that organisation's own namespace, and the CLI that trusts them has to be built by hand with the matching trusted repository.

1. The owner creates a scratch organisation and a repository in it (for example `darkplum-scratch/darkplum-release`), and applies the settings above (tag protection, `release` environment, packages public).
2. Push this branch's `.github/`, `darkplum/`, `LICENSE` and `NOTICE` to the scratch repo's `main`, unmodified. Nothing in the workflows hard-codes the repository: the manifest records `github.repository` of the run, and images are named under its owner.
3. The scratch repo's `main` has no CI history, so for this exercise only, let its own CI run on `main` first (the same workflow files run there), then tag `v0.0.1`, approve the `release` environment, and let the workflow finish into a draft. Plain `vX.Y.Z` tags are fine in the scratch organisation.
4. In the scratch organisation the workflow builds the CLI trusting the scratch repository, so its own anonymous `verify` job exercises the production identity path end to end (`NON-DEFAULT TRUST` is expected there). Download the draft's linux archive with a clean `HOME` and run it by hand too:
   ```bash
   tar -xzf darkplum_0.0.1_linux_amd64.tar.gz darkplum
   ./darkplum version           # NON-DEFAULT TRUST: scratch-org/repo, channel release
   ./darkplum verify-release    # exact identity, anonymous, against the scratch images
   ```
   Then run it through the mirror steps once those exist, and confirm the verified identity names the scratch repo at `refs/tags/v0.0.1`. That the same binary refuses the product's manifest is covered by the unit tests; do not edit a manifest by hand here.
5. Negatives: re-sign an image from a branch in the scratch repo and confirm `verify-release` refuses it; confirm an rc image is refused by the release binary.
6. Publish to a scratch brew tap; `brew install` with a clean `HOME`; record the quarantine behaviour of the curl path.
7. Attach logs, digests and the verification output to the PR.

## After merge: smoke gate before anyone is told

The first real tag is cut and left as a **draft**. Until this checklist passes, nothing is published, README install instructions are not switched to the new flow, nothing is announced, and the brew tap is not bumped:

- [ ] Download the draft's artifacts with a clean `HOME` (no `~/.darkplum`, no `~/.terraform.d`, no `~/.kube`).
- [ ] `darkplum version` shows channel `release`, the tag, and three digests.
- [ ] `darkplum verify-release` passes for all three images against `refs/tags/vX.Y.Z` on the real repository.
- [ ] Mirror to a throwaway ECR; deploy Bedrock-only through the verification step.
- [ ] `checksums.txt` verifies (see "Verifying a release by hand").

Then publish the draft, and only then open the follow-up that switches the install docs.

## Revert point

The merge commit of the Makefile-removal change is the revert point: `git revert -m 1 <merge-commit>` restores the Makefile flow in one step.

What reverting does and does not undo:

- **An install that was only adopted** (`darkplum init --adopt`, nothing deployed since) goes back by hand: adoption renames `backend.tf` and `terraform.tfvars` to `*.adopted` and prints the two `mv` commands (and the `rm -rf` of the workspace) that undo it. Its state never moved.
- **An install that was adopted and then deployed with the new CLI must not go back to the Makefile's Terraform without reviewing a `terraform plan` first.** The new Terraform owns resources the old one does not know (the per-cluster ECR repositories, `<cluster>/darkplum-*`), pins different module versions, and declares `backend "s3" {}` with settings supplied separately. Applying the old code over it would plan to destroy the ECR repositories and change module pins. If the gate fails after real installs were deployed from the new CLI, revert the code, but run those installs only through a reviewed plan.
- If the post-merge gate fails before any customer install used the new flow: revert, do not forward-fix under time pressure.
