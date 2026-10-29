# Runbook — Anthropic and OpenAI (external API backends)

Operational steps for the `external_api` backend class: releasing it, turning a
vendor on, rotating and removing keys, diagnosing a vendor that is not
answering, and — the part that needs care — rolling back.

Everything here runs in your own cluster. The vendor key is a Kubernetes Secret
(`darkplum-system/darkplum-provider-keys`), file-mounted into the gateway only.
A key can be installed, replaced and removed from **Admin > Models > Model
vendors** or with `make set-anthropic-key` / `make set-openai-key`; both write
the same Secret.

**Neither the key nor its ConfigMap mirror is in Git or Terraform**: a rebuilt
cluster has no vendor key until it is installed again. The catalog filter, once
chosen in the UI, lives in the database and survives the rebuild; a filter only
ever set from the command line lives in the ConfigMap and does not.

**Who holds what.** The api's ServiceAccount (`darkplum-api`) may `patch` that
Secret and the `darkplum-provider-config` ConfigMap, and nothing else: no other
Secret, no list, watch, create or delete. That is a bound on *reach*, not on
*reading*: a Kubernetes PATCH returns the object, so the api process could read
the vendor keys. It never does — it discards the response and no endpoint returns
a key — but treat an api compromise as a vendor-key compromise and rotate. The
gateway has no Kubernetes permission at all.

**Transport.** A key installed from the UI travels browser → load balancer →
web → api → Kubernetes API. Only the last hop is always TLS. The api refuses a
key unless Darkplum is served over HTTPS (`make expose-web CERT_ARN=…`);
`localhost` is the exception, which is also the way in before you have a
certificate: `kubectl port-forward -n darkplum-system svc/darkplum-web 8081:80`,
then `http://localhost:8081`. "Served over HTTPS" means the api is configured for
it (`SECURE_COOKIES=true` in the `darkplum-auth` Secret, which `make expose-web
CERT_ARN=…` sets): if you terminate TLS at your own ingress instead, the page is
HTTPS but the api still refuses a key until that setting is true and the api has
been restarted. Inside the cluster, web → api and
api → gateway (for the verification call) are plain HTTP, like the rest of
Darkplum's internal traffic, and the gateway accepts connections on its port only
from the api (`gateway-ingress` NetworkPolicy). If that is not acceptable for a
third-party spend credential in your environment, use the make target: terminal →
Kubernetes API over TLS, and nothing else sees the key.

**One api replica.** The install rate limit, the cached vendor status and the
install-time fast sync are per-process. They are correct at `replicas: 1`, which
is what ships; do not scale the api out without revisiting them. Both objects
are created empty by the deploy and are never given a `data` stanza in Git, so a
redeploy cannot wipe an installed key.

**One filter, two ways to set it.** The models ticked in the UI and `MODELS=` on
the command line are the same setting. The UI writes the database and mirrors it
into the ConfigMap; the make target writes the ConfigMap. Each write carries a
`<VENDOR>_MODEL_FILTER_SET_AT` stamp and the gateway uses whichever was written
last (stamps only ever move forward, so a laptop clock cannot make a write lose).
The backstop under all of this: a filter only decides which vendor models are
*catalogued*. Every newly catalogued model arrives **disabled**, and the
organisation's residency still refuses vendor models until an administrator
allows them — so a filter that is briefly broader than intended produces
disabled rows in Admin > Models, never a reachable model.

**Never hand-edit `<VENDOR>_MODEL_FILTER` in the ConfigMap**: without a newer
stamp the edit is ignored, the gateway logs a warning that the two disagree, and
the next UI change overwrites it.

## 1. Releasing this version

`make deploy` (everything) or `make deploy-platform` (api, gateway and web only,
when the infrastructure is unchanged). The order inside is already right — the
api migrates the database (migration 016) and must finish rolling before the
gateway rolls. `make deploy-k8s` on its own is **not** enough: it applies the
self-hosted model workloads and does not roll the api or the gateway.

Migration 017 adds `vendor_provider_settings` — the model list chosen in the UI
per vendor. A gateway that starts before it exists simply uses the command-line
filter.

What migration 016 changes for an existing install:

- Organisation residency gains a middle step. Every install whose organisation
  residency was *Any model* is moved to *Your cloud account only*. No traffic
  changes: before this release nothing outside the cloud account was reachable.
  An organisation already at *Self-hosted only* is untouched. `updated_at` is
  not changed, so the policy card does not claim someone edited it — and for the
  same reason there is no row for it in Admin > Activity. Put it in your own
  release notes so an administrator who notices the new label knows why.
- `inference_events` gains a nullable `backend_class` column. Rows written
  before the migration have none; Admin > Activity shows them as "Not recorded".

Mixed versions during the roll are safe in both directions of a roll-**forward**:
an old api never asks the gateway for vendor models (it does not send the
`X-Darkplum-Classes` header), so none can be catalogued or enabled; a new
gateway against a not-yet-migrated database keeps recording usage without the
new column and picks it up within 30 seconds of the migration.

`make deploy-model`, `remove-model` and `deploy-semantic-router` restart the
gateway without touching the api. That is safe for the same reason.

## 2. Turning a vendor on

Either path; they are interchangeable. **In the product:** Admin > Models > Model
vendors > *Install key* — paste, *Check key*, tick the models to offer, *Install
key*. The card shows *Installing* until the cluster has picked the key up, then
*Review and enable*. Nothing restarts. **From a terminal:**

```bash
read -s ANTHROPIC_API_KEY && export ANTHROPIC_API_KEY
make set-anthropic-key MODELS='claude-*'
```

1. Create the key at the vendor **scoped and capped first**: an Anthropic key
   created *inside* a workspace, with a workspace spend limit (a key created at
   organisation level works too, but Anthropic then requires a workspace ID on
   every request — give it on the install form or with `WORKSPACE=wrkspc_…`;
   the key check says "not scoped to an Anthropic workspace" when it is
   missing); an OpenAI project key with a model
   allowlist and a project budget. Darkplum has no spend cap and cannot see what
   the key is scoped to. This is the only control beneath the gateway.
2. `MODELS` is the catalog filter. Name a family (`claude-*`, `gpt-*`). A bare
   `*` is refused unless `ALLOW_ALL=1`, because a glob also admits models the
   vendor releases later.
3. The target checks the key against the vendor, prints what the filter admits,
   stores the key, and restarts the gateway once (first install or filter change
   only).
4. In **Admin > Models**, *Model vendors* shows the vendor as configured. Choose
   **Review and enable**: it loosens the organisation's residency to *Any model,
   including outside your account* (confirmed and written to the audit log) and
   enables the models you tick. Tighten individual users or API keys back to
   *Your cloud account only* under Users & Keys where needed.

Behind an egress proxy: set `HTTPS_PROXY` (and `NO_PROXY` for in-cluster names)
on the gateway Deployment. Vendor calls honour it.

## 3. Rotating a key

*Replace key* on the vendor's card, or re-run the make target with the new key
and no `MODELS`. The new key is verified with the vendor before anything is
stored, so a mistyped key cannot break live traffic. The gateway re-reads the
mounted file; there is no restart and no dropped request. Allow about two
minutes: the floor is the kubelet's Secret sync (about a minute by default;
80-90 seconds measured on a local kind cluster — not yet measured on EKS). During that window,
if the old key is already revoked at the vendor, vendor requests fail with
`502 provider_auth_failed`; local and Bedrock models are unaffected. Revoke the
old key at the vendor *after* the new one is answering.

### 3b. The end-user identifier sent to Anthropic and Bedrock (`darkplum-user-hmac`)

The OpenAI `user` field goes raw only to in-cluster vLLM and LM Studio. To
OpenAI (`user`), Anthropic (`metadata.user_id`) and Bedrock
(`requestMetadata.user_id`) it goes only as `<key id>:<hex HMAC-SHA256(secret,
user)>`, for example `k1:9f2c...` (67 characters, inside every vendor's
256-character limit and Bedrock's value charset). All three vendors receive the
SAME value for the same person; correlating one person across vendors is
therefore possible for anyone who sees more than one vendor's logs (accepted). The secret is the Kubernetes Secret `darkplum-user-hmac` in
`darkplum-system` (files `user-hmac-key`, `user-hmac-key-id`), created by
`make deploy-platform` and kept if it already exists. It exists nowhere else:
not Postgres, not the control plane, not an env var, never logged.

Same user plus same secret gives the same value, so the vendor can still tell
one end user from another; without the secret the value cannot be traced to a
person. Check it with `kubectl get secret darkplum-user-hmac -n darkplum-system`.

If the Secret is absent, unreadable, or the key is shorter than 16 characters,
the gateway starts normally and the field is left off OpenAI, Anthropic and Bedrock
requests (never sent raw). The gateway logs one WARNING
(`userid: user-hmac secret unavailable ...`) until the secret is usable again.
The omission is not yet recorded per request; the dropped-parameter record is
part of the next phase of GitHub #3.

**Rotating.** Rotating changes every pseudonym a vendor has seen, so vendor-side
per-user history stops lining up across the cut. Do it only on suspected
compromise or a policy requirement.

1. Choose a new key id (`k2`; lowercase letters, digits, `_` or `-`, up to 16
   characters) so old and new values are distinguishable in vendor logs.
2. Replace both files in one command:
   `kubectl create secret generic darkplum-user-hmac -n darkplum-system
   --from-literal=user-hmac-key="$(openssl rand -base64 48)"
   --from-literal=user-hmac-key-id=k2 --dry-run=client -o yaml | kubectl apply -f -`
3. No restart: the gateway re-reads the mounted files (kubelet sync, about a
   minute, then up to 3 seconds). Until the sync lands the old `k1:` values keep
   being sent; there is no window with a raw value.
4. `make deploy-platform` generates a key only when the Secret is NotFound; any
   other kubectl error aborts it, so a bad context can never replace a live key.
   Re-running it keeps the installed key and id. To rotate
   through make, run it with `USER_HMAC_KEY` and `USER_HMAC_KEY_ID` set.

## 4. When a vendor is not answering

`GET /health` on the gateway carries a `providers` array, and **Admin > Models >
Model vendors** shows the same state:

| state | meaning | action |
|---|---|---|
| `not_configured` | no key file mounted | *Install key* in Admin > Models, or `make set-…-key` |
| `filter_missing` | key present, no catalog filter | re-run `make set-…-key MODELS=…`. A hand-edited ConfigMap does nothing until the gateway restarts: `kubectl rollout restart -n darkplum-system deploy/darkplum-gateway` |
| `filter_matched_nothing` | the filter admits none of the models the key can reach | widen `MODELS`, or widen the key's scope at the vendor |
| `auth_failed` | the vendor rejected the key, or the key file is empty/unreadable | rotate (section 3) |
| *key check:* `workspace_required` | an organisation-level Anthropic key with no workspace ID | enter the workspace ID (Anthropic console > Workspaces), or use a key created inside a workspace |
| *key check:* `vendor_error` | the vendor answered the check with a non-auth HTTP error (status and error type shown) | the vendor's answer to this key: check its organisation, billing and permissions in the vendor console |
| `unreachable` | network, DNS, proxy or vendor outage | check egress (`gateway-egress` allows `:443`), `HTTPS_PROXY`, the vendor's status page |
| `ok` | discovery succeeded | — |

**"I installed a key in the UI and nothing happened."** Work down the path:

1. The form said *Nothing was stored* — verification failed, and the message
   says which way: *rejected this key* (auth), *not scoped to an Anthropic
   workspace* (add the workspace ID), *answered HTTP nnn* (the vendor's own
   objection to this key or organisation — billing, permissions), or *could not
   be reached* (a real transport failure: the gateway, not the api, makes that
   call, so check its egress, DNS and `HTTPS_PROXY`). The gateway log carries the
   same outcome with the vendor's HTTP status and a transport class, never the
   vendor's text or the key.
2. `503` "This cluster was deployed before keys could be installed from here" —
   the Secret, ConfigMap or the api's Role do not exist yet. Run
   `make deploy-platform` once. `503` "Darkplum is not permitted to write the
   vendor-key Secret" — the RoleBinding is missing or the api is not running as
   `darkplum-api`: `kubectl auth can-i patch secret/darkplum-provider-keys -n
   darkplum-system --as=system:serviceaccount:darkplum-system:darkplum-api`.
3. The card stays on *Installing* and then says *Taking longer than expected* —
   the key is stored; the gateway has not seen the file yet. Check the mount:
   `kubectl exec -n darkplum-system deploy/darkplum-gateway -- ls
   /var/run/darkplum/provider-keys` (the image is distroless; if `ls` is absent,
   read the vendor's `state` in the gateway's `/health` instead). A kubelet under
   pressure can take several minutes. The make target's restart is the
   break-glass: `make set-…-key` with the same key.
4. `409` "the gateway … is older than this page" — the gateway predates UI key
   install; upgrade it, or use the make target.
5. If a future NetworkPolicy restricts the api's egress, it must allow the
   Kubernetes API server (on EKS that is an `ipBlock`, not a pod selector) or
   installs fail at step 2's `503`.

What a caller sees: `429 provider_rate_limited` with the vendor's `Retry-After`;
`502 provider_auth_failed`; `502 upstream_error`; `504 upstream_timeout`. None
carries vendor error text. **There is no failover**: a vendor failure is never
retried on a local model or another vendor. The one exception is a routed
alias: when its picked route is down it may move to the alias's default route,
only as the alias's *If the model on a chosen route is down* setting allows (by default never
to a looser backend class, so a local outage is not sent to a vendor), and a
classifier failure always uses the default route. Separately, when a model id is registered with a stricter backend class and
only a looser class can answer it now, the request is refused with
`503 model_class_downgraded` rather than sent there (for example a self-hosted
id whose backend is down while a vendor lists the same id). The conditions: api
migration 028 applied and a gateway that reads it (apply api migrations first;
until then there is no check); the id must have been registered by a sync (an id
served only between two syncs is not); and an in-cluster source that once lists
a vendor's id pins that id to self-hosted. After upgrading, compare
`model_registered_classes` with your self-hosted inventory. To fix a wrong row,
or to move a model on purpose, accept the class it is discovered under now:
`PATCH /api/v1/models/:id` `{"accept_class": true}` (audited as
`model.class_accepted`; API only). A
vendor outage never affects self-hosted or Bedrock models, and never affects
gateway readiness.

Answering "did anything leave our cloud account?": Admin > Activity, *Where it
ran* = *Outside your cloud account*, or `GET /api/v1/usage?backend_class=external_api`
(the CSV export takes the same filter and carries `provider` and
`backend_class` columns). Refused requests carry the class too.

## 4b. Bedrock models that need an AWS opt-in (data retention)

Amazon Bedrock refuses some models — today Claude Fable 5 and Fable 5.1 — unless
the AWS account's **data-retention mode** for that region is `aws_review` (or the
legacy `provider_data_share`). Darkplum can list such a model (the account can
*see* it) while every request to it fails. What you see:

- **Admin > Models** marks the row **Needs AWS opt-in** with the account's
  current mode and the one the model needs. The gateway reads the mode each
  discovery tick (`bedrock:GetAccountDataRetention`, granted read-only to its
  role) and knows the families that require it; a model outside that list
  self-reports the same way after its first refused request.
- **Chat** answers "*<model> on Bedrock requires your AWS account's Bedrock
  data-retention mode to be raised*" — no retry — naming the route when the
  message came through a routed alias. Admin > Activity filters on *Needs AWS
  opt-in* (`denial_code = model_requires_data_retention`).

What the opt-in means: for models that require it, AWS retains prompts and
outputs **inside the AWS boundary for up to 30 days and may review them** to meet
the model provider's access condition; the model provider does not receive them,
and models that do not require review are unaffected whatever the setting. It is
per region, account-wide (projects exist only on the bedrock-mantle endpoint),
and there is no console UI for it. Darkplum never changes it — the gateway's
role cannot write it — because it is a data-handling decision for the account
owner. To opt in, with your own credentials, in the gateway's Bedrock region:

```bash
# read the current mode (`inherit` resolves to each model's default)
curl --aws-sigv4 "aws:amz:us-east-1:bedrock" --user "$AWS_ACCESS_KEY_ID:$AWS_SECRET_ACCESS_KEY" \
  -H "x-amz-security-token: $AWS_SESSION_TOKEN" https://bedrock.us-east-1.amazonaws.com/data-retention
# opt in
curl -X PUT --aws-sigv4 "aws:amz:us-east-1:bedrock" --user "$AWS_ACCESS_KEY_ID:$AWS_SECRET_ACCESS_KEY" \
  -H "x-amz-security-token: $AWS_SESSION_TOKEN" -H "Content-Type: application/json" \
  -d '{"mode":"aws_review"}' https://bedrock.us-east-1.amazonaws.com/data-retention
```

(`aws configure export-credentials --format env` prints those three variables for
an SSO profile; a current AWS CLI exposes the same as `aws bedrock
put-account-data-retention`.) The change applies immediately; the badge clears
on the next discovery tick. If your organisation's policy is zero or default
retention, the alternative is to route around these models: point the alias's
routes at models that do not require review, or do not enable them. An SCP
denying `bedrock:PutAccountDataRetention` unless the mode is `none` enforces that
organisation-wide — see the AWS data-retention documentation.

## 4c. Usage, cost and the billing key

Darkplum records every request's tokens with who sent it, which key, model,
alias, route and backend. **Admin > Activity > Spend** turns that into an
estimated cost at the vendors' published list prices (**Admin > Models >
Prices** holds the price list: seeded from the vendors' pricing pages on the
date shown, versioned, editable — replace a row when a vendor changes a price).
Estimates are per person, key, model, alias, route and backend; that
attribution exists nowhere else, because vendors only report by workspace,
project, key and model.

**Reconciling against the vendor's own numbers is optional and uses a separate
credential.** Anthropic's usage and cost reports need an **Admin API key**
(`sk-ant-admin01-…`, Console > Settings > Admin keys); the inference key —
even the organisation-level one — is refused, and a workspace key cannot read
them at all. OpenAI's `/organization/usage` and `/organization/costs` need an
organisation **admin key**, not a project key. These keys can list your
organisation's workspaces, projects and keys, so Darkplum treats them as a
different thing from the inference key:

- They live in their own Secret (`darkplum-system/darkplum-billing-keys`),
  mounted into the **api** only — the gateway never has them.
- They are used by one daily job (and once on install) that pulls the last 31
  days of usage and cost reports; nothing else reads them.
- Install from Admin > Models (the *Billing key* section on each vendor card,
  visibly apart from the inference key) or:

```bash
read -s ANTHROPIC_ADMIN_KEY && export ANTHROPIC_ADMIN_KEY
make set-anthropic-billing-key            # checks it can read usage first
read -s OPENAI_ADMIN_KEY && export OPENAI_ADMIN_KEY
make set-openai-billing-key
```

Spend then shows *vendor-reported* beside *estimated*, per day and per model,
with the difference. Expect small deltas: list price versus your contract,
rounding, and up to a day of reporting lag (Anthropic says usage appears within
about five minutes; cost reports are daily). Bedrock spend comes from AWS Cost
Explorer (`ce:GetCostAndUsage`, the api's own read-only Pod Identity role; a
local stack uses the developer's credentials with `DEV_AWS=1`), which lags up
to 24 hours and reports by usage type rather than model id.

Removing a billing key (`make remove-…-billing-key` or the card) stops the
pulls; estimates keep working. Revoke it at the vendor as well.

## 5. Turning a vendor off

*Remove key* on the vendor's card (it asks you to type the vendor's name, and
offers to turn the vendor's enabled models off in the same step; your model list
is kept for next time), or:

```bash
make remove-anthropic-key      # or remove-openai-key
```

The make target removes the key and the command-line filter and restarts the
gateway; the UI removes the key without a restart. The vendor's models
keep their settings, stop answering (`404 model_not_found`), and show as
unreachable in Admin > Models within about two minutes. Then revoke the key at
the vendor — removing it here does not invalidate it. To stop all vendor traffic
immediately without touching keys, set the organisation's residency back to
*Your cloud account only*; it takes effect within seconds.

## 6. Rolling back — read before `kubectl rollout undo`

Roll-forward is order-independent. **Roll-back is not.** The previous api and
web bundle do not know the `external_api` class: the old api would list an
enabled vendor model to the old chat picker, which would label it "Runs in your
infrastructure" while a current gateway sends it to the vendor.

**Gate: before rolling the api or web back past this release, drain the vendor
catalog.**

1. Remove both keys — *Remove key* on each vendor card, or
   `make remove-anthropic-key && make remove-openai-key`
2. In **Admin > Models > Policy**, set the organisation's residency back to
   *Your cloud account only*.
3. In **Admin > Models**, disable every model under the Anthropic and OpenAI
   cards. The summary line at the top of the tab must show no enabled
   External API model.

Before rolling the **gateway** back past the release that added UI key install,
also confirm the ConfigMap mirror matches what the UI shows — an older gateway
ignores the database and reads only the ConfigMap, and a stale ConfigMap filter
could be *broader* than the one chosen in the UI:

```bash
kubectl get configmap darkplum-provider-config -n darkplum-system -o jsonpath='{.data}'
```

Only then roll back, **gateway first, then api and web together**. Migration
016 does not need reverting: the previous release ignores the new column, reads
the legacy residency mirror (which holds `any` for *Your cloud account only* —
correct for a gateway that has no vendor backends), and treats per-user
*Your cloud account only* rows as "inherit", which is also correct once no
vendor backend exists. Rolling back the gateway alone is always safe: it simply
has no vendor backends.

## 7. Verifying a release

- `make dev-up`, then install a key from Admin > Models (or
  `make set-anthropic-key DEV=1 …`), exercises the whole path on a local kind
  cluster.
- After any deploy, the api's permission is checkable in one line each:
  `kubectl auth can-i patch secret/darkplum-provider-keys -n darkplum-system
  --as=system:serviceaccount:darkplum-system:darkplum-api` must say `yes`, and the
  same for `secret/darkplum-auth` must say `no` (the role reaches that one Secret
  only).
- The **Vendor smoke** GitHub workflow (manual, uses the repository's
  `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` secrets) makes real discovery,
  completion and streaming calls through the gateway's provider packages. Run it
  before releasing a change to `services/gateway/internal/provider/{anthropic,openai}`.
