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
