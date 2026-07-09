# OpenStack Management API

Self-service API for **delegated OpenStack resource management**. It lets an
organisation hand out resource quota along a delegation tree, lets users request
projects funded from those delegations, and reconciles approved projects into
real OpenStack projects. It is the backend of the DHBW self-service UI.

## Core concepts

- **Delegation** — a node in a budget-pool **tree**. A parent delegates a quota
  slice to a child (child limit ≤ parent). Two strategies:
  - `pool` — shared budget, projects need **manual approval**.
  - `allowance` — per-user cap, projects auto-approve up to that cap.
- **Project** — a resource request **funded by a delegation**. Lifecycle:
  `pending → approved → released` (plus `change_pending` for quota changes).
- **Usage rollup** — quota usage is aggregated live over the whole subtree, so
  every level sees the resource consumption below it.
- **Role switch** — a root admin can temporarily *assume a group identity* to
  act on its behalf (see `ROOT_ADMIN_TOKENS`).
- **Role provider** — pluggable source of a user's group tokens and group search:
  - `mock` — built-in test identities (no external dependency).
  - `http` — the external [role-provider-service](../role-provider-service).
- **Reconciler** — optional background loop that materialises approved projects
  into OpenStack (create/tag/quota) and cleans up released ones.

## Quick start

```bash
# Live-reload dev server (in-memory store, mock identities, dummy auth)
make dev            # API_MODE=development, listens on :8083

# Run the test suite
make test

# Build everything locally (tests + bundled docs + binary)
make all

# Container image (no tests — see below)
docker build -t openstack-management-api .
```

API is served under `/v1`. The OpenAPI spec is at `/swagger.json` and a bundled
TypeScript client at `/client` (consumed by the UI at runtime).

## Configuration

All configuration is via environment variables (`.env` is auto-loaded in dev).

| Variable | Default | Purpose |
|---|---|---|
| `API_MODE` | `production` | `development` enables dummy auth + verbose mode |
| `API_BIND` | `:8083` | Listen address |
| `API_DUMMY_AUTH` | `false` | Dev-only auth bypass via `X-Dummy-Auth-User` (refused when `API_MODE=production`) |
| `DB_TYPE` | `memory` | `memory` \| `sqlite` \| `postgres` |
| `DB_CONNECTION_STRING` | — | DSN for `sqlite`/`postgres` |
| `DB_ADD_MOCK_DATA` | `false` | Seed the mock delegation tree (only if the store is empty) |
| `ROLE_PROVIDER` | `mock` | `mock` \| `http` |
| `ROLE_PROVIDER_URL` / `ROLE_PROVIDER_API_TOKEN` | — | Required when `ROLE_PROVIDER=http` |
| `ROOT_ADMIN_TOKENS` | — | Comma-separated `user:`/`group:` tokens granted root admin + role-switch |
| `OIDC_ISSUER_URL` / `OIDC_CLIENT_ID` | — | OIDC bearer-token verification |
| `RECONCILER_ENABLED` | `false` | Enable the OpenStack sync loop |
| `RECONCILER_DRY_RUN` / `RECONCILER_NO_DELETE` | — | Reconciler safety switches |
| `OS_AUTH_URL`, `OS_APPLICATION_CREDENTIAL_ID/SECRET`, `OS_PROJECT_ID`, `OS_REGION_NAME` | — | OpenStack application credentials (only used when the reconciler is enabled) |

See [`internal/config.go`](internal/config.go) for the full list (including all
`RECONCILER_*` tuning knobs).

## Authentication

Requests authenticate with an **OIDC bearer token** (verified against
`OIDC_ISSUER_URL`). In `development` mode, `API_DUMMY_AUTH=true` allows
impersonating a test identity via the `X-Dummy-Auth-User` header. The caller's
group tokens come from the configured **role provider**; `ROOT_ADMIN_TOKENS`
elevates matching callers to root admin and enables role switching.

## Build & CI

`make` targets:

- `make all` — `test` + `bundle` + `build` (local default).
- `make image` — like `all` **without tests**; used by the Docker build.
- `make test` — `go test -cover ./...`.
- `make build` — compile the binary to `tmp/build/`.
- `make dev` — live-reload server (requires [air](https://github.com/air-verse/air)).
- `make update-deps` — update Go + npm dependencies.

The GitHub Actions workflow builds and pushes the `linux/amd64` image to
`ghcr.io/pfisterer/openstack-management-api`. **Tests are intentionally not run
in CI** — run them locally with `make test`. Image tags feed the ArgoCD
image-updater: `X.Y.Z-test.N` → staging, `X.Y.Z` → production.

## Deployment

Ships as a Helm chart in [`helm-chart/`](helm-chart) and is deployed via ArgoCD
from the `dhbw-deployment` repo (values rendered per environment).
