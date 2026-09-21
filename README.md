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

Some requests should not need a decision at all. A budget can approve requests on its own — as a pool, bounded only by the budget itself, or with a cap per requester — so allocations are granted the moment they are asked for and only what exceeds that reaches a human. That is what keeps delegation from turning into a queue one level further down.

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

- **Budget** — an inner node: a delegated pool of capacity. Its `admin_scope` tokens manage it (approve or reject children, edit it, delegate further); its `eligible_requesters` tokens may request child nodes under it. Delegation *is* creating a sub-budget with someone else in `admin_scope` — there is no separate concept for it. A budget may carry `auto_approve`: without a `per_requester_limit` it grants whatever it has room for (a pool), with one only up to that per-person cap. It covers later changes to a project as well.
- **Project** — a leaf: a concrete allocation with exactly one `owner`.
  Lifecycle `pending → approved → released`, plus `change_pending` while a change
  is proposed (rejecting a change returns the node to `approved`). Budgets go
  through the same cycle.
- **Resource catalogue** — what a node's limit is made of. A resource is either a quantity (`kind: count`, the default: cores, RAM, storage — sums across children and is capped by the parent) or an *availability* (`kind: bool`: a network, an image, a GPU flavour — granted or not, passed down rather than divided). An availability carries a `grant` naming what it means in OpenStack by ID: a shared network (Neutron RBAC), an image membership (Glance) or flavour access (Nova). The catalogue is deployment configuration (`RESOURCE_DEFINITIONS`) and replaces the built-in set rather than extending it; an invalid entry stops the service at startup instead of producing a resource that governs nothing. A resource new to the catalogue is added to the root, so it can be delegated; an availability cannot be withdrawn from a budget while a node below still holds it. Each node reports the resources in scope at it (`available_resources`), so a budget granted two of forty shows two.
- **Usage rollup** — approved and `change_pending` leaves consume capacity, aggregated live over every subtree; availabilities never sum. By default a released leaf stays charged until its OpenStack project is actually gone (`API_CHARGE_RELEASED`), and a leaf is charged the larger of its limit and what OpenStack reports it uses (`API_CHARGE_OS_IN_USE`), so shrinking a project after filling it frees nothing on paper. Approving anything checks capacity along the whole ancestor chain, not just the direct parent.
- **Authorization walks the parent chain** — deciding on a node requires a token
  in an *ancestor's* `admin_scope`, so nobody approves their own request. Root
  admins are simply the `admin_scope` of the `root` node, synchronised from
  `ROOT_ADMIN_TOKENS` at startup.
- **Role switch** — a root admin may act within a single group, or fully
  impersonate another identity, to see the platform as that person sees it.
- **Role provider** — pluggable source of a caller's group tokens and of group search: `mock` (built-in test identities, no external dependency) or `http` (the external [role-provider-service](https://github.com/pfisterer/role-provider-service)).
- **History** — every node keeps a history of its lifecycle events, recording who made a change and through which channel (`ui` for the web UI and REST API, `mcp` for an agent acting with a person's token).

Editing follows the same idea. A project leaf accepts exactly one direct edit — a rename, because a name is a label, not an allocation. Everything else goes through a change request, which for a *pending* node is amended in place: that is how a manager trims an over-sized request instead of rejecting it. On an active project a change that gives resources back, ends sooner or only changes members takes effect at once, and so does growth or an extension the budget's auto-approve covers; the rest waits for a manager.

➡️ **Architecture:** [`ARCHITECTURE.md`](ARCHITECTURE.md) — domain model,
authorization rules, API surface, storage, reconciler, SDK pipeline.

## The reconciler

An optional background loop (`RECONCILER_ENABLED`) that makes the tree true in OpenStack: it creates a project per approved leaf, keeps name, description, quota, members and group assignments in sync, grants and revokes the leaf's availabilities, imports unknown OpenStack projects as `imported` leaves so they can be adopted, and handles released ones — their projects are tagged for deletion (or deleted outright with `RECONCILER_DELETE_RELEASED_PROJECTS`), and the released leaf is removed once its project is gone from the reconciler's scope. That scope is the scope parent project (`RECONCILER_SCOPE_PARENT_ID`, or `RECONCILER_SCOPE_PARENT_NAME`, which is created if it does not exist yet): new projects are created under it, and only projects under it are considered for import. Keystone users the reconciler pre-created and that no longer hold any role assignment are pruned. If OpenStack is unreachable at startup, the connection is retried in the background instead of leaving provisioning off until a restart.

Grants only ever touch targets the catalogue names. Flavour access and image members carry no marker saying who created them, so a grant made by hand on anything outside the catalogue is left exactly as it is.

It marks what it owns with Keystone tags, and those tags are the contract with anyone holding only OpenStack credentials (every name and prefix below is configurable):

| Tag | Written when | Purpose |
|---|---|---|
| `managed` | on create | this project belongs to the platform |
| `managed-resource-id:<node>` | on create | which node it belongs to |
| `termination:<RFC3339>` | when the node's termination date changes | read "what runs out when" straight from OpenStack, no access to this API needed |
| `status:<status>` | when the leaf's status changes, including on release | select projects by lifecycle state (above all the released ones) without querying this API |
| `pending-deletion:<date>` | when a leaf is released | scheduled deletion day (grace period) |
| `contact:<email>` | when a leaf is released | who to ask before it goes |

Tag writes happen only when a value actually changed — this loop runs every
interval for every leaf, and an unconditional update would be one Keystone write
per project per tick for a value that changes twice in a project's lifetime.

**Credentials and scopes.** The reconciler authenticates either with an application credential or as a service user with a password; configure exactly one (with both complete, the application credential wins). An application credential is always scoped to a single project, which on a cloud enforcing the modern RBAC scope defaults is not enough to create projects or assign roles across them. A service user can ask for a domain-scoped token instead, and a user that is admin on one domain can do everything the reconciler needs inside that domain and nothing outside it. Those clouds then split the work in two: Keystone wants the domain scope for creating projects, while Nova, Neutron and Cinder accept quota and grant calls only from a project-scoped token. A domain-scoped client therefore builds a second, project-scoped token against the scope parent — the one project that outlives every managed one — and routes compute, network and block-storage calls through it. For every other auth method nothing changes.

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
make dev            # live-reload dev server on :8083 (API_MODE=development)
make test           # go test -cover -coverpkg=./... ./...
make all            # tests + embedded docs + binary
docker build -t openstack-management-api .
```

The in-memory store and the mock role provider are the defaults, so a local run needs no database and no role-provider-service. `OIDC_ISSUER_URL`, `OIDC_CLIENT_ID` and `OPENSTACK_AUTH_URL` are validated at startup even when nothing uses them. To work without an identity provider, add `API_DUMMY_AUTH=true` (and `DB_ADD_MOCK_DATA=true` for a populated tree); then any well-formed values for those three will do.

## API

The REST API is served under `/v1`, and the service publishes its own OpenAPI description — that spec is the reference, so it cannot drift from the implementation the way a hand-written endpoint list does:

- **`GET /swagger.json`** — the OpenAPI spec
- **`GET /config.json`** — unauthenticated bootstrap data for a frontend: the running version and the OIDC settings
- A generated TypeScript client is published to npm as `@dhbw-cloud/os-mgt-client`.
  It used to be served from `/client` and loaded by the browser at startup; consumers
  now depend on a version at build time, so a missing operation is a build error there
  instead of a silent no-op in the browser.

Besides the tree itself (`/v1/nodes`), the spec covers API tokens (`/v1/tokens`), the role switch (`/v1/role-switch`), principal search for token fields (`/v1/principals/search`), the resource catalogue as a frontend sees it (`/v1/config`) and, for root admins, the reconciler's status and a manual trigger (`/v1/admin/reconcile/status`, `/v1/admin/reconcile/trigger`).

**MCP.** `/mcp` serves the [Model Context Protocol](https://modelcontextprotocol.io) (streamable HTTP), so an LLM client can work with projects and budgets on a person's behalf. It sits behind the same authentication as `/v1` — typically an API token — and contains no authorization of its own: every tool calls the same service method the REST handler calls, with that person's rights. A read-only token is not offered the tools that change anything, releasing a project or deleting a budget requires repeating its exact name (`confirm_name`), and every change is recorded in the node's history as coming through `mcp`.

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
| `API_DUMMY_AUTH` | `false` | Dev-only bypass via `X-Dummy-Auth-User`; the service **refuses to start** with it unless `API_MODE=development` |
| `CORS_ALLOWED_ORIGINS` | — | Comma-separated browser origins allowed to call `/v1` cross-origin; empty allows none (right when the UI reaches the API same-origin) |
| `DB_TYPE` | `memory` | `memory` \| `postgres` |
| `DB_CONNECTION_STRING` | local `postgres` DSN | DSN for `postgres` |
| `DB_ADD_MOCK_DATA` | `false` | Seed the mock budget tree (only into an empty store) |
| `ROLE_PROVIDER` | `mock` | `mock` \| `http` |
| `ROLE_PROVIDER_URL`, `ROLE_PROVIDER_API_TOKEN` | — | Required for `http` |
| `ROOT_ADMIN_TOKENS` | — | Comma-separated `user:`/`group:` tokens that become root admins |
| `OIDC_ISSUER_URL`, `OIDC_CLIENT_ID` | — | Bearer-token verification; required at startup |
| `API_TOKEN_TTL_HOURS`, `API_TOKEN_ALLOW_NEVER_EXPIRES` | `24`, `false` | API token lifetime when a request names none (any other lifetime may be requested), and whether tokens without expiry may be issued |
| `API_MAX_AUTHORIZED_USERS` | `32` | Cap on additional members per project |
| `API_CHARGE_OS_IN_USE` | `true` | Charge a leaf the larger of its limit and its measured OpenStack usage |
| `API_CHARGE_RELEASED` | `true` | Keep a released leaf charged until its OpenStack project is gone |
| `RESOURCE_DEFINITIONS` | built-in set | The resource catalogue as a JSON array; replaces the built-in set, an invalid entry stops startup |
| `SERVICE_TIMEOUT_SECONDS` | `30` | Timeout for calls to the role provider and for service requests |
| `RECONCILER_ENABLED` | `false` | Turn the OpenStack loop on |
| `RECONCILER_INTERVAL_SECONDS` | `300` | How often it runs |
| `RECONCILER_DRY_RUN` | `false` | Log what it would do, write nothing |
| `RECONCILER_NO_DELETE` | `false` | Never delete in OpenStack (records of projects already gone are still cleaned up) |
| `RECONCILER_SCOPE_PARENT_NAME` / `_ID` | — | Confine it to one parent project, created by name if missing (`_ID` wins); **never** point this at the domain root |
| `RECONCILER_GROUP_PREFIX` | `managed-` | Prefix of the Keystone groups it creates for group tokens |
| `RECONCILER_MANAGED_PROJECT_TAG`, `RECONCILER_RESOURCE_ID_TAG_PREFIX` | `managed`, `managed-resource-id:` | Ownership tags |
| `RECONCILER_TERMINATION_TAG_PREFIX`, `RECONCILER_STATUS_TAG_PREFIX` | `termination:`, `status:` | Empty disables the tag |
| `RECONCILER_DELETE_RELEASED_PROJECTS` | `false` | Delete a released leaf's project right away instead of tagging it |
| `RECONCILER_PENDING_DELETION_TAG_PREFIX`, `_GRACE_DAYS`, `RECONCILER_CONTACT_TAG_PREFIX` | `pending-deletion:`, `30`, `contact:` | Release handling |
| `OPENSTACK_AUTH_URL`, `OPENSTACK_REGION`, `OPENSTACK_INSECURE` | —, `microstack`, `false` | Endpoint (the URL is required at startup); each falls back to its `OS_*` equivalent (`OS_REGION_NAME` for the region) |
| `OPENSTACK_APPLICATION_CREDENTIAL_ID`, `OPENSTACK_APPLICATION_CREDENTIAL_SECRET` | — | Application-credential auth; falls back to `OS_*` |
| `OPENSTACK_USERNAME`, `OPENSTACK_PASSWORD`, `OPENSTACK_USER_DOMAIN_NAME` | —, —, `Default` | Service-user auth (see *Credentials and scopes*); falls back to `OS_*` |
| `OPENSTACK_SYSTEM_SCOPE`, `OPENSTACK_DOMAIN_NAME`, `OPENSTACK_PROJECT_ID`, `OPENSTACK_PROJECT_NAME`, `OPENSTACK_PROJECT_DOMAIN_NAME` | — | Token scope for service-user auth, in that order of precedence (`OPENSTACK_SYSTEM_SCOPE=all`); falls back to `OS_*` |
| `OPENSTACK_FEDERATED_PROVISIONING`, `OPENSTACK_FEDERATED_IDP_ID`, `_PROTOCOL_ID`, `_DOMAIN_ID` | `false`, `keycloak`, `openid`, `default` | Pre-seed federated accounts (see above) |

[`internal/config.go`](internal/config.go) has the complete list, including the `RECONCILER_DEFAULT_*` network-quota defaults, which apply to the built-in catalogue only.

## Authentication

Callers authenticate with an **OIDC bearer token**, verified against `OIDC_ISSUER_URL`, or with an **API token** issued by this service. Either way their group tokens come from the configured role provider, fetched fresh per request; `ROOT_ADMIN_TOKENS` elevates matching callers to root admin and enables the role switch.

API tokens (prefix `os_mgt_`) are for non-interactive callers — a CI job, a script, an MCP client. They are created, listed and revoked under `/v1/tokens`, carry a description and a lifetime (see the `API_TOKEN_*` settings), record when they were last used, and can be read-only, which rejects every write. A token always belongs to the real caller, never to an identity assumed through the role switch: a temporary view must not turn into a permanent credential. With `DB_TYPE=memory` tokens do not survive a restart.

In development, `API_DUMMY_AUTH=true` allows asserting one of the built-in mock identities with the `X-Dummy-Auth-User` header; the service refuses to start with it unless `API_MODE=development`.

## Build & CI

- `make all` — tests + bundle + build (the local default).
- `make image` — like `all` **without tests**; this is what the Docker build runs.
- `make bundle` — regenerate `swagger.json` and the embedded docs the server compiles in; a fresh clone does not build without it.
- `make dev` — live-reload server, needs [air](https://github.com/air-verse/air).
- `make npm-package` / `make npm-publish` — build and publish the TypeScript client `@dhbw-cloud/os-mgt-client` (a prerelease goes to the `next` dist-tag). Publishing is a manual step, not part of CI.
- `make generate-role-provider-client` — regenerate the Go client for role-provider-service from the spec attached to the release `RP_VERSION` names.
- `make bump V=x.y.z` — set `VERSION` and the chart's `version`/`appVersion` together.

GitHub Actions runs two workflows. **Checks** runs `make bundle`, `go vet` and `make test`, and verifies that the committed role-provider spec and generated client match the pinned release. **Docker Image CI** builds a `linux/amd64` image and pushes it to `ghcr.io/pfisterer/openstack-management-api`. Tests deliberately do not run inside the image build — the emulated arm64 build OOM-killed the in-image test run — so the image build uses `make image`. An image or chart version that is already published is never overwritten, a stable `VERSION` (`X.Y.Z`) additionally gets a Git tag and a GitHub release with notes from the commit subjects, and a prerelease (`X.Y.Z-test.N`) gets neither.

## Deployment

**Normally deployed as part of [cloud-self-service](https://github.com/pfisterer/cloud-self-service)**, the umbrella chart that composes this service with the other three and pins it by version — and a pinned chart version pins its `appVersion`, which pins the image tag. Installing this chart on its own works, but then nothing keeps it in step with the services it talks to.

A Helm chart lives in [`helm-chart/`](helm-chart). Production needs PostgreSQL; `DB_TYPE=memory` exists for development and demos only.

The chart is published as an OCI artifact whenever a push to `main` carries a chart version that is not published yet:

```sh
helm pull oci://ghcr.io/pfisterer/charts/openstack-management-api --version 0.8.13
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
