# OpenStack Management API

Self-service API for **delegated OpenStack resource management**. It lets an
organisation hand out compute capacity along a **budget tree**, lets users
request projects funded from those budgets, and reconciles approved projects
into real OpenStack projects. It is the backend of the DHBW self-service UI.

➡️ **Architecture:** see [`ARCHITECTURE.md`](ARCHITECTURE.md) — domain model,
authorization rules, API surface, storage, reconciler, SDK pipeline.

## Core concepts

The whole domain is **one tree of nodes** (`internal/tree`):

- **Budget** — an inner node: a delegated capacity pool. Its `admin_scope`
  tokens manage it (approve/reject children, edit, delegate further); its
  `eligible_requesters` tokens may request child nodes under it. Delegation =
  creating a sub-budget with someone else in `admin_scope`. A budget may carry
  an `auto_approve.per_requester_limit` policy: requests within that
  per-person cap are approved automatically (self-service).
- **Project** — a leaf node: a concrete resource request with exactly one
  `owner`. Lifecycle: `pending → approved → released`, with
  `change_pending` for proposed changes (a rejected change simply returns the
  node to `approved`). Budgets go through the same request/approval cycle.
- **Usage rollup** — capacity is consumed only by active leaves and
  aggregated live over every subtree; approving anything checks capacity
  along the whole ancestor chain.
- **Authorization walks the parent chain** — deciding on a node requires a
  token in an *ancestor's* `admin_scope`; nobody approves their own request.
  Root admins are simply the `admin_scope` of the `root` node (synchronized
  from `ROOT_ADMIN_TOKENS` at startup).
- **Role switch** — a root admin can temporarily act within a single group or
  fully impersonate another identity (see `ARCHITECTURE.md` §4.3).
- **Role provider** — pluggable source of a user's group tokens and group search:
  - `mock` — built-in test identities (no external dependency).
  - `http` — the external [role-provider-service](../role-provider-service).
- **Reconciler** — optional background loop that materialises approved leaves
  into OpenStack (create/tag/quota/members), imports unknown OpenStack
  projects as `imported` leaves under the `unassigned` node, and cleans up
  released ones.

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
| `DB_TYPE` | `memory` | `memory` \| `postgres` |
| `DB_CONNECTION_STRING` | — | DSN for `postgres` |
| `DB_ADD_MOCK_DATA` | `false` | Seed the mock budget tree (only if the store is empty) |
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
