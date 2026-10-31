<!-- One change per pull request. Say what it does and why; the commit message should stand on its own. -->

## What this changes

## Why

## How it was verified

<!-- Tests added or changed; what you ran locally (go test, pnpm run typecheck / check:parity / build, scripts/dev.sh up); anything you could not verify. -->

## Checklist

- [ ] Tests cover the change (a bug fix carries the test that would have caught it)
- [ ] Routes: `docs/api-routes.json`, `services/web/src/lib/routes.ts` and `docs/API.md` (`go run ./cmd/apidoc`) updated together, if a route changed
- [ ] Migrations are additive and numbered, if the schema changed
- [ ] No user content reaches logs or audit rows; no credential reaches Postgres, logs or responses

## Install path and releases

<!-- Only for changes to deploy, destroy, init, configure, the bundle, the release workflows or anything else on the install path. Delete the section otherwise. The list and its scripts: darkplum/docs/E2E-RELEASE-CHECK.md -->

- [ ] The rows of the e2e list that this change can affect were run on a release candidate, and the evidence is attached (rows 3 and 7a to 7d have scripts in `darkplum/scripts/e2e/`)
- [ ] `darkplum/scripts/e2e/selftest.sh` passes; a new assertion script has a case that proves it fails
- [ ] Timings from `darkplum deploy` / `darkplum configure` are in the description if a step got slower
- [ ] Rows not run are listed with the reason ("not run" is allowed, "assumed" is not)
