# Architecture — openstack-management-api

Self-service backend for **delegated OpenStack resource management** at DHBW.
Organizations hand out compute capacity along a **budget tree**, users request
projects funded from those budgets, and a reconciler materializes approved
projects as real OpenStack projects (and imports unknown ones back).

This document describes the system as of 2026-07 — after the rewrite of the
domain core to the **unified tree model** (`internal/tree`) and the matching
self-service-ui overhaul. The design analysis that motivated the rewrite lives
in [`__Modellanalyse.md`](__Modellanalyse.md) (German); its §5 is the blueprint
this implementation follows.

> **Scope:** this file is the architecture reference; `README.md` covers
> quick-start, configuration and build/CI and links here for everything else.

---

## 1. System context

```
                 ┌──────────────────┐
                 │   Browser (SPA)  │  self-service-ui (React/Mantine)
                 │  loads TS SDKs   │  one UI for DNS zones + cloud projects
                 └────────┬─────────┘
                          │ REST /v1 (OIDC bearer or dev dummy auth)
                          ▼
┌───────────────────────────────────────────────────┐
│           openstack-management-api (:8083)        │
│                                                   │
│  webserver ─ identity ─ tree.Service ─ tree.Store │
│                  │                        │       │
│                  │                   memory / pg  │
│            role provider                          │
│             (mock/http)          reconciler       │
└──────────┬────────────────────────────┬───────────┘
           │ REST + Bearer              │ Keystone/Nova/Cinder/Neutron APIs
           ▼                            ▼
┌─────────────────────┐        ┌──────────────────┐
│ role-provider-service│        │    OpenStack     │
│ (:8085, Zanzibar-    │        │  (bwCloud/ost.   │
│  style tuple store)  │        │   dhbw.cloud)    │
└─────────────────────┘        └──────────────────┘
```

- **self-service-ui** is a separate repository. It does not pin an API client:
  it downloads the bundled TypeScript SDK from this service **at runtime**
  (`/client`), so API changes only require rebuilding this backend.
- **role-provider-service** answers "which `group:` tokens does this user
  have?" and powers group search. In development a built-in mock can be used
  instead (`ROLE_PROVIDER=mock`).
- **OpenStack** is only touched by the reconciler and only when
  `RECONCILER_ENABLED=true`. The API itself never blocks on OpenStack.

---

## 2. Domain model: one tree, one node type

The entire domain is a single tree of `tree.Node` values
([internal/tree/model.go](internal/tree/model.go)):

- **Budgets** are the inner nodes — delegated capacity pools.
- **Projects** are the leaves — concrete resource allocation requests.

Both share one lifecycle, one authorization rule and one capacity mechanism.
This replaced a model with separate Delegation/Project entities whose
double-used fields caused four structural authorization bugs (F-1…F-4 in the
Modellanalyse); the old `/v1` API and `internal/applogic`/`storage` were
deleted without migration (no production users existed).

### 2.1 The three rules

Everything else is bookkeeping around these:

1. **Usage rollup.** `Usage(N)` = Σ `Limit` of all *active* descendant
   **leaves** of N (active = `approved` or `change_pending`). Budgets between
   N and the leaves contribute nothing themselves — only leaves consume
   capacity. The rollup is computed live per request and attached to API
   responses as `usage: {status → {limit, node_ids}}`; it is never persisted.

2. **Management authority walks the parent chain.** `canManage(N)` = caller
   has a token in the `AdminScope` of N **or of any ancestor**. Deciding on a
   node (approve/reject/transfer/delete/reparent) requires authority over the
   **parent chain exclusive of the node itself** — nobody approves their own
   budget request. Root admins are simply the `AdminScope` of the `root`
   node; there is deliberately **no separate root bypass code path**.

3. **Requesting.** Creating a child under N requires a token in
   `N.EligibleRequesters`; the child starts `pending`. If N carries an
   `AutoApprove` policy and the requester's cumulative active usage under N
   (matched by `Owner`) plus the request stays within
   `auto_approve.per_requester_limit` **and every ancestor has remaining
   capacity**, the request is approved immediately (auto-approve).

Capacity is enforced on every approval: for each ancestor A,
`Usage(A) + Δ ≤ A.Limit`. All capacity-relevant paths serialize on a single
service-level mutex (`approvalMu`) to keep check-then-write atomic.

### 2.2 Node lifecycle

```
             ┌──────────┐  approve   ┌──────────┐  release  ┌──────────┐
 request ───▶│ pending  ├───────────▶│ approved ├──────────▶│ released │ (terminal)
             └────┬─────┘            └───┬──▲───┘  (leaves) └──────────┘
                  │ reject               │  │
                  ▼                      │  │ approve (apply) /
             ┌──────────┐   request-     │  │ reject (discard change)
             │ rejected │   change       ▼  │
             └──────────┘ (terminal) ┌──────┴────────┐
                                     │ change_pending│
                                     └───────────────┘
 reconciler import ──▶ imported ── promote ──▶ pending (under a real budget)
```

Key decisions:

- **No `change_rejected` status.** Rejecting a change discards the proposed
  `PendingChanges` and returns the node to `approved` — the previously
  approved state stays valid. (The old model killed the whole project on a
  rejected change, orphaning its OpenStack resources.)
- **Budget requests are native.** Inner nodes can be created `pending` by an
  eligible requester and go through the same approval cycle as leaves.
- **Approve may modify.** A manager can approve with a `modified_limit`
  (grant less/different than requested); recorded in history.
- `rejected` and `released` are terminal; `imported` leaves are read-only
  until promoted.

### 2.3 Roles on a node

| Field | Who | Grants |
|---|---|---|
| `AdminScope` | managers (tokens) | approve/reject children, edit policy, create children directly; inherited downward |
| `EligibleRequesters` | consumers (tokens) | may request child nodes here — nothing else |
| `Owner` | exactly one `user:` token (leaves) | the responsible person; "My Projects" scope; membership in the OS project |
| `AuthorizedUsers` | list of token + OpenStack role | additional members of the OS project |

The strict separation of `AdminScope` (manage) and `EligibleRequesters`
(consume) is the fix for the old model's worst bug: allowance members could
approve each other because eligibility was expressed through admin scope.

**Delegation** is not a separate concept anymore: delegating capacity =
creating a sub-budget with someone else's token in its `AdminScope`.

**Ownership** is single-owner by design; managers of the parent chain can
transfer it (`transfer-owner`). Owner matters for auto-approve accounting
(per-owner, not per-group — F-1 fix) and for email-scoped views.

### 2.4 Bootstrap nodes

Created/ensured on every startup (`Service.Bootstrap`):

- **`root`** — the single parentless budget. Its `AdminScope` is
  **synchronized from `ROOT_ADMIN_TOKENS` on every start** (config is the
  source of truth; manual edits to root's scope do not survive restarts).
- **`unassigned`** — collection point for reconciler-imported leaves. Its
  limit is all-zero, which makes approving anything under it arithmetically
  impossible; imported leaves must be **promoted** (reparented) into a real
  budget.

### 2.5 Auxiliary node data

- `Pending *PendingChanges` — proposed limit/termination-date/member changes
  while `change_pending`.
- `History []HistoryEntry` — every lifecycle event with actor, from/to status,
  limit, parent, owner deltas.
- `Flags` — orthogonal markers; currently only `promote_on_reconcile`.
- `TerminationDate` — intended end of life. **Informative only for now**;
  enforcement (expiry sweep) is a known follow-up.
- OpenStack linkage (leaves, reconciler-maintained): `OSProjectID`,
  `OSProjectName`, `OSOvercommitted`, `ExternalGroupAssignments`.

---

## 3. Package layout

```
cmd/                       main() → internal/app
internal/
  app.go, config.go        wiring + env configuration (this is package app)
  common/                  shared types: TokenList, ProjectQuota (+ -1 = unlimited),
                           AuthorizedUser, Identity, RoleProvider iface, errors,
                           ManagedProject (resource definitions), pagination
  tree/                    THE domain: model, Service (all operations + authz),
                           Store iface, memory + postgres implementations
  identity/                role switch / impersonation, effective-token resolution,
                           assumable-identity listing (model-agnostic, composed
                           into tree.Service so middleware sees ONE service)
  webserver/               Gin: auth middlewares, /v1/nodes handlers, role-switch,
                           group search, reconciler admin, static SDK serving
  roleprovider/            RoleProvider implementations: mock (from mockdata) and
                           http (generated Go client for role-provider-service)
  reconciler/              two-way OpenStack sync (see §7)
  openstack/client/        thin Keystone/quota client (gophercloud-based)
  mockdata/                dev/test seed: identities + a small university tree
  generated_docs/          swagger.json + bundled TS client, embedded via go:embed
  helper/                  env parsing, misc
```

Layering (strict, top to bottom): `webserver → tree.Service (embeds identity)
→ tree.Store`. Handlers never touch the store; the reconciler consumes a
3-method structural subset of `tree.Store` (`ReconcilerStore`).

---

## 4. Authentication & identity

### 4.1 AuthN modes

- **Production:** OIDC bearer tokens (`OIDC_ISSUER_URL`/`OIDC_CLIENT_ID`).
  Claims → email + user token; group tokens come from the RoleProvider.
- **Development:** `API_DUMMY_AUTH=true` (refused unless
  `API_MODE=development` — dummy auth is hard-disabled in prod builds). The
  `X-Dummy-Auth-User: <email>` header selects the user; tokens are looked up
  in `mockdata` identities. The UI sets this header from its `?dev_user=` URL
  parameter.

  ⚠️ **Dev gotcha:** an email that is *not* a mock identity falls back to the
  **root** mock identity's tokens (so any dev email can drive the full API).
  Surprising-looking privileges in dev are usually this fallback.

### 4.2 Token model

Authorization never compares emails — it compares **tokens**:
`user:<email>` and `group:<name>`. A caller's effective identity is a
`TokenList`; every scope field on nodes is a `TokenList`. Group membership is
externalized to the **RoleProvider** (`mock` or `http` → role-provider-service).

### 4.3 Role switch (context switch)

Implemented in `internal/identity`, exposed via `/v1/role-switch`. Gated to
holders of `ROOT_ADMIN_TOKENS`. Two distinct modes:

| Mode | Trigger | Effective tokens | Effective email |
|---|---|---|---|
| **Group override** | `PUT {group_token}` | own non-group tokens + the one group (keeps `user:` and root grants) | unchanged |
| **Impersonation** | `PUT {impersonate_user}` | **replaced** by the target's tokens, resolved via **RoleProvider** (drops own root grant) | target's email |

⚠️ **Dev gotcha #2:** dummy auth resolves tokens from `mockdata`, but
impersonation resolves via the **RoleProvider**. With `ROLE_PROVIDER=http`
against a mock-seeded role-provider-service, the two sources can disagree —
e.g. the role-provider mock data may put `faculty@cs.example` into
`dept_cs_admin` and `root_uni`, while the local mock identity only has
`dept_cs_faculty`. Impersonated views then show more budgets than direct
dummy-auth login. This is a test-data inconsistency, not an authz bug.

`EffectiveAuthMiddleware` resolves (email, tokens) once per request into an
`AuthContext`; all handlers use only that.

---

## 5. HTTP API surface

All under `/v1`, JSON, bearer-authenticated (or dummy auth in dev). Swagger
annotations on the handlers generate `swagger.json` (see §8).

| Endpoint | Purpose |
|---|---|
| `GET /v1/config` | resource definitions (UI-visible ones), OpenStack roles, dummy dev users |
| `GET /v1/nodes/{id}` · `GET /v1/nodes/{id}/children` | node + children (usage attached) |
| `GET /v1/nodes/mine` | leaves owned by the effective email |
| `GET /v1/nodes/my-budgets` | budgets whose **own** `AdminScope` matches a caller token (flat list, no ancestor walk — the UI dedups nested entries) |
| `GET /v1/nodes/to-manage` | pending / change_pending / imported nodes the caller may decide on (parent-chain rule) |
| `GET /v1/nodes/eligible-for-me` · `eligible-for-owner` | budgets that accept requests from the caller / a given owner |
| `POST /v1/nodes` | request/create a node (budget or project; manager-created children skip pending) |
| `PUT /v1/nodes/{id}` | direct manager edit (name, scopes, limit w/ usage+parent checks, auto-approve policy) |
| `POST /v1/nodes/{id}/request-change` | owner-proposed change → `change_pending` |
| `POST /v1/nodes/{id}/approve` (opt. `modified_limit`) · `reject` · `release` | lifecycle decisions |
| `POST /v1/nodes/{id}/reparent` | move (requires authority over BOTH parents + capacity in new chain) |
| `POST /v1/nodes/{id}/transfer-owner` | change leaf owner |
| `POST /v1/nodes/{id}/promote` | imported leaf → flag for adoption under a real budget |
| `DELETE /v1/nodes/{id}` | delete (only without active descendants) |
| `GET/PUT/DELETE /v1/role-switch` · `GET /v1/role-switch/identities` | context switch (root-gated) |
| `GET /v1/groups/search` · `GET /v1/groups/mine` | group token search (RoleProvider) |
| `GET /v1/admin/reconcile/status` · `POST /v1/admin/reconcile/trigger` | reconciler admin (root-gated) |
| `GET /swagger.json` · `GET /client/*` | OpenAPI spec + bundled TS SDK (see §8) |

Error mapping is centralized (`errors.go`): domain sentinel errors →
HTTP status (403 authz, 404 unknown node, 409 capacity/state conflicts, …).

---

## 6. Storage

`tree.Store` is a small interface (Get/List by query/Upsert/Delete/IsEmpty/
Seed + identity listing). Queries are expressed as `NodeQuery` (kinds,
statuses, parent IDs, owner, `AdminScopeAny`, `EligibleAny`).

Two implementations:

- **memory** (`tree/memory.go`) — dev/tests; also used by the scenario tests.
- **postgres** (`tree/postgres.go`) — **one table `nodes`**: a small indexed
  row shell (id PK, parent_id, kind, status, owner) + the full node as JSONB
  `data`; token-scope queries use JSONB containment. GORM AutoMigrate creates
  it. Tables of the pre-rewrite model are simply no longer touched (they stay
  orphaned in existing databases; drop manually if desired).

Mock seed (`DB_ADD_MOCK_DATA=true`, only when the store is empty): a small
university — root, departments, a faculty pool, a student auto-approve budget
with `auto_approve`, example leaves in every lifecycle state, one `imported`
leaf under `unassigned`.

---

## 7. Reconciler (two-way OpenStack sync)

Optional background loop (`RECONCILER_ENABLED`, interval, manual trigger via
admin API). Consumes the tree store structurally; field mapping is
Quota→Limit, requester→Owner.

**Direction 1 — tree → OpenStack:** every `approved`/`change_pending` leaf is
projected as an OpenStack project: created on first encounter (named
`<node name> [<leafID>]`, tagged `ManagedProjectTag` + `<ResourceIDTagPrefix><id>`),
quota synced from the approved limit on every run (`change_pending` uses the
*currently approved* limit — proposed changes apply only after approval).
Name and description are re-synced too, so renaming a node renames its project.
The `[<leafID>]` suffix is required for correctness, not cosmetics: Keystone
enforces project-name uniqueness **per domain** (not per parent), so plain node
names would collide with each other and with foreign projects. Identification
always runs on the resource-id tag, never on the name. Keystone caps names at 64
characters and rejects non-BMP characters (emoji), so names are sanitized and
truncated. Keystone *groups* have no parent and no tags and therefore keep the
`GroupPrefix` (`RECONCILER_GROUP_PREFIX`, default `managed-`).
Members (owner + authorized users + group assignments) are synced to Keystone;
groups are auto-created and their memberships synced from the RoleProvider.

**Direction 2 — OpenStack → tree:** projects that carry the managed tag (or,
with `ScopeParentID`, any project under that parent) without a matching known
leaf are imported as synthetic **`imported` leaves under `unassigned`** with
their observed name/ID and quota. Vanished OS projects remove their imported
leaf again.

Run phases (`Reconcile`): 1 load both sides → 2 lookup maps → 2.5 promote
flagged imports (tag the existing OS project, node → `pending`, flag removed)
→ 3 sync groups/memberships → 4 tree→OS create/quota/members → 5 OS→tree
import/remove → 6 prune orphaned auto-created Keystone users.

**Safety modes:** `DryRun` (log only), `NoDelete` ("phase 1" rollout mode: no
destructive OS or store operations; released projects are only *tagged*).
Released leaves either delete the OS project (`DeleteReleasedProjects=true`)
or tag it `pending-deletion:<date>` + `contact:<email>` for external cleanup
workflows.

**Quota mapping** (`reconciler/mapper.go` + `ManagedProject` definitions in
config.go): each resource definition declares its OpenStack quota field,
optional multiplier (RAM GB→MB), linked fields (instances mirrors cores) and
whether it participates in **overcommit detection** — if a project's real OS
usage exceeds the granted limit, the leaf is marked `os_overcommitted` and the
project's ability to create resources is curtailed. Static (non-UI) resources
— networks, ports, volumes, … — are fixed at project creation from env-tunable
defaults.

---

## 8. Generated API clients (SDK pipeline)

```
swag annotations ─▶ swagger.json (OpenAPI 2) ─▶ swagger2openapi ─▶ openapi3
      (make generate-swagger-json)                                   │
                                                     @hey-api/openapi-ts
                                                                     ▼
 embedded.go (go:embed) ◀─ esbuild bundle (ESM+CJS) ◀─ TypeScript client
```

`make all` runs tests + this pipeline + build; `make bundle` is the pipeline
alone. The bundle is **embedded into the binary** and served at `/client/*`;
the UI imports it at runtime (`import(backendUrl + '/client/...')`). In
production the static endpoints send `Cache-Control: no-store` — a stale
cached SDK once silently hid new API operations from the UI.

A generated **Go client** for role-provider-service is produced the same way
(`make generate-role-provider-client` → `internal/roleprovider/api`).

Because generated directories are rewritten on every build, dev watchers must
exclude them (`run-development.sh` generates per-repo air configs for this).

---

## 9. Frontend integration (self-service-ui)

Separate repo (`../self-service-ui`), React + Mantine, section
`web/projects/`. Layered:

- `api-nodes.jsx` — `useNodesApi()` wraps every SDK operation with uniform
  error unwrapping; single place that touches the generated client.
- `util-project.jsx` — status vocabulary (approved="Active",
  pending="Awaiting approval", …), quota helpers.
- Reusable building blocks — `QuotaInputs`, `TokenListEditor`,
  `NodeUsageBars`, `NodeChangesDiff`, `NodeStatusBadge`, `BudgetTree`.
- Presentational cards (`card-budget.jsx`, `card-project.jsx`) emit
  `onAction(action, node)`; views own single modal instances.
- Views/tabs: **My Projects** (owner cards), **My Budgets** (master-detail:
  lazily loaded tree with guide lines, full-text search, expand/collapse all;
  the detail panel reuses the cards), **Approvals** (one flat sortable +
  filterable table, type icon per request, row click = details), **Root
  Admin**.

"Delegation" has no UI concept of its own — it is "create a sub-budget with
someone else in *Managed by*".

---

## 10. Testing

- **`webserver/api_scenario_test.go` is the acceptance oracle:** it builds the
  DHBW tree via the real HTTP API (including a budget request), drives the
  full lifecycle and asserts the usage rollup on every level. Regressions in
  authz/capacity almost always surface here.
- Unit tests live next to the code (`tree/service_test.go`,
  `api_nodes_test.go`, role-switch tests). The per-owner auto-approve
  regression (F-1) is only testable at tree level (dummy auth knows one mock
  student).
- Test helpers (`testhelpers_test.go`): `routerFromStore`, `do`/`mustDecode`,
  dummy-auth users.
- UI verification is Playwright-based (real Chrome against the dev stack);
  scripts currently live outside the repo.
- CI convention: images are built **without** running tests (amd64-only);
  `make test`/`make all` run tests locally.

---

## 11. Development & deployment

**Local stack:** `dhbw-deployment/run-development.sh` starts PowerDNS
(docker), dynamic-zones-api, role-provider-service (memory + mock data),
this API (memory, dummy auth, mock seed, `ROOT_ADMIN_TOKENS=group:root_uni`)
and the UI dev server, each under live-reload; any API change re-runs
`make all` so the embedded TS client stays fresh. A local `.env` (git-ignored)
can override — note it takes precedence and may e.g. point `ROLE_PROVIDER` at
the real role-provider-service (see the §4.3 gotcha).

**Configuration** is env-only (see README table): `API_MODE`, `API_BIND`,
`API_DUMMY_AUTH`, `DB_TYPE`/`DB_CONNECTION_STRING`/`DB_ADD_MOCK_DATA`,
`ROLE_PROVIDER[_URL|_API_TOKEN]`, `ROOT_ADMIN_TOKENS`, `OIDC_*`,
`OPENSTACK_*` (accepts `OS_*` fallbacks), `RECONCILER_*`,
`PROJECT_DEFINITIONS` (optional JSON override of resource definitions).

**Deployment:** Helm chart in `helm-chart/`, deployed via ArgoCD (staging
tracks `-test.N` prereleases, prod semver). Postgres in production; the tree
model only needs its own `nodes` table (AutoMigrate). Reconciler rollout
pattern: start with `RECONCILER_DRY_RUN=true`, then `NO_DELETE=true`, then
full.

---

## 12. Known gaps / follow-ups

- **TerminationDate is not enforced** — needs an expiry sweep (reconciler
  phase or scheduled job).
- **Mock-data inconsistency** between `mockdata` identities and the
  role-provider-service mock seed (different group memberships for the same
  emails) makes dev impersonation confusing (§4.3).
- **Dummy-auth root fallback** for unknown emails (§4.1) is convenient but
  surprising; consider an explicit opt-in.
- Old storage tables from the pre-rewrite model linger in existing databases
  (harmless; manual drop).
