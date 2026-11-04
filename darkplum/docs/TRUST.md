# What you are trusting

darkplum never receives your data. This is everything else you rely on.
`darkplum verify` proves the gateway's IAM role allows only the Bedrock models
you configured.

- **The `darkplum` binary is the trust anchor.** It carries the Terraform and
  Kubernetes manifests it applies and the identity (this repository's release
  workflow) that deployed images must be signed by. If you run a tampered
  binary, nothing else can protect you. The Homebrew formula pins each
  archive's SHA-256, written only after the release workflow verifies the
  published archives against their attestation; for a manual download, run
  the checks under [Install the CLI](../README.md#install-the-cli). The CLI checks
  darkplum's own images against that identity before copying them into your
  registry by digest. That catches a swapped image, not a tampered binary.
- **Third-party images are pinned by digest, not mirrored.** vLLM, the NVIDIA
  device plugin and the optional semantic router are pinned to exact digests
  in the binary but pulled by your cluster from Docker Hub, nvcr.io and
  ghcr.io. They are not copied into your registry or signature-checked.
- **The EKS API endpoint is public.** It is authenticated by AWS IAM and also
  reachable privately. This release does not restrict it to a list of
  networks.
- **The vLLM server container runs as root** (its model-download init
  container does not). The pod has no Kubernetes API token, accepts traffic
  from the gateway only, and may connect out only over public HTTPS.
- **The semantic router reads prompt text.** If enabled, its pod may connect
  to any public address on port 443 (private ranges stay blocked) because it
  downloads models from Hugging Face at startup.
- **There is no air-gapped install.** The machine running `darkplum deploy`
  needs `ghcr.io` and `pkg-containers.githubusercontent.com` (images and
  signatures), `tuf-repo-cdn.sigstore.dev` (signature trust root), the
  Terraform registry and provider downloads, and your AWS account's ECR, S3
  and STS endpoints. The cluster needs Docker Hub, nvcr.io and ghcr.io for the
  third-party images and `huggingface.co` for model weights.
