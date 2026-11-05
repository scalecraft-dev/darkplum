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

## The Library bucket (optional)

The Library lives in the install's Postgres, like conversations. With
`library_bucket_enabled` (off by default), every library item is also written
to an S3 bucket in your account as a plain file (`library/v1/org/...` for
shared items, `library/v1/people/<person>/...` for private ones, the person
segment derived from the email at the time the account was created). Inside
the app, administrators never see private items.
In your cloud account, anyone your IAM grants `s3:GetObject` on the bucket can
read every file, private ones included; the bucket is private to the account,
encrypted at rest (SSE-S3 by default, or a key you name), reachable only over
TLS, and the gateway's role is denied every action on it by both its own
policy and the bucket's. The api's role is the only principal the install
grants access. Object keys carry folder names and the person segment, so they
appear wherever your account records S3 activity (CloudTrail data events, S3
Inventory) if you turn those on; nothing darkplum logs carries a key, a name
or a path. The bucket is versioned: a deleted item or user has every version
of its files purged; a rename or move starts a new history at the new key and
purges the old one. `darkplum destroy` keeps the bucket and its contents
unless you pass `--delete-library-data=<cluster_name>`. Turning the bucket
off (`library_bucket_enabled = false`) also keeps it, and the app stops
writing to it: files already there, private ones included, stay until the
bucket is turned on again (the queue then catches up, deletions included) or
deleted with that command.
