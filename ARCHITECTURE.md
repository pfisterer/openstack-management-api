# Architecture — openstack-management-api

Self-service backend for **delegated OpenStack resource management**. Organizations hand out capacity along a **budget tree**, people request projects funded from those budgets, and a reconciler makes the approved state true in OpenStack (and imports projects it does not know back into the tree).

> **Scope:** `README.md` explains why the service exists, what it does, how to configure, build and deploy it. This file goes one level deeper: the packages, how a request and a reconcile pass flow through them, and the invariants the code relies on. Where the two overlap, the README is the short version.

---

## 1. System context

```
 ┌───────────────────────────┐          ┌───────────────────────────┐
 │ Web UI (self-service-ui)  │          │ MCP client, script, CI    │
 │ generated TS client, npm  │          │ API token (os_mgt_…)      │
 └─────────────┬─────────────┘          └─────────────┬─────────────┘
               │ REST /v1, OIDC bearer                │ /mcp or /v1
               ▼                                      ▼
┌──────────────────────────────────────────────────────────────────────┐
│                  openstack-management-api (:8083)                    │
│                                                                      │
│   webserver ──────────▶ tree.Service ──────────▶ tree.Store          │
│   (REST, MCP, auth)     (embeds identity)        (memory | Postgres) │
│        │                      │                        ▲             │
│        │ token lookup         │ group tokens           │ ListNodes / │
│        ▼                      ▼                        │ UpsertNode  │
│   token.Service          RoleProvider            reconciler          │
│   (shared golib)         (mock | http)           (supervised loop)   │
└───────────────────────────────┬──────────────────────────┬───────────┘
                                │ REST + bearer            │ Keystone, Nova,
                                ▼                          ▼ Neutron, Cinder, Glance
                  ┌───────────────────────────┐  ┌───────────────────────┐
                  │  role-provider-service    │  │       OpenStack       │
                  └───────────────────────────┘  └───────────────────────┘
```

- **Clients** use the published OpenAPI description. The browser UI depends on the generated TypeScript client as an npm package at build time; the service no longer serves a client at runtime (§10).
- **role-provider-service** answers "which `group:` tokens does this person hold?", lists group members and searches groups and users. A built-in mock replaces it for local work (`ROLE_PROVIDER=mock`, the default).
- **OpenStack** is only touched by the reconciler, and only when `RECONCILER_ENABLED=true`. No API request waits on OpenStack; the API only reports whether provisioning is running (`provisioningEnabled` in `/v1/config`).

---

## 2. Domain model: one tree, one node type

The whole domain is a single tree of `tree.Node` values ([internal/tree/model.go](internal/tree/model.go)):

- **Budgets** (`kind: budget`) are the inner nodes — delegated capacity pools.
- **Projects** (`kind: project`) are the leaves — concrete allocations that become OpenStack projects.

Both share one lifecycle, one authorization rule and one capacity mechanism. This replaced an earlier model with separate delegation and project entities, whose double-used fields let allowance members approve each other; that model's code is gone, and its three tables are dropped at startup (§9).

### 2.1 The three rules

Everything else is bookkeeping around these:

1. **Usage rollup.** The usage of a budget is the sum, over every *charged* descendant **leaf**, of what that leaf costs. Budgets in between contribute nothing themselves. Only quantities (`kind: count`) are summed; availabilities never are (§3). Which leaves are charged and what each one costs is the accounting's decision (§4). The rollup is computed live per request (`loadSubtreeUsage`: one BFS over the budget subtree, one batched query for its leaves, then each leaf's cost is attributed up its chain) and attached to responses as `usage: {status → {limit, node_ids}}`. It is never persisted.
2. **Management authority walks the parent chain.** A caller manages a node if a token of theirs is in the `admin_scope` of that node **or any ancestor** (`managesNode`). *Deciding* on a node — approve, reject, transfer ownership, raise its limit, move it away — requires authority over the parent chain **excluding the node itself** (`managesParentChain`), so nobody approves their own request for more. Root admins are simply the `admin_scope` of the `root` node; there is deliberately no separate root bypass in the tree service.
3. **Requesting.** Creating a child under a budget requires a token in its `eligible_requesters` (or managing it); a requester's child starts `pending`. If the budget carries `auto_approve` and the request is a project, every ancestor must have capacity and — when the policy has a `per_requester_limit` — the owner's cumulative charged usage directly under that budget plus the request must fit it; then the project is approved on the spot, recorded with actor `system:auto-approval`. A policy without `per_requester_limit` is a *pool*: the budget's own capacity is the only bound (`autoApprovable`). With `allow_requests_beyond_auto_approve: false` the policy is a hard limit for requesters: a project or change it does not cover is refused (403) instead of waiting for a manager; managers of the parent chain are not bound by it.

Capacity is enforced on every path that makes consumption active (manager creation, auto-approval, approval, reparenting, promotion): for each ancestor A, `usage(A) + Δ ≤ A.limit`, where `-1` means unlimited. For budgets the edge rule is structural instead: a child budget's limit may not exceed its direct parent's for any resource, and may not be unlimited under a limited parent (`validateChildBudgetLimit`). It is checked on every budget edge that is written — create, approve, direct limit edit, reparent — so it holds inductively. Lowering a budget below its current charged usage is refused.

All capacity check-then-write sections serialize on one process-level mutex (`approvalMu`). That is correct for the single replica the Helm chart runs; several replicas would need row locking in the database instead. Validation that talks to the role provider (group tokens in `authorized_users`) runs *before* the lock is taken.

### 2.2 Node lifecycle

```
             ┌──────────┐  approve   ┌──────────┐  release  ┌──────────┐  project gone   leaf
 request ───▶│ pending  ├───────────▶│ approved ├──────────▶│ released ├───────────────▶ removed
             └────┬─────┘            └───┬──▲───┘  (leaves) └──────────┘  (reconciler)
                  │ reject               │  │
                  ▼                      │  │ approve (apply) /
             ┌──────────┐   request-     │  │ reject (discard change)
             │ rejected │   change       ▼  │
             └──────────┘ (terminal) ┌──────┴────────┐
                                     │ change_pending│
                                     └───────────────┘
 created by a manager of the parent chain, or auto-approved ──▶ approved (no pending step)
 reconciler import ──▶ imported ── promote ──▶ (reconciler tags the project) ──▶ pending
```

Key decisions:

- **No `change_rejected` status.** Rejecting a change discards the proposed `pending` changes and returns the node to `approved`; the previously approved state stays valid. (The history event is still called `change_rejected`.)
- **A pending request is amended in place.** `request-change` on a `pending` node rewrites the request itself (limit, termination date, members, reason) instead of stacking a proposal on it — that is how a manager trims an oversized request rather than rejecting it.
- **Approve may modify.** An approver can pass `modified_limit` to grant something other than what was asked; the history records both values.
- **Budget requests are native.** Eligible requesters may request sub-budgets unless the budget sets `allow_sub_budget_requests: false`; managers are not restricted by that flag.
- **Release is for approved leaves only**, by the owner or a manager of the parent chain. Budgets are deleted instead. `rejected` and `released` are terminal statuses; a released leaf's *record* is removed by the reconciler once its OpenStack project is gone (§8.4).
- `imported` leaves are read-only until promoted.

### 2.3 Roles and fields on a node

| Field | Who | Grants |
|---|---|---|
| `admin_scope` | managers (tokens) | approve/reject children, edit the budget's policy, create children directly; inherited downward. Required on every budget created through the API |
| `eligible_requesters` | consumers (tokens) | may request child nodes here, and read the budget — nothing else |
| `owner` | exactly one `user:` token (leaves) | the responsible person; "my projects" scope; receives the Keystone role `member` in the project |
| `authorized_users` | list of `user:`/`group:` token + OpenStack role | additional members of the OpenStack project; role is `member` or `reader` |

The strict separation of `admin_scope` (manage) and `eligible_requesters` (consume) is the fix for the old model's worst bug. **Delegation** is not a separate concept: delegating capacity means creating a sub-budget with someone else's token in its `admin_scope`.

**Owner** is single by design; it is set from the requester's email on creation, and managers of the parent chain can `transfer-owner`. It matters for the per-requester auto-approve cap (counted per owner token, not per group) and for the email-scoped "mine" view.

**Authorized users are validated because they have consequences in OpenStack**: the reconciler creates a Keystone group per group token and an account per member. So a `user:` token must be a valid email address, a `group:` token must exist according to the role provider (checked fail-closed), the role must be one of `common.OpenstackRoles`, and the list is capped (`API_MAX_AUTHORIZED_USERS`, default 32 — a course belongs in as one group token). `admin` is not offered: OpenStack's default policy treats `admin` as cloud-wide, not project-local.

### 2.4 Editing

Two kinds of change exist, and they are authorized differently ([service_ops.go](internal/tree/service_ops.go)):

- **Direct edits** (`PUT /v1/nodes/{id}`, `UpdateNode`) take effect immediately. On a budget, policy fields (name, `admin_scope`, `eligible_requesters`, `auto_approve`, `allow_sub_budget_requests`) need a manager of the node or above; the limit and termination date need a manager of the *parent* chain, because nobody raises their own budget. A project leaf accepts exactly one direct edit — a rename, by its owner or a manager — because a name is a label, not an allocation. The root's `admin_scope` cannot be edited through the API (it is owned by configuration, §2.5).
- **Change requests** (`POST /v1/nodes/{id}/request-change`, `RequestChange`) are how everything else about a leaf changes, and how a budget's managers ask their parent for more. On an approved node the proposal is stored in `pending` and the node moves to `change_pending`; the approved limit stays in force (and is what the reconciler applies) until a manager of the parent chain approves. On a project leaf some changes need nobody (`leafChangeDecision`): giving resources back, an earlier end and member changes always take effect at once; growth does when the budget's `auto_approve` would grant the difference, and a later end when the budget has `auto_approve` and the date stays within the budget's own. Every part of a proposal has to qualify — otherwise the whole of it waits, so a manager never decides on half a change.

### 2.5 Bootstrap nodes

`Service.Bootstrap` runs on every startup: it seeds mock data into an empty store if asked to (`DB_ADD_MOCK_DATA`), then ensures two structural nodes.

- **`root`** — the single parentless budget. On creation its limit is unlimited for every quantity and granted for every availability. Its `admin_scope` is **synchronized from `ROOT_ADMIN_TOKENS` on every start** — configuration is the source of truth. On every start the root also **adopts resources new to the catalogue** (`adoptNewCatalogueResources`): a resource the root does not hold could never be delegated, because every edge is bounded by its parent. Adoption only adds missing keys; an operator's deliberate cap on the root is left alone.
- **`unassigned`** — the collection point for reconciler imports, directly under root, with an all-zero limit, so nothing can be approved under it; imported leaves must be promoted into a real budget. It has no admin scope of its own; root admins manage it through the ancestor rule. Neither structural node can be moved or deleted, and nothing can be moved into `unassigned`.

### 2.6 Stored and derived node data

Stored with the node (one JSON document per node):

- `pending` — proposed limit, termination date or members while `change_pending`.
- `history` — every lifecycle event (`created`, `approved`, `amended`, `change_requested`, `change_rejected`, `released`, `reparented`, `owner_transferred`, `promote_requested`, `updated`, …) with timestamp, actor, status from/to and the relevant before/after values.
- `flags` — orthogonal markers; currently only `promote_on_reconcile`.
- `termination_date` — intended end of life. Nothing outlives the budget it draws from: a node's end lies no later than the earliest end in its parent chain. A request without one under a budget that ends gets the budget's end, a later one is refused, and a manager cannot remove the end there. When a budget ends earlier — edited, approved as a proposal, or a node moved under an earlier-ending budget — every node below that ends later or never is cut to the new end, waiting requests and proposals included, with an `end_shortened` history entry naming the budget. Extending a budget extends nothing below it. An approval grants no longer than the chain allows, which catches requests older than the rule. **Published, not enforced**: the reconciler writes it as a tag on the OpenStack project (§8.3), but nothing stops a project past its date.
- OpenStack linkage on leaves, maintained by the reconciler: `os_project_id`, `os_project_name` (imports), `os_in_use` (measured usage; a missing key means *not measured*, not zero), `os_overcommitted`, `external_group_assignments` (group role assignments on an imported project that map to no token here; preserved verbatim).

**History carries a channel.** `tree.Actor` is `{Email, Via}` — a struct rather than two string parameters so the two cannot be swapped silently. REST handlers pass `tree.UIActor(email)`; the MCP endpoint passes `Via: "mcp"`. `newHistoryEntry` writes `via: "ui"` for an unset channel rather than leaving it empty, because an absent `via` already means "written before the field existed". The recorded email is the *effective* identity, so under impersonation a change is attributed to the impersonated person (§6.3), identically for REST and MCP.

Attached to responses and never persisted (`attachUsage` and friends): `usage` (budgets), `child_count` (so a client can tell an empty budget from an unloaded one), `parent_name` (one batched query instead of one per node), `available_resources` (§3) and, on `/v1/nodes/my-budgets` only, `ancestor_ids` — root-most first, loaded one tree level per query — so a client can tell which of the budgets it manages are top-most even when the budgets in between belong to someone else.

---

## 3. Resource catalogue

A node's limit is a `map[resource id]int`. What the ids mean is the **resource catalogue**, a list of `common.ManagedProject` entries ([internal/common/projects.go](internal/common/projects.go)). Each entry combines the display definition (name, unit, min/max/default, group heading, `show_on_ui`) with its OpenStack mapping, so adding a resource is a catalogue entry, not a code change.

**Two kinds** decide the arithmetic:

- `count` (the default when `kind` is empty) — a quantity: cores, RAM, storage. Summed across siblings, capped by the parent, charged against budgets. Mapped to OpenStack through `os_quota_field`, optional `os_multiplier` (RAM is stored in GB, Nova wants MB), `os_linked_field` (`instances` mirrors `cores`) and `os_overcommit_check` (measure in-use for this resource). `static: true` marks infrastructure quotas (networks, ports, volumes, …) applied once at project creation from `default`.
- `bool` — an *availability*: a network, an image, a GPU flavour. Held as 0 or 1 and **never summed**: three projects with a network are not three networks. A parent grants it to children rather than dividing it. An availability requires a `grant {type, target}` naming what it means in OpenStack by ID — `network` (Neutron RBAC `access_as_shared`), `image` (Glance image member, accepted on the project's behalf) or `flavor` (Nova flavour access). IDs, not names, because names are neither unique nor stable there.

`hours` is reserved and rejected. The service keeps the ids of the `count` resources in a separate `countIDs` slice, and every sum, capacity check and rollup iterates that slice — the name at the call site is what keeps an availability out of arithmetic.

**Invariants for availabilities** ([service.go](internal/tree/service.go)):

- Only 0 or 1 is accepted. `-1` is refused by name: the edge rule reads `-1` as "no cap", so on an availability it would act as a wildcard for every descendant.
- On budget edges, a child cannot hold an availability its parent does not (`validateChildBudgetLimit` covers all resources).
- A project cannot hold an availability its budget does not (`validateLeafAvailabilities`), checked when it is created, when a change is requested, when it is approved and when an active project moves. The capacity and per-requester checks count quantities only, so without this rule a requester could name any availability — and an auto-approve budget would grant it. The root holds the whole catalogue.
- An availability cannot be **withdrawn** from a budget while any node below it still holds it (`checkAvailabilityWithdrawal`). The edge rule runs when a child is written and cannot see this direction; refusing instead of cascading keeps a silent revocation across a whole subtree from happening.

**Where the catalogue comes from** (`loadResourceCatalogue` in [config.go](internal/config.go)): `RESOURCE_DEFINITIONS` holds a JSON array that **replaces** the built-in set rather than extending it; without it the built-in set applies (cores, RAM, storage, GPUs, plus static network/storage quotas whose defaults come from `RECONCILER_DEFAULT_*`). Either way the catalogue is validated at startup by `ValidateManagedProjects`, and any error stops the process: empty or duplicate ids, unknown kinds, a `bool` without a valid grant, a grant on a quantity, a quota field on an availability, or a half-written mapping (multiplier, linked field or overcommit check without a quota field). The alternative — skipping a bad entry — would produce a resource that is requestable in the portal and governs nothing.

**What clients see.** `/v1/config` returns only entries with `show_on_ui`, and only through an explicit display type (`webserver.uiResource`: id, name, default, min, max, unit, message, kind, group). A new field on `ManagedProject` therefore reaches the browser only when someone adds it there; grant targets and quota mappings stay server-side. Every node in a response carries `available_resources`: the root lists the whole catalogue (it is where everything is delegated from); every other node lists what its own limit carries — a quantity above zero, an unlimited quantity, or a granted availability — in catalogue order. This is presentation only; what may be requested is decided by validation, not by what is shown.

---

## 4. Accounting

What a budget is billed for is decided in one place, `tree.Accounting`, with two switches that only ever make the books stricter when on (both default on):

- **`API_CHARGE_OS_IN_USE`** — a leaf costs `max(limit, measured OpenStack usage)` per resource (`chargedQuota`). OpenStack accepts a quota reduction below current usage and keeps the servers running, so without this a project filled and then shrunk would free budget on paper while occupying the hardware. The maximum is taken **per leaf, before summing** (max does not distribute over addition); a resource missing from `os_in_use` means "not measured" and leaves the limit untouched; an unlimited limit stays unlimited. Only resources with `os_overcommit_check` are measured (cores, RAM and storage in the built-in set).
- **`API_CHARGE_RELEASED`** — a released leaf stays charged. Releasing only hands the deletion to OpenStack (§8.4), so until the project is actually gone the capacity is still occupied; not charging it would let the same hardware be booked twice. It ends by itself: the reconciler removes the released record once the project has disappeared from its scope.

`chargedStatuses()` is the single answer to "which leaf states count" — `approved` and `change_pending`, plus `released` when the second switch is on. The rollup, the per-requester auto-approve cap, the capacity checks and reparenting all use it, so a leaf cannot be billed in one view and move for free in another. The switches exist so a bad measurement (for instance a reconciler bug reporting inflated usage) can be turned off without a release.

---

## 5. Package layout

```
cmd/main.go                  → app.RunApplication
internal/
  app.go, config.go          package app: env configuration, catalogue loading, wiring of
                             stores, role provider, auth middleware, reconciler, router
  reconciler_supervisor.go   retries the OpenStack connection in the background (§8.1)
  common/                    shared types: ProjectQuota (-1 = unlimited), TokenList/TokenSet,
                             AuthorizedUser, RoleProvider interface, sentinel errors,
                             ManagedProject + catalogue validation, OpenStack roles
  tree/                      THE domain: Node model, Service (operations, authorization,
                             accounting), Store interface, memory + Postgres stores,
                             one-time drop of the pre-tree tables
  identity/                  role switch, impersonation, principal search — model-agnostic,
                             embedded into tree.Service so handlers see ONE service
  webserver/                 Gin router: auth middlewares, /v1 node, token, role-switch,
                             principal-search and reconciler-admin handlers, /mcp, static
  roleprovider/              RoleProvider implementations: mock (from mockdata) and http
                             (api/: Go client generated from role-provider-service's spec)
  reconciler/                two-way OpenStack sync (§8) and the catalogue ↔ quota mapper
  openstack/client/          gophercloud client: projects, quotas, members, groups,
                             federated users, grants, the project-scoped second client
  mockdata/                  development seed: five identities and a small university tree
  generated_docs/            swagger.json + VERSION, embedded via go:embed (make bundle)
  helper/                    embedded landing page
```

Code shared with the sibling services lives in `github.com/pfisterer/cloud-self-service-golib`: `authn` (claims, `Claims.Identity()`, bearer parsing), `ginweb` (CORS, cache headers, the read-only flag), `envconf`, `logging`, `redact` (secrets in the logged configuration), `token` / `tokengorm` (API tokens) and `mcpserve` (MCP transport and tool gating).

Layering: `webserver → tree.Service (embeds identity.Service) → tree.Store`. Handlers never touch the store; all authorization lives in the service. The reconciler is the one other writer: it consumes a structural subset of the store (`reconciler.ReconcilerStore`: `ListNodes`, `UpsertNode`, `DeleteNodes`, `UpdateNode`, `DeleteNodeIf`) and does not go through the service. A pass loads its nodes once and then spends seconds per leaf in OpenStack, so every write to a node that already existed goes through `UpdateNode` (apply only the reconciler's own fields to the node as it is *now*; in PostgreSQL under a row lock) and every delete through `DeleteNodeIf` (delete only if the node is still what the pass assumed, e.g. still `imported`). Writing the early copy back would undo a change request, an approval or a release made during the pass.

---

## 6. Authentication and identity

### 6.1 Authentication

Every `/v1` and `/mcp` request passes the configured auth middleware ([authentication.go](internal/webserver/authentication.go)):

- **OIDC bearer tokens** are verified against `OIDC_ISSUER_URL` with `OIDC_CLIENT_ID` as audience. `OIDC_ISSUER_URL`, `OIDC_CLIENT_ID` and `OPENSTACK_AUTH_URL` are validated at startup even in setups that do not use them.
- **API tokens** are recognised by their `os_mgt_` prefix and looked up through the shared `token.Service` (only a SHA-256 hash of the secret is stored). The token's subject becomes the email claim; its read-only flag is recorded on the request.
- **Dummy auth** (`API_DUMMY_AUTH=true`, refused at startup unless `API_MODE=development`) takes the email from `X-Dummy-Auth-User` and the tokens straight from `mockdata`. ⚠️ An email that is not a mock identity — or no header at all — falls back to the **root** mock identity's tokens, so any dev email can drive the full API; surprising privileges in development are usually this fallback.

For OIDC and API tokens the caller's tokens come from `RoleProvider.GetUserTokens`, **fresh on every request**. Authorization never compares emails, only **tokens**: `user:<email>` and `group:<name>`. A caller's identity is a `TokenList`, and every scope field on a node is a `TokenList`. The caller's email is `Claims.Identity()` (email, then preferred_username, then sub).

### 6.2 Role providers

`common.RoleProvider` has four methods: `GetUserTokens`, `SearchGroups`, `SearchUsers` (by address only, never by name) and `GetGroupUsers`. Two implementations:

- **`mock`** — derived from the mockdata identities; no external dependency. Logged as a warning because it hands out fake group memberships.
- **`http`** — calls role-provider-service with a bearer token (`ROLE_PROVIDER_URL`, `ROLE_PROVIDER_API_TOKEN`) through a client generated from that service's released OpenAPI spec (§10). If the token lookup fails, the caller gets only their own `user:` token — they lose group rights for that request rather than the request failing. Search errors are returned as errors: principal search then degrades to what it could still find, while the `authorized_users` group check fails closed.

Any other `ROLE_PROVIDER` value stops the service at startup rather than silently falling back to the mock.

### 6.3 Effective identity and the role switch

`EffectiveAuthMiddleware` resolves identity once per request into an `AuthContext`, and handlers read only that:

| Field | Meaning |
|---|---|
| `ActorEmail`, `OriginalTokens` | the real, authenticated caller |
| `UserEmail`, `EffectiveTokens` | who the caller is acting as — used for all scoping and authorization |

The role switch (`/v1/role-switch`, [identity/service.go](internal/identity/service.go)) is gated on the **original** tokens holding one of `ROOT_ADMIN_TOKENS` (any token type). One override slot per actor, two modes:

| Mode | Request | Effective tokens | Effective email |
|---|---|---|---|
| **Group override** | `PUT {group_token}` | own non-group tokens + that one group (keeps `user:` and a `user:`-based root grant) | unchanged |
| **Impersonation** | `PUT {impersonate_user}` | **replaced** by the target's tokens from the role provider — the actor's own root grant is gone | target's email |

There is no whitelist of assumable identities: an unknown address yields whatever the role provider answers for it (with the http provider, at least that person's `user:` token), which is the only way to see what someone covered solely by a pattern rule sees. If resolution fails or returns nothing, the actor's original tokens are used. Overrides live in process memory (a copy-on-write map), so they end with a restart.

Two deliberate consequences:

- **Root-admin surfaces follow the effective identity.** The reconciler admin routes and `eligible-for-owner` check effective tokens, so while impersonating a non-root user those actions are denied — what the actor can do always matches the identity on screen.
- **API tokens belong to the actor, never to the effective identity.** `/v1/tokens` lists, issues and revokes under `ActorEmail`: a temporary view must not turn into a permanent credential.

⚠️ Dev gotcha: dummy auth resolves tokens from `mockdata`, while impersonation resolves them through the configured role provider. With dummy auth but `ROLE_PROVIDER=http`, the same email can therefore carry different groups when logged in directly and when impersonated.

### 6.4 API tokens and read-only

Tokens are issued through `/v1/tokens` with a description (at most 100 characters), a lifetime resolved by the shared `token.TTLPolicy` (`API_TOKEN_TTL_HOURS` when the request names none; `-1` = never expires, only with `API_TOKEN_ALLOW_NEVER_EXPIRES`) and an optional read-only flag. The secret is returned once. Revoking an unknown id and revoking someone else's token give the same 404. The store follows `DB_TYPE`: in memory tokens die with the process; with Postgres they use the same connection pool as the tree (§9).

Read-only is enforced where the operation's nature is known: on `/v1`, `ginweb.RejectWritesForReadOnlyTokens` refuses every non-GET request from a read-only token; on `/mcp` every call is a POST, so the check moves to the tools (§7.1).

---

## 7. HTTP API surface

All under `/v1`, JSON, authenticated as above. Swagger annotations on the handlers generate the OpenAPI description served at `/swagger.json`, which is the reference; this table is the map. Listings are paginated (`limit` default 100, max 500; `offset`) and return a `NodePage {items, total, limit, offset}` — `total` is counted by a separate query so a full page never looks complete.

| Endpoint | Purpose |
|---|---|
| `GET /v1/config` | UI-visible catalogue entries, allowed OpenStack roles, `provisioningEnabled`, dummy dev users (dev only) |
| `GET /v1/nodes/{id}` | one node; readable by its owner, authorized users, eligible requesters and managers of its chain |
| `GET /v1/nodes/{id}/children` | direct children of a budget; managers only |
| `GET /v1/nodes/mine` | leaves owned by the effective email |
| `GET /v1/nodes/my-budgets` | budgets whose **own** `admin_scope` matches a caller token, with `ancestor_ids` |
| `GET /v1/nodes/to-manage` | `pending`, `change_pending` and `imported` nodes awaiting the caller: with `scope=direct` (default) under the administered budgets and their undelegated sub-budgets, with `scope=subtree` anywhere below |
| `GET /v1/nodes/eligible-for-me` · `GET /v1/nodes/eligible-for-owner?owner_token=…` | approved budgets accepting requests from the caller / from given tokens (root admins; used when promoting) |
| `GET /v1/nodes/search?q=…` | full-text search below the administered budgets (name, reason, id, owner, creator, status, OpenStack project, tokens); flat list, only the page is decorated |
| `POST /v1/nodes` | create a budget or project: managers create it approved, eligible requesters create a request (possibly auto-approved) |
| `PUT /v1/nodes/{id}` | direct edit (§2.4) |
| `POST /v1/nodes/{id}/request-change` | propose a change, or amend a pending request |
| `POST /v1/nodes/{id}/approve` (opt. `modified_limit`) · `reject` · `release` | lifecycle decisions |
| `POST /v1/nodes/{id}/reparent` | move a node; needs authority over its current parent chain **and** the new parent, a cycle-free target, and capacity in the new chain for charged nodes |
| `POST /v1/nodes/{id}/transfer-owner` | hand a leaf to another person; parent-chain managers |
| `POST /v1/nodes/{id}/promote` | move an imported leaf into a real budget with an owner and flag it for the reconciler (§8.5) |
| `DELETE /v1/nodes/{id}` | delete a budget subtree; refused while anything below is pending, awaiting a change decision, imported, or an approved leaf. Leaves are never deleted directly |
| `GET/PUT/DELETE /v1/role-switch` | role switch state, set, clear (§6.3) |
| `GET /v1/principals/search?q=…` | what a token field can be filled with: groups (by token, label or description) and users (by address only, only for a non-empty query; role-provider results plus everyone already owning or participating in a leaf) |
| `GET/POST /v1/tokens` · `DELETE /v1/tokens/{id}` | API tokens of the actor |
| `GET /v1/admin/reconcile/status` · `POST /v1/admin/reconcile/trigger` | reconciler status and manual run; root admins; 503 while disabled or still connecting |
| `/mcp` | Model Context Protocol endpoint (§7.1) |
| `GET /` · `GET /swagger.json` · `GET /config.json` | unauthenticated: landing page, OpenAPI description, version + OIDC settings for a frontend; served with caching disabled |

Errors from the service are sentinel-wrapped and mapped centrally in [errors.go](internal/webserver/errors.go): `ErrForbidden` → 403, `ErrNotFound` → 404, `ErrConflict` → 409 (the node already moved to a status where the operation no longer applies — the client's view is stale), everything else → 400. CORS on `/v1` allows only `CORS_ALLOWED_ORIGINS` (plus loopback origins in development mode).

### 7.1 MCP endpoint

`/mcp` ([mcp.go](internal/webserver/mcp.go)) serves MCP over streamable HTTP inside this process, mounted behind the same auth middleware and `EffectiveAuthMiddleware` as `/v1`. That one mount is why it is not a separate server: token resolution, fresh group rights, the role-switch rules and the actor/effective split all come for free.

- **No authorization of its own.** Every tool calls the same `tree.Service` method the REST handler calls, with the effective tokens, and passes `tree.Actor{Email: effective email, Via: "mcp"}` so the history records the channel.
- **A server per request**, built around the caller (`mcpserve.Handler`), so a tool closure can never run with another request's rights. A request without a resolved caller is refused.
- **Read-only by omission.** Tools are registered through `mcpserve.AddTool` with a `mutates` flag; for a read-only token the mutating tools are simply not offered.
- **Tools**: reads (`list_my_projects`, `list_my_budgets`, `get_project`, `search_projects`), reversible writes (`create_project`, `request_project_change`, `approve_request`, `reject_request`, `rename_project`), structural writes (`create_budget`, `move_to_budget`, `transfer_ownership`, `adopt_imported_project`) and two irreversible ones (`release_project`, `delete_budget`) that additionally require echoing the node's exact current name in `confirm_name`. That echo catches a model that resolved "the old one" to the wrong id; it is not a defence against prompt injection.
- **Own payload types.** Tool inputs and outputs are hand-written structs rather than `tree.Node` and the request types, to keep policy fields and history out of a model's context. `mcp_input_drift_test.go` holds each input against the request type it feeds, so a new request field fails the build until someone decides whether the tool offers it.

---

## 8. Reconciler (two-way OpenStack sync)

[internal/reconciler/reconciler.go](internal/reconciler/reconciler.go). Optional (`RECONCILER_ENABLED`), runs every `RECONCILER_INTERVAL_SECONDS` and on demand.

### 8.1 Lifecycle and connection

`app.go` does not connect to OpenStack in the startup path. A `reconcilerSupervisor` is handed to the webserver immediately and retries building the OpenStack client (which authenticates) in the background with exponential backoff (15 s doubling to 5 min, ±10 % jitter). Until that succeeds `Ready()` is false: `/v1/config` reports `provisioningEnabled: false` and the admin routes answer 503 "still connecting" instead of presenting a configured cloud as disabled. Once connected, the reconciler runs one pass immediately, then on every tick or trigger; the trigger channel holds one signal, so repeated triggers coalesce. `GET /v1/admin/reconcile/status` returns the counters of the last run, its error, the pre-seeding conflicts (§8.3) and the configured termination-tag prefix.

A pass aborts only when it cannot load its inputs (leaves, the scope parent, the project listing). Everything per project, group or user is logged and skipped, and the next pass retries — the desired state lives in the tree, not in the call.

### 8.2 Phases of one pass

1. **Load.** Reconcilable leaves (`approved`, `change_pending`), all leaves in real statuses, imported leaves; resolve the **scope parent** (`RECONCILER_SCOPE_PARENT_ID`, or `_NAME` looked up and created if missing — without managed tags, so it can never look like a managed leaf; in dry run a missing parent is not created and the pass runs unscoped); build the project-scoped client if needed (§8.6); list OpenStack projects — every project under the scope parent, or without a scope parent only projects carrying the managed tag.
2. **Lookup maps** by resource-id tag and by OpenStack project id.
3. **Promote** imported leaves flagged `promote_on_reconcile` (§8.5).
4. **Groups.** For every `group:` token in the reconcilable leaves' `authorized_users`, ensure a Keystone group named `RECONCILER_GROUP_PREFIX` + group name (groups have no parent and no tags, so the prefix marks them), and sync its members from `RoleProvider.GetGroupUsers`, creating accounts as needed.
5. **Tree → OpenStack** for each reconcilable leaf (§8.3).
6. **OpenStack → tree**: import unknown projects, handle released ones, drop records whose project is gone (§8.4, §8.5).
7. **Prune** Keystone users this service created (recognised by a fixed description) that no longer hold any project role assignment. A user with any assignment, including one made by hand, is never deleted.

### 8.3 Tree → OpenStack

**Identification is by tag, never by name.** A project created for a leaf carries the managed tag and `<resource-id prefix><node id>`. Its name is `<node name> [<short node id>]` (e.g. `Cloud Computing [p_7ad31c42]`, the leaf's reason standing in for a missing name), sanitized (non-BMP and control characters dropped, whitespace collapsed) and truncated to Keystone's 64 characters. The id suffix is required for correctness: Keystone enforces name uniqueness per *domain*, not per parent. Nothing parses the name back, so renaming a node is always safe; name and description (`<owner>: <reason> (managed project)`) are re-synced on every pass, the name only when it changed.

**Recovering a lost tag.** Project admins can remove tags in OpenStack. Before creating a second project for a leaf whose tag is missing, `recoverUntaggedProject` looks the project up by the leaf's stored `os_project_id`, re-tags it, and removes an imported leaf that may already shadow it (kept under `NoDelete`). It refuses when the project is tagged for another node or already claimed by another leaf in the same pass.

**New project.** Created under the scope parent; then the full quota set — the leaf's mapped quantities plus the `static` defaults — is written, retried up to four times because compute and block storage may not know a brand-new project for a few seconds. On failure the project is kept and its id stored; the next pass syncs the quota. Members and group assignments are set in the same pass; grants and the lifecycle tags follow on the next pass, when the project is found by its tag.

**Existing project**, every pass:

- **Quota**: only the managed fields — Nova `cores`, `ram`, `instances` and Cinder `gigabytes` — are written, from the *approved* limit (`change_pending` keeps the current one). Static quotas set at creation are not touched again. These four fields are always sent, so a catalogue that maps none of its resources to one of them writes 0 there. Mapping goes through the catalogue ([mapper.go](internal/reconciler/mapper.go)): multiplier, linked field, and a fixed table of known quota fields.
- **Measured usage**: the quota detail (Nova detail, Cinder usage) yields `os_in_use` for resources with `os_overcommit_check`, and `os_overcommitted` when in-use exceeds the granted limit. OpenStack accepts such a reduction and only refuses new resources; the flag surfaces it. The measurement is only stored when the read succeeded (`applyOSSyncState`) — a failed read must not overwrite the last known usage with "nothing", or the accounting (§4) would fall back to the limit.
- **Tags**: `termination:<RFC3339>` and `status:<status>` are brought to their desired values in one rebuild (`applyPrefixedTags`) and written only when something changed, because this runs for every leaf on every tick. An empty prefix switches a tag off.
- **Members**: the owner gets `member` (and the project becomes their default project if they have none); `user:` entries in `authorized_users` get their role, clamped to the currently allowed roles so a stored `admin` from an older release is not re-granted. Real users (names containing `@`) not in the desired set lose their direct assignments; service accounts are never touched. `group:` entries become group role assignments; `external_group_assignments` are preserved.
- **Grants**: for every availability in the catalogue, grant or revoke according to the leaf's value. **Only targets named in the catalogue are ever touched** — flavour access and image members carry no marker of who created them, so a grant made by hand on anything else is left alone. Changes are logged only when they happened; a dry run reads and logs only differences.

**Pre-creating accounts.** A role needs an existing account. `FindOrCreateUser` creates a passwordless local user, or with `OPENSTACK_FEDERATED_PROVISIONING` a federated one carrying the link `(idp_id, protocol_id, unique_id)` that a login resolves by ([federated.go](internal/openstack/client/federated.go)). Resolution order: if the account a login would create (its id is derivable from domain and unique_id) exists, it wins and a stand-in of ours is removed; a stand-in of ours with the right link is reused (re-read by GET, because Keystone's list omits federated attributes); an account with that name that is neither is reported as a **pre-seeding conflict** in the status instead of guessed at; otherwise a stand-in is created (Keystone assigns its id and ignores a supplied one).

### 8.4 Released projects

For a released leaf whose project is still in scope: with `RECONCILER_DELETE_RELEASED_PROJECTS` (and not `NoDelete`) the project is deleted; otherwise it is tagged `pending-deletion:<today + grace days>`, `contact:<owner email>` and `status:released`, handing the deletion to whoever acts on those tags. The date is written once and never pushed out again on later passes.

A released leaf whose project is **no longer in scope** is removed from the tree (`removeReleasedLeavesWithoutProject`). Out of scope counts as gone: the scope is what the reconciler is responsible for. This is safe because a failed project listing aborts the pass before this point, so "not listed" never means "listing failed"; a project still in scope under the stored id but without its tag is not treated as gone. That removal is what ends the charge of a released leaf (§4).

### 8.5 OpenStack → tree

A project in scope is **imported** when it has no resource-id tag, or a tag pointing at a node that no longer exists. Tagged projects whose leaf exists in any real status are left alone, so a pending or rejected leaf's project is not re-imported. An import becomes an `imported` leaf under `unassigned`, keyed stably by its OpenStack project id, with the quota translated back through the catalogue as its limit, every user member whose email can be resolved as an authorized user with their actual role, and group assignments as `group:` tokens where the Keystone group can be resolved, kept as external assignments otherwise. Imported leaves whose project has left the scope are removed.

**Promotion** is two-step. The API (`PromoteNode`, root admins plus a manager of the target) reparents the imported leaf, sets owner, reason, members and optionally a new limit, checks capacity early and sets `promote_on_reconcile`. The next pass tags the existing OpenStack project for the node, sets the leaf to `pending` and removes the flag; from there the normal approval cycle applies.

### 8.6 The OpenStack client and its two scopes

[internal/openstack/client](internal/openstack/client) wraps gophercloud with one `OpenStackClient` holding Identity, Compute, Network, Block Storage and Image service clients (block storage is found under either catalogue type, `volumev3` or `block-storage`). Two authentication methods, exactly one configured; application credentials win when both are complete:

- **Application credential** — always project-scoped by Keystone; no scope may be passed.
- **Service user with password** — can request system, domain or project scope (in that precedence). This exists because clouds enforcing the modern RBAC scope defaults require more than a project scope to create projects and assign roles across them.

Those clouds split the work: Keystone wants the **domain** scope for creating projects, while Nova, Neutron and Cinder accept quota and grant calls only from a **project**-scoped token. So a client built with password auth and domain scope keeps its auth recipe, and at the start of each pass the reconciler calls `EnsureProjectScope(scope parent)`. The first successful call authenticates a second, project-scoped provider against the scope parent — the one project that outlives every managed one — and atomically swaps the compute, network and block-storage clients over to it; later calls are no-ops. Keystone and Glance stay on the primary client. With application credentials, system scope, project scope or no scope parent this never happens and the primary clients serve everything. If building it fails, the pass continues and the refused quota calls show up in the log.

### 8.7 Safety modes

- **`RECONCILER_DRY_RUN`** — no writes to OpenStack or the store; creation, deletion, tagging and grants are logged (grants only where they differ).
- **`RECONCILER_NO_DELETE`** — no destructive operations *in OpenStack*: released projects are only tagged, orphaned users are flagged via their description instead of deleted, and member, group-member and group-assignment removals are skipped. Records of things OpenStack no longer has (stale imports, released leaves whose project is gone) are still removed — keeping them would make the tree disagree with the cloud.

---

## 9. Storage

`tree.Store` ([store.go](internal/tree/store.go)) is a small interface: get, list and count by `NodeQuery`, upsert, delete, count children per parent, plus seeding and participant listing. `NodeQuery` fields combine with AND, values within a field with OR (ids, parent ids, kinds, statuses, owner, `AdminScopeAny`, `EligibleAny`). `ListNodes` and `CountNodes` share the same query translation, so a page and its total cannot disagree.

- **memory** ([memory.go](internal/tree/memory.go)) — development and tests; returns copies so callers cannot mutate stored state.
- **postgres** ([postgres.go](internal/tree/postgres.go)) — table `nodes`: indexed columns `id`, `parent_id`, `kind`, `status`, `owner`, the token lists `admin_scope` and `eligible_requesters` as JSONB (queried with `@>` containment), and the full node as JSONB `data`. Response-only fields are cleared before writing. Table `identities` holds seeded mock identities. GORM `AutoMigrate` creates both; because AutoMigrate never drops anything, `dropLegacyTables` removes the three tables of the pre-tree model (`delegations`, `projects`, `eligibility_rules`) at startup if present, logging their row counts first — an explicit list, so it can never touch a table still in use. The pool is capped at 10 open / 5 idle connections, since `database/sql` defaults to unlimited and the database may be shared. API tokens (`tokengorm`) use the same `*gorm.DB`.

Mock seed (`DB_ADD_MOCK_DATA=true`, only into an empty store): five identities (root admin, CS admin, CS faculty, biology faculty, CS student) and a small university — root, `unassigned`, a CS department with a faculty pool and a student budget with `auto_approve`, a biology department, a pending sub-budget request, leaves in `approved`, `pending` and `change_pending`, and one `imported` leaf with an external group assignment.

---

## 10. Generated clients

```
swag annotations ─▶ swagger.json (OpenAPI 2) ─▶ embedded (generated_docs, go:embed) ─▶ GET /swagger.json
  (make bundle)          │
                         └─▶ swagger2openapi ─▶ @hey-api/openapi-ts ─▶ esbuild + tsc ─▶ npm package
                                                     (make npm-package / make npm-publish)
```

`make bundle` generates `swagger.json` and the embed file (plus `VERSION`); a fresh clone does not compile without it. The TypeScript client is no longer embedded or served: it is published as an npm package (`@dhbw-cloud/os-mgt-client`, prereleases on the `next` dist-tag) so consumers depend on a version at build time and a missing operation is a build error there rather than a silent no-op in a browser with a stale cached client.

The **Go client for role-provider-service** (`internal/roleprovider/api`) is generated with a pinned `oapi-codegen` from the `swagger.json` attached to the role-provider-service release named by `RP_VERSION` in the Makefile, not from a sibling checkout; `RP_SWAGGER_URL` can point at an unreleased spec while an API change is in flight. The spec and the generated client are committed, and CI checks both against the pinned release.

---

## 11. Testing

- **`webserver/api_scenario_test.go` is the acceptance oracle:** it builds a multi-level tree through the real HTTP API as five different mock identities (including a budget request), drives the full lifecycle and asserts the usage rollup on every level. Regressions in authorization or capacity almost always surface here.
- Unit and focused tests sit next to the code. In `tree/`: service operations, availabilities, charged quotas, released accounting, actor/channel, ancestor ids, authorized-user validation, the legacy table drop. In `reconciler/`: grants, in-use measurement, released leaves, tag recovery, scope parent, project names, prefixed tags, desired members. In `openstack/client/`: federated pre-seeding and conflicts, the project-scope split. In `webserver/`: node handlers, role switch (including eligibility under impersonation), CORS, the config endpoint, read-only tokens, MCP and the MCP input drift check. In `internal/` (package `app`): catalogue loading and validation, the built-in catalogue's measurement flags, the supervisor's retry loop.
- Test helpers ([testhelpers_test.go](internal/webserver/testhelpers_test.go)): `setupRouter`, `setupRouterSeeded`, `routerFromStore` (memory store, dummy auth, chosen accounting), `do`, `mustDecode`, `decodePage`.
- CI (`Checks`) runs `make bundle`, `go vet`, `make test` and the role-provider spec/client checks. The image build deliberately runs no tests (`make image`).

---

## 12. Development and deployment

**Locally**, the defaults need nothing external: in-memory store, mock role provider, reconciler off. `make dev` runs the server with live reload under `API_MODE=development`; add `API_DUMMY_AUTH=true` and `DB_ADD_MOCK_DATA=true` for a populated tree driven by `X-Dummy-Auth-User`. Configuration is environment-only (see the README table and [config.go](internal/config.go)); a `.env` in the working directory **overrides** already-set variables, which also decides what a local test run sees.

**Deployment** is a Helm chart in `helm-chart/` (with `values.schema.json`), published as an OCI artifact and normally composed with the other services by the umbrella chart described in the README. It runs one replica, which the in-process `approvalMu` and the in-memory role-switch overrides rely on. Postgres is expected in production; the tree needs only its `nodes` and `identities` tables plus the token table. A cautious reconciler rollout goes from `RECONCILER_DRY_RUN=true` to `RECONCILER_NO_DELETE=true` to full operation.

---

## 13. Known gaps and follow-ups

- **`termination_date` is not enforced** — published as a tag, but nothing expires a project.
- **Single replica assumed**: capacity serialization (`approvalMu`) and role-switch overrides are process-local.
- **Dummy-auth root fallback** for unknown emails (§6.1) is convenient but surprising.
- **Dummy auth and impersonation can disagree** about an email's groups when `ROLE_PROVIDER=http` (§6.3).
