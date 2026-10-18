# Security

darkplum is deployed inside its users' own cloud accounts to keep their data
there. A vulnerability in it is a vulnerability in every installation, so
please report privately.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting: open the repository's
**Security** tab and choose **Report a vulnerability**. Do not open a public
issue or pull request for a security problem.

Include what you can: the affected service (gateway, api, web, cli,
Terraform, Kubernetes manifests), the version or commit, steps to reproduce,
and what an attacker gains. A proof of concept helps; an exploit against
someone else's installation is not acceptable.

You will get an acknowledgement within three working days and a plan, or a
reason the report is out of scope, within ten. Fixes ship as an ordinary
release with a note in the changelog that credits you unless you ask
otherwise. There is no bug bounty.

## What is in scope

- Any way for one user to read another user's conversations or private
  library items, or for an administrator to read them through the product.
- Any way a request reaches a backend class above the organisation's
  residency setting (self-hosted only, your cloud account only, any), or a
  model or tool server that a user's or key's allowlist or the tool-host
  allowlist does not permit. Residency is the product's core promise.
- Credential exposure: vendor keys, tool credentials, session or service
  keys appearing in Postgres, logs, responses or exports.
- Server-side request forgery from the api or gateway, including through
  tool servers.
- Authentication and session weaknesses, CSRF on state-changing routes.
- Supply-chain problems in the images, manifests or Terraform this
  repository ships.

Out of scope: vulnerabilities in the models themselves (prompt injection
that does not cross a policy boundary), rate limiting of a single user's own
requests, and findings that require the database credential or cluster-admin
access, which the operations guide documents as trusted. One known limit is
also documented rather than a finding: per-user and per-key model allowlists
are enforced by the gateway and are not yet backstopped by separate cloud IAM
roles; every principal shares the gateway's role until workspaces ship.

## Supported versions

Before 1.0, only the `main` branch receives fixes.
