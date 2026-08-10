# OpenStack Management API

## Why

An OpenStack cloud can be handed out in exactly two unsatisfying ways. Either a
central team creates every project by hand — then it is the bottleneck, and the
answer to "who has what, paid from whose share?" lives in tickets. Or everyone
gets admin rights — then nothing is bounded, and the first over-provisioned
project is discovered when the cloud is full.

A fixed default quota does not resolve it either. Whatever size it has, it fits
almost nobody: too small for a course with two dozen students, far too generous for
a demo that runs for an afternoon. Every request that leaves the default behind —
which is most of them — becomes a ticket again, and the wait comes back with it.

What is missing between the two is *delegated capacity*: a department gets a share
it can pass on, a lecturer can hand part of theirs to a course, and a student can
ask their lecturer instead of the data centre. Each step should be a decision by
someone who actually owns the resources — and who can judge whether the request is
reasonable, which an administrator reading a ticket usually cannot — recorded where
the next person can see it, and ending in a real OpenStack project without anyone
clicking it together.

Some requests should not need a decision at all. A budget can carry an
auto-approval cap per requester, so small allocations are granted the moment they
are asked for and only what exceeds the cap reaches a human. That is what keeps
delegation from turning into a queue one level further down.

This service is that middle layer. It owns the budget tree, the request and
approval cycle, and the reconciliation into OpenStack.

It is an API and usable on its own — everything below is reachable over HTTP, and
it ships a generated TypeScript client. If you would rather not build a frontend,
[**self-service-ui**](https://github.com/pfisterer/self-service-ui) is one: a web
interface covering the whole flow (requesting, approving, delegating, releasing,
plus the DNS half of the platform). Its README has screenshots of what that looks
like.

## What it does

The whole domain is **one tree of nodes** (`internal/tree`):

- **Budget** — an inner node: a delegated pool of capacity. Its `admin_scope`
  tokens manage it (approve or reject children, edit it, delegate further); its
  `eligible_requesters` tokens may request child nodes under it. Delegation *is*
  creating a sub-budget with someone else in `admin_scope` — there is no separate
  concept for it. A budget may carry an `auto_approve.per_requester_limit`:
  requests within that per-person cap are granted automatically.
- **Project** — a leaf: a concrete allocation with exactly one `owner`.
  Lifecycle `pending → approved → released`, plus `change_pending` while a change
  is proposed (rejecting a change returns the node to `approved`). Budgets go
  through the same cycle.
- **Usage rollup** — only active leaves consume capacity, aggregated live over
  every subtree. Approving anything checks capacity along the whole ancestor
  chain, not just the direct parent.
- **Authorization walks the parent chain** — deciding on a node requires a token
  in an *ancestor's* `admin_scope`, so nobody approves their own request. Root
  admins are simply the `admin_scope` of the `root` node, synchronised from
  `ROOT_ADMIN_TOKENS` at startup.
- **Role switch** — a root admin may act within a single group, or fully
  impersonate another identity, to see the platform as that person sees it.
- **Role provider** — pluggable source of a caller's group tokens and of group
  search: `mock` (built-in test identities, no external dependency) or `http`
  (the external [role-provider-service](../role-provider-service)).

Editing follows the same idea. A project leaf accepts exactly one direct edit — a
rename, because a name is a label, not an allocation. Everything else goes
through a change request, which for a *pending* node is amended in place: that is
how a manager trims an over-sized request instead of rejecting it.

➡️ **Architecture:** [`ARCHITECTURE.md`](ARCHITECTURE.md) — domain model,
authorization rules, API surface, storage, reconciler, SDK pipeline.

## The reconciler

An optional background loop (`RECONCILER_ENABLED`) that makes the tree true in
OpenStack: it creates a project per approved leaf, keeps name, description,
quota and members in sync, imports unknown OpenStack projects as `imported`
leaves so they can be adopted, and handles released ones.

It marks what it owns with Keystone tags, and those tags are the contract with
anyone holding only OpenStack credentials:

| Tag | Written when | Purpose |
|---|---|---|
| `managed` | on create | this project belongs to the platform |
| `managed-resource-id:<node>` | on create | which node it belongs to |
| `termination:<RFC3339>` | when the node's termination date changes | read "what runs out when" straight from OpenStack, no access to this API needed |
| `pending-deletion:<date>` | when a leaf is released | scheduled deletion day (grace period) |
| `contact:<email>` | when a leaf is released | who to ask before it goes |

Tag writes happen only when a value actually changed — this loop runs every
interval for every leaf, and an unconditional update would be one Keystone write
per project per tick for a value that changes twice in a project's lifetime.

**Pre-seeding federated users.** A role can only be assigned to an account that
exists, and Keystone creates a federated account on the user's first login. So
the reconciler pre-creates one, carrying the federation link
`(idp_id, protocol_id, unique_id)` — and that link, not the user ID, is what a
login resolves by. Two facts about this are easy to get wrong and were verified
against a live cloud:

- Keystone assigns the ID itself on `POST /v3/users` and **ignores** one supplied
  in the request, so a pre-created account can never carry the ID that a
  login-created shadow user would have. That difference is not an error.
- Keystone's user **list** omits federated attributes; only a single-user **GET**
  returns them. Judging an account's link on a list payload declares every
  pre-created account link-less.

If a login nevertheless creates its own shadow account, the next pass notices,
moves the role there and removes the stand-in. Accounts the platform did not
create are never touched or reused; those are reported as pre-seeding conflicts
in the reconciler status instead of resolved by guessing.

## Quick start

```bash
make dev            # live-reload dev server on :8083
                    # (in-memory store, mock identities, dummy auth)
make test           # go test -cover ./...
make all            # tests + bundled docs/SDK + binary
docker build -t openstack-management-api .
```

## API

Everything is served under `/v1`, and the service publishes its own OpenAPI
description — that spec is the reference, so it cannot drift from the
implementation the way a hand-written endpoint list does:

- **`GET /swagger.json`** — the OpenAPI spec
- A generated TypeScript client is published to npm as `@dhbw-cloud/os-mgt-client`.
  It used to be served from `/client` and loaded by the browser at startup; consumers
  now depend on a version at build time, so a missing operation is a build error there
  instead of a silent no-op in the browser.

[self-service-ui](https://github.com/pfisterer/self-service-ui) renders the same
spec in the browser under *Cloud Projects → API Documentation*, which is usually
the quickest way to look something up and try it out. For the reasoning behind the
endpoints — the domain model, the authorization rules, the storage layout — see
[`ARCHITECTURE.md`](ARCHITECTURE.md).

## Configuration

Everything is environment variables; a `.env` in the working directory is loaded
automatically and **overrides already-set variables** (`godotenv.Overload`), which
matters when a sourced `openrc` is in the same shell.

| Variable | Default | Purpose |
|---|---|---|
| `API_MODE` | `production` | `development` enables verbose mode and permits dummy auth |
| `API_BIND` | `:8083` | Listen address |
| `API_DUMMY_AUTH` | `false` | Dev-only bypass via `X-Dummy-Auth-User`; the service **refuses to start** if combined with `API_MODE=production` |
| `DB_TYPE` | `memory` | `memory` \| `postgres` |
| `DB_CONNECTION_STRING` | — | DSN for `postgres` |
| `DB_ADD_MOCK_DATA` | `false` | Seed the mock budget tree (only into an empty store) |
| `ROLE_PROVIDER` | `mock` | `mock` \| `http` |
| `ROLE_PROVIDER_URL`, `ROLE_PROVIDER_API_TOKEN` | — | Required for `http` |
| `ROOT_ADMIN_TOKENS` | — | Comma-separated `user:`/`group:` tokens that become root admins |
| `OIDC_ISSUER_URL`, `OIDC_CLIENT_ID` | — | Bearer-token verification |
| `API_MAX_AUTHORIZED_USERS` | — | Cap on additional members per project |
| `RECONCILER_ENABLED` | `false` | Turn the OpenStack loop on |
| `RECONCILER_INTERVAL_SECONDS` | `60` | How often it runs |
| `RECONCILER_DRY_RUN` | — | Log what it would do, write nothing |
| `RECONCILER_NO_DELETE` | — | Never delete in OpenStack |
| `RECONCILER_SCOPE_PARENT_NAME` / `_ID` | — | Confine it to one parent project; **never** point this at the domain root |
| `RECONCILER_TERMINATION_TAG_PREFIX` | `termination:` | Empty disables the tag |
| `RECONCILER_PENDING_DELETION_TAG_PREFIX`, `_GRACE_DAYS`, `RECONCILER_CONTACT_TAG_PREFIX` | `pending-deletion:`, `30`, `contact:` | Release handling |
| `OPENSTACK_AUTH_URL`, `OPENSTACK_APPLICATION_CREDENTIAL_ID`, `OPENSTACK_APPLICATION_CREDENTIAL_SECRET` | — | Credentials; each falls back to its `OS_*` equivalent |
| `OPENSTACK_FEDERATED_PROVISIONING`, `OPENSTACK_FEDERATED_IDP_ID`, `_PROTOCOL_ID`, `_DOMAIN_ID` | `false`, `keycloak`, `openid`, `default` | Pre-seed federated accounts (see above) |

[`internal/config.go`](internal/config.go) has the complete list, including the
`RECONCILER_DEFAULT_*` network-quota defaults.

## Authentication

Callers authenticate with an **OIDC bearer token**, verified against
`OIDC_ISSUER_URL`. Their group tokens come from the configured role provider;
`ROOT_ADMIN_TOKENS` elevates matching callers to root admin and enables the role
switch. In development, `API_DUMMY_AUTH=true` allows asserting an identity with
the `X-Dummy-Auth-User` header — never available in a production build.

## Build & CI

- `make all` — tests + bundle + build (the local default).
- `make image` — like `all` **without tests**; this is what the Docker build runs.
- `make bundle` — regenerate swagger + the embedded TypeScript client.
- `make dev` — live-reload server, needs [air](https://github.com/air-verse/air).

GitHub Actions builds and pushes a `linux/amd64` image to
`ghcr.io/pfisterer/openstack-management-api`. **Tests deliberately do not run in
CI** — the emulated arm64 build OOM-killed the in-image test run, so testing is a
local step (`make test`). Image tags drive the ArgoCD image updater:
`X.Y.Z-test.N` → staging, `X.Y.Z` → production.

## Deployment

**Normally deployed as part of [cloud-self-service](https://github.com/pfisterer/cloud-self-service)**, the umbrella chart that composes this service with the other three and pins it by version — and a pinned chart version pins its `appVersion`, which pins the image tag. Installing this chart on its own works, but then nothing keeps it in step with the services it talks to.

A Helm chart lives in [`helm-chart/`](helm-chart); the DHBW installation renders
its values per environment from the `dhbw-deployment` repo and syncs with ArgoCD.
The database is expected to be a CloudNativePG cluster in production;
`DB_TYPE=memory` exists for development and demos only.

The chart is published as an OCI artifact on every push to `main`:

```sh
helm pull oci://ghcr.io/pfisterer/charts/openstack-management-api --version 0.8.5-test.1
```

Values for this chart go under its chart name in the umbrella:

```yaml
openstack-management-api:
  openstackManagementApi:
    ...
```

## Related projects

- [cloud-self-service](https://github.com/pfisterer/cloud-self-service) — the umbrella chart that composes all four
- [self-service-ui](https://github.com/pfisterer/self-service-ui) — the web interface
- [role-provider-service](https://github.com/pfisterer/role-provider-service) — group membership and token resolution
- [dynamic-zones](https://github.com/pfisterer/dynamic-zones) — the DNS half of the platform

## License

See [LICENSE](./LICENSE).
