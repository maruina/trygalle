# Pi Environment and Image Implementation Plan

> Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the approved Pi environment and image design (`plans/pi-environment-and-image/design.md`) as a root `Dockerfile` with its entrypoint, a `deploy/trygalle` Helm chart, a GHCR publishing workflow, and the run/customization documentation, so a user reaches a working Telegram-to-Pi bot with three inputs and no repository build.

**Smallest user-feedback slice:** Slice 1 — a locally built image passes the committed `scripts/image-smoke.sh`: it runs as UID 1000, reports the pinned Pi, Trygalle, and kubectl versions, supplies `--session-dir` from `TRYGALLE_SESSION_DIR` (default `/data/pi/sessions`), and exits non-zero fast with the missing-token diagnostic when run with no inputs.

**Out of Scope:** From the design's non-goals: the private dotfiles checkout, chezmoi rendering, the Git-cloning init container, and deploy-key handling (separate sidecar design); readiness, liveness, and startup probes (follow-up #4); publishing the Helm chart as an OCI artifact; media inputs, webhooks, a Service or Ingress, metrics, tracing, multi-user support; a prompt-injection defense or application-level authorization; any change to Go application code or the `internal/*` packages; Renovate automation for `KUBECTL_VERSION` (deliberate bumps only); Talos-specific scheduling passthroughs (nodeSelector/affinity/tolerations) until the first cluster install shows a need.

**Architecture:** One multi-stage `Dockerfile` (Go builder stage, `node:24-bookworm-slim` runtime stage) installs Trygalle, Pi 1.5.2, and the pinned toolchain, and runs as UID 1000 with `trygalle-entrypoint` as the process hook. `deploy/trygalle` renders a one-replica StatefulSet over a PVC through two subPath mounts, one ServiceAccount, a chart-managed or user-supplied Secret, and four verbatim RBAC passthrough lists. `.github/workflows/release.yml` builds a single `linux/amd64` + `linux/arm64` manifest and pushes it to `ghcr.io/maruina/trygalle` after the shared smoke script passes.

**Tech Stack:** Docker (BuildKit/buildx multi-platform), POSIX `sh` for scripts, Helm chart (`apiVersion: v2`, no dependencies), `values.schema.json`, chart-testing (`ct`, `helm/chart-testing-action`, `actions/setup-python` for Yamale and yamllint), GitHub Actions (`docker/setup-qemu-action`, `docker/setup-buildx-action`, `docker/login-action`, `azure/setup-helm`, `cycjimmy/semantic-release-action`, `docker/build-push-action`), semantic-release with `@semantic-release/commit-analyzer`, `@semantic-release/release-notes-generator`, and `@semantic-release/github`, Renovate custom regex manager (existing).

---

## Skills loaded and used

| Skill | Source | Why loaded | How used |
|---|---|---|---|
| `resolve-worktree` | `prompt-required` | Design path lives in a feature worktree | Resolved `plans/pi-environment-and-image/design.md` to `~/go/src/.worktrees/trygalle/maruina-pi-environment-and-image` (branch `maruina/pi-environment-and-image`); all work happens there |
| `skill-loader` | `prompt-required` | Plan template requires it | Routed planning skills; confirmed no Go, Kubernetes-API, CLI, or Terraform triggers — the chart is Helm/YAML and no `.go` file changes, so `go-best-practices`, `k8s-api-design`, `k8s-controller-dev`, and `cli-best-practices` are not triggered |
| `learning-lookup` | `prompt-required` | Advisory guidance before planning decisions | 18-section store, 7 sections returned for docker/renovate/helm/kubernetes terms; 2 material: "Renovate pins digests only through the docker datasource" (already applied in the design's base-image strategy; also confirms GitHub Actions digest pinning comes from `config:recommended`, no repo change needed) and "Enumerate every versioned surface before scoping Renovate or promotion" (applied: the Renovate manager must cover the Dockerfile pin or that surface strands silently); Datadog-platform, resolv.conf, and capacity sections not material |
| `learning-candidates` | `agent-selected` | A confirmed fix disproved the planned chart-lint mechanism | Recorded three candidates in this file's `## Learning candidates` ledger: `helm lint` ignores `ci/`, `GITHUB_TOKEN` tags do not trigger workflows, and a configured Renovate file does not mean Renovate runs |
| `script-best-practices` | `skill-loader` | POSIX entrypoint, smoke script, and workflow steps will be written | `set -eu`, quoted variables, stderr diagnostics, exec bit, interpreter checks applied to the planned script tasks; the entrypoint stays POSIX `sh` per the design even though the skill prefers `bash` for new scripts |
| `write` | `agent-selected` | The plan is a prose artifact | Applied to this document and the alignment brief |
| `feature-worktree` | `prompt-required` | Writes and commits must stay off `main` | Continued in the resolved design worktree per rule 1 (input path resolved to it); base checkout stays on `main` |

### Execution

Slice 1 executed 2026-11-14 by glm-5.3-flash in the plan worktree (`maruina/pi-environment-and-image`, base `67f23b4`).

| Skill | Source | Why loaded | How used |
|---|---|---|---|
| `skill-loader` | `prompt-required` | Execution stage routing | No Go, Kubernetes-API, CLI, or Terraform triggers; `script-best-practices` is the only language skill this slice's files trigger |
| `script-best-practices` | `skill-loader` | `scripts/image-smoke.sh` and `trygalle-entrypoint` are new POSIX scripts | `set -eu`, quoted expansion, stderr diagnostics, exec bit, constants at top, `mktemp` + `trap` cleanup applied to the smoke script; the entrypoint stays POSIX `sh` per the design |

Task 1 deviation note: the smoke script extracts the pin literals with `grep -o`, not the planned `rg -o` — GitHub-hosted runners do not guarantee ripgrep, and `grep` keeps the script dependency-free on every machine that runs it. The check itself (Go constant == Dockerfile literal == in-image `pi --version` == 1.5.2) is unchanged. Verified locally: `docker build -t trygalle:smoke .` green (linux/arm64 native), smoke script PASS on all checks, `go test -race -short ./...`, `go fix -diff ./...`, and `gofmt -l` clean.

## Planning alignment brief (confirmed 2026-11-14)

Confirmed decisions the user approved in chat before this plan was written:

- **Design-review gate waiver.** The design-only review report (2026-11-13) reviewed blob `60b99be` and ended in NEEDS CLARIFICATION; the user confirmed every fix verbatim in the same session (secret-surface decision, multi-arch, `TRYGALLE_SESSION_DIR` typo, findings 4–6 and the two low fixes), the fixes were applied and committed as `785adb8` (current blob `c0f1c33da6b10e5f7d7b7b4daa31c1ecdbd3a415`, status `Approved`), and the user directed planning to proceed without a fresh review round. Recorded here as the approval evidence for the design-to-plan handoff.
- **Kubectl pin `v1.37.2`** (design open question resolved): the Talos cluster's API server reports `v1.37.1` (verified with `kubectl --context talos-lan version`), and kubectl must stay within one minor of the server, so the design's example `1.34.2` is out of skew for the primary consumer. `KUBECTL_VERSION` stays a deliberate, non-Renovate-managed pin.
- **Literal Pi version in the npm install line** instead of the design example's `ARG PI_VERSION=1.5.2`: the existing Renovate matchString `@earendil-works/pi-coding-agent@(?<currentValue>...)` already matches that shape, so the only Renovate change is adding `/^Dockerfile$/` to `managerFilePatterns`; the ARG form would match nothing and need a bespoke regex. Mechanism (one bump PR moves every pin surface) is unchanged. `ARG KUBECTL_VERSION` stays because kubectl is deliberately not Renovate-managed.
- **Smoke checks as `scripts/image-smoke.sh`**, committed in slice 1 and invoked by a new `image` job in `ci.yml` (every pull request and push to `main`) and by `release.yml` in slice 3, so both workflows run the same already-proven checks; rejected inline workflow steps (branching logic in YAML, no local reuse).
- **Image build and smoke gate in pull-request CI.** The `image` job builds `trygalle:smoke` and runs the smoke script on every pull request, so a broken `Dockerfile` fails before merge instead of on the next push to `main`; `release.yml` re-runs the same script before it pushes. Cost: one `linux/amd64` image build per pull request.
- **Chart fixtures in `deploy/trygalle/ci/*.yaml` are linted by chart-testing.** `ct lint` lints a chart once for each `*-values.yaml` file in its `ci/` directory, which is the convention the fixtures follow. Plain `helm lint` does not read that directory: with Helm v4.3.1, a chart whose defaults violate its own schema fails `helm lint` even when a valid `ci/` fixture exists, and an invalid `ci/` fixture is ignored. `ct lint --config ct.yaml` is therefore the chart lint gate, and `helm template -f <fixture>` renders a fixture for the property assertions, so the strict schema never blocks chart-level lint.
- **Chart-testing config follows the sibling repository.** `ct.yaml` sets `chart-dirs: [deploy]`, `target-branch: main`, and `check-version-increment: false`, because the chart version is a deliberate release decision, not a per-PR bump — the same reason `maruina/go-infrabin` PR #475 disabled that check. `ct install` is not used: the chart requires real Telegram and provider credentials at runtime, so a kind-cluster install with fixture values would exit non-zero on the missing token and fail the gate for the wrong reason.
- **CI follows the current Helm and chart-testing releases.** `azure/setup-helm` installs the latest Helm and `helm/chart-testing-action` the latest `ct`; the chart commands were verified against Helm v4.3.1 and `ct`'s documented `ci/` behaviour. A release that changes lint or schema behaviour is a plan amendment instead of a silent pin.
- **Automatic release with semantic-release** (user decision, 2026-11-14): every push to `main` runs `cycjimmy/semantic-release-action` with `.releaserc.json`; conventional commit titles decide the version, and the same job builds and pushes the images because GitHub does not run workflows for tags pushed with the default `GITHUB_TOKEN`. `release.yml` therefore has no `v*` tag trigger and publishes no `sha-` tag; a release publishes `v<version>` and `latest`. This supersedes the design's tag-push publishing model and needs a design amendment before `/execute`.
- **Commit types decide the version, not authors.** `.releaserc.json` maps `feat` to minor and `fix` and `chore` to patch, so Renovate's `chore(deps):` squash titles release; `docs:`, `ci:`, and `test:` publish nothing, and an unconventional merge title silently produces no release. Renovate needs `"semanticCommits": "enabled"` in `.github/renovate.json5` for that mapping to hold.
- **Renovate must be enabled, not only configured.** `.github/renovate.json5` landed in commit `0fc985d` (2026-11-11), but the repository has no Dependency Dashboard issue, no `renovate/*` branch, and no Renovate-authored pull request, so onboarding has never run and the single-bump-PR mechanism is unproven. Task 8 gates the release automation on a real Renovate dependency PR.
- **Strict schema + fixture-driven lint.** The schema rejects empty `telegram.token`/`telegram.allowedUser` when `secret.create` is true, which the chart defaults alone violate; this is deliberate — a chart whose defaults pass lint with an empty token would let that token reach runtime. Rejected alternative: loosening the schema so default values lint clean (benefit: plain `helm lint` without `-f` passes; cost: silently empty credentials).
- **Builder stage cross-compiles instead of emulating:** `FROM --platform=$BUILDPLATFORM golang:1.27-bookworm` with `GOARCH=$TARGETARCH` in the build step, so the arm64 build does not run the Go compiler under QEMU. Build-mechanics improvement over the design's example Dockerfile; the produced image is identical.
- **First release and chart version.** No Git tags exist, so semantic-release computes the first release version from the commit history; a local `npx semantic-release --dry-run` records the resolved version before merge (Task 5). `Chart.yaml` keeps `version: 0.1.0` and `appVersion: "0.1.0"` in git, `ct.yaml` disables the version-increment check, and Task 7 syncs `appVersion` to the first released version so `image.tag: ""` resolves to an existing image. Chart publication as an OCI artifact stays deferred (design non-goal).
- **Slice split:** three slices as the design names; slice 2 (chart) does not block on slice 1 because `ct lint` and `helm template` render without the image; slice 3 blocks on both.

### Advisory learning records

- Renovate digest learning: applied via the design (base images referenced by tag, digests recorded by `config:recommended`'s docker datasource).
- Versioned-surface enumeration learning: the Renovate manager change in Task 2 exists because of it; the kubectl ARG pin is recorded as a deliberately unmanaged surface.

---

## Implementation Contract

### Components Affected

| Component | Files | Responsibility | Verification |
|---|---|---|---|
| Container image | `Dockerfile` | Two-stage build; pinned Pi, kubectl, apt tools; UID 1000; writable `/data/pi` paths; OCI source label | `scripts/image-smoke.sh` against a local build |
| Entrypoint | `trygalle-entrypoint` | Supplies `--pi-bin pi --session-dir` (env-overridable), preserves extra args, sidecar hook | Smoke script's fake-`trygalle` args check |
| Pin surface | `.github/renovate.json5` | Adds the Dockerfile to the existing custom manager so one bump PR moves all Pi pin surfaces | Pattern grep + unchanged full-suite pin gate |
| Smoke script | `scripts/image-smoke.sh` | The design's image smoke checks plus the `harness.PinnedPiVersion` cross-check, shared by local runs and CI | Direct execution; runs in the `ci.yml` image job and in `release.yml` |
| Helm chart | `deploy/trygalle/` (Chart.yaml, values.yaml, values.schema.json, `_helpers.tpl`, `serviceaccount.yaml`, `secret.yaml`, `statefulset.yaml`, `pvc.yaml`, `rbac-role.yaml`, `rbac-rolebinding.yaml`, `rbac-clusterrole.yaml`, `rbac-clusterrolebinding.yaml`, `ci/*.yaml`), `ct.yaml` | One-replica StatefulSet, PVC subPath mounts, Secret-backed credentials, verbatim RBAC passthroughs, volume and container passthroughs | `ct lint --config ct.yaml` (one lint per `ci/` fixture) + `helm template -f <fixture>` + grep assertions |
| Image CI | `.github/workflows/ci.yml` (new `image` job) | Builds `trygalle:smoke` and runs the smoke script on every PR and push to `main` | Green job on the slice-1 PR |
| Chart CI | `.github/workflows/ci.yml` (new `chart` job) | Runs `ct lint` and the template assertions on every PR and push to `main` | Green job on the slice-2 PR |
| Release policy | `.releaserc.json` | Conventional-commit version mapping (`feat` minor; `fix` and `chore` patch; other types none) and release-notes publication | `semantic-release --dry-run` output; first release verified in Task 7 |
| Publishing | `.github/workflows/release.yml` | semantic-release on every push to `main`; image build, smoke gate including the pin cross-check, and GHCR push only for a published release (`v<version>` and `latest`) | YAML parse; `semantic-release --dry-run`; first release run verified in Task 7 |
| Documentation | `README.md`, `docs/customize.md`, `AGENTS.md` | Run quickstart, customization guide, image/chart developer instructions | Doc review in Task 6 |

### Key Decisions

1. **Literal Pi pin in the Dockerfile npm line.** Rationale: the existing Renovate matchString covers it unchanged. Rejected alternative: `ARG PI_VERSION` with a new matchString — benefit of the ARG form is a parameterized build arg, rejected because no consumer needs it and it adds a regex that duplicates the npm pattern.
2. **`KUBECTL_VERSION` pinned to `v1.37.2`** with SHA256 verification against the published `.sha256` file. Rationale: within the supported skew of the primary consumer's v1.37.1 API server; kubectl is the tool with real Kubernetes blast radius, so its pin is deliberate, not Renovate-managed.
3. **Smoke checks in a committed script.** Rationale: branching/fail-fast logic belongs in shell, not inline YAML; one source of truth for local and CI. Rejected alternative: inline steps — benefit is fewer files, rejected because the release workflow then owns logic nobody can run locally.
4. **Builder cross-compilation (`BUILDPLATFORM` + `GOARCH=$TARGETARCH`).** Rationale: arm64 image builds without emulating the Go compiler; C is not in the build path (`CGO_ENABLED=0`).
5. **Strict schema + fixture-driven lint.** Rationale: defaults that pass lint with empty credentials hide the misconfiguration until runtime; fixture values are the honest CI input. See the brief.
6. **No Service object despite StatefulSet `serviceName`.** Rationale: `spec.serviceName` is a network-identity field, not a dependency; the pod schedules without the headless Service existing, and the design creates no Services. The chart sets `serviceName` to the chart fullname.
7. **Image contains zero credentials by construction.** Everything sensitive enters through runtime environment variables from a Secret; no build args carry secrets; example manifests and fixture values use dummy placeholders only.
8. **semantic-release, with the artifacts in the same job.** Rationale: a tag pushed by the default `GITHUB_TOKEN` does not trigger a second workflow, so the image build must run in the release job; semantic-release also makes the version a function of commit history instead of a manual tag. Rejected alternative: keep the tag-push workflow — benefit is a slimmer release job, rejected because it never runs for automated tags.
9. **chart-testing with `check-version-increment: false`.** Rationale: the chart version is bumped deliberately at rollout, so a per-PR change must not fail a version check, and `ct lint` is the stock tool that lints every `ci/` fixture. Rejected alternative: hand-rolled `helm lint -f` loops in workflow YAML — benefit is one fewer tool, rejected because it reimplements fixture discovery inside YAML, which is where the original error came from.
10. **No `ct install`.** Rationale: the chart needs real credentials at runtime, so a fixture-driven kind install exits non-zero on the missing token. Chart installation stays manual acceptance (Task 7).

### Implementation Constraints

- **No Go code changes.** `go test -race -short ./...` must stay green after every slice; `go fix -diff ./...` and `gofmt -l` remain clean (they apply to existing files; nothing new is expected).
- **Renovate manager:** one custom manager, one more `managerFilePatterns` entry (`/^Dockerfile$/`), `"semanticCommits": "enabled"`, no new matchString, no datasource change.
- **Release workflow:** one job; `permissions: contents: write, packages: write`; no `v*` tag trigger; no artifact step runs unless semantic-release reports `new_release_published == 'true'`; the workflow holds no version literal.
- **Chart lint:** `ct lint --config ct.yaml` only; `ct install` and kind clusters are out of scope.
- **Chart:** `apiVersion: v2`, no dependencies; no Service, Ingress, probes, or resource annotations; one replica; no embedded RBAC policy (the four lists render user objects verbatim with name/namespace defaults only).
- **Image runtime user:** UID/GID 1000 (`node`), no root paths; `podSecurityContext.fsGroup: 1000` in the chart is the only ownership shim for PVC mounts.
- **Scripts:** POSIX `sh`, `set -eu` (smoke script), quoted expansion, diagnostics to stderr, executable bit preserved by `COPY` (the repo files must already carry the exec bit).
- **Stop conditions:** if Pi 1.5.2 fails to install cleanly on `linux/arm64` or requires a different Node major than 24 (design open question 2, rechecked: the installed Pi 1.5.2 declares `engines: node >=22.19.0`, which Node 24 satisfies), stop the affected task and report instead of weakening the pin or pin shape.
- **Design deltas:** the Dockerfile deviates from the design's example in two recorded ways (literal Pi pin, cross-compile builder), and publishing deviates in one user-approved way (semantic-release on `main` replaces the tag-push and `sha-` tag model, so `plans/pi-environment-and-image/design.md` needs that amendment first); everything else follows the example.

### Security and Observability Requirements

- The image and repository contain no tokens, keys, or user IDs; fixture and example values use clearly fake placeholders; the chart's Secret holds credentials only as `stringData` rendered from values or referenced from the user's existing Secret.
- kubectl enters through a pinned URL verified against its published SHA256 checksum at build time.
- The container runs as UID 1000 without privileged mode, host mounts, or added capabilities; no Kubernetes API access exists unless the user renders RBAC objects.
- Startup failures stay loud and non-secret: the existing binary behavior is untouched, and the smoke script asserts the missing-token exit.
- Image identification: `org.opencontainers.image.source` baked via `LABEL`; version and revision labels applied at push time from the semantic-release version and `github.sha`.
- No new log surfaces: Trygalle lifecycle logs and Pi stderr carry diagnostics as before.

### Failure Modes to Handle

| Failure | Handling |
|---|---|
| npm install of the pinned Pi fails at build | Build fails; nothing is pushed (push happens only after build and smoke pass) |
| kubectl checksum mismatch | Build fails on `sha256sum --check` |
| Smoke check fails in `release.yml` | Workflow stops before push; no broken image is published |
| Bad/revoked Telegram token at runtime | Existing binary logs the Telegram rejection and exits non-zero (manual acceptance, Task 7) |
| Poll conflict (409) from two containers on one token | Existing fatal exit path (manual acceptance, Task 7) |
| Missing provider key in the user's existing Secret | Pod starts, Trygalle exits non-zero loudly at startup; documented in `docs/customize.md` |
| First GHCR publish 403 | Workflow grants `permissions: packages: write`; the first release run is verified before announcing the image (Task 7) |
| Merge to `main` without a release-warranting commit | semantic-release publishes nothing, every artifact step skips, and the previous image stays current |
| Non-conventional squash title | semantic-release fails closed and publishes nothing; re-land the commit with a conventional title |
| `ct lint` fails on chart YAML style | `ct lint` runs Yamale and yamllint as well as `helm lint`; add a `lintconf.yaml` that relaxes the rule the chart genuinely needs instead of disabling validation |
| Renovate not installed on the repository | No dependency PR appears; Task 8 blocks the rollout until the Renovate GitHub App is installed and the Dependency Dashboard issue exists |

### Rollout and Rollback

- **Rollout (owner: Matteo):** confirm Renovate is enabled (Task 8); merge the three slices; the merge to `main` runs semantic-release, so verify the tag, the release notes, and the two-platform image; sync `Chart.yaml` `appVersion` to that release; install the chart on the Talos cluster with the parent design's restricted RBAC supplied through the four value lists; run the real-bot quickstart.
- **Rollback:** redeploy the previous image tag and chart revision (`helm rollback`); the PVC persists, so sessions survive; images exist only for released versions, so a rollback picks an earlier `v*` tag; removing the rendered RBAC objects or scaling the StatefulSet to zero contains Kubernetes access immediately. No schema migration exists; chart 0.1.0 is the rollback floor.

### Test Strategy

- **Highest deterministic supported interface:** the built image itself (docker run) for boot/pin/user checks; Helm rendering with fixture values for chart behavior; pattern matching on the Renovate config and Dockerfile for pin coverage.
- **Seams:** `scripts/image-smoke.sh` is one new seam, justified because local runs, the `ci.yml` image job, and `release.yml` share it; `deploy/trygalle/ci/*.yaml` fixtures are the chart-testing seam, linted by `ct lint` and rendered with `-f` for the assertions; no Go test seams are added because no Go behavior changes.
- **Tool versions:** no pin; `azure/setup-helm` installs the current Helm and `helm/chart-testing-action` the current `ct`, and every chart command was verified against Helm v4.3.1. A tool release that changes lint or schema behaviour is a plan amendment.
- **Release evidence:** `npx semantic-release --dry-run` records the version a given commit set resolves to, so the commit-type mapping is proven without publishing; the first real release is verified in Task 7.
- **Mocked boundaries:** none needed — the smoke checks need no network (credential rejection happens before any Telegram call), and chart rendering is local. Real Telegram/provider/cluster interactions stay manual (design validation limits).
- **Narrow failing first command:** `scripts/image-smoke.sh trygalle:smoke` fails before `Dockerfile` exists; the first chart task's `ct lint --config ct.yaml` fails before the chart and `ct.yaml` exist.
- **Unchanged suites:** `go test -race -short ./...` gates every slice; the full suite and its pin gate are untouched CI.

---

## Requirements

### Requirement: Loud credential failure
The image SHALL exit non-zero quickly with a missing-token diagnostic when started with no inputs, and SHALL NOT start the poll loop or wait indefinitely.

#### Scenario: no inputs
- GIVEN a built image
- WHEN `docker run` starts it with no environment and no args
- THEN the process exits non-zero within seconds and stderr contains `missing bot token (set TRYGALLE_TELEGRAM_TOKEN)`

### Requirement: Pinned toolchain
The image SHALL contain Pi at the version `harness.PinnedPiVersion` declares, the Trygalle binary, kubectl at `KUBECTL_VERSION`, and `bash`, `git`, `curl`, `jq`, `ripgrep`, and CA certificates on PATH.

#### Scenario: version and tool checks
- GIVEN a built image
- WHEN the smoke script runs `pi --version`, `trygalle --help`, and `kubectl version --client` inside the image
- THEN `pi --version` equals the Dockerfile npm literal and the `PinnedPiVersion` value the script extracts from `internal/harness/harness.go`, `kubectl version --client` matches the `ARG KUBECTL_VERSION` literal, every required tool resolves, and `trygalle --help` exits 0

### Requirement: Non-root runtime
The container SHALL run as UID/GID 1000 with writable `/data/pi/sessions` and `/data/pi/work`.

#### Scenario: user and writability
- GIVEN a built image
- WHEN the smoke script checks `id -u` and writes probe files in both `/data/pi` paths
- THEN `id -u` prints `1000` (not `0`) and both writes succeed

### Requirement: Session directory override and flag passthrough
The entrypoint SHALL exec Trygalle with `--pi-bin pi` and `--session-dir "$TRYGALLE_SESSION_DIR"`, defaulting to `/data/pi/sessions`, and SHALL pass any extra args through to Trygalle.

#### Scenario: default and override
- GIVEN a fake `trygalle` executable that echoes its arguments
- WHEN the entrypoint runs with and without `TRYGALLE_SESSION_DIR`
- THEN the echoed arguments contain `--pi-bin pi` and `--session-dir /data/pi/sessions` (default) or `--session-dir <override>`, plus any extra args in order

### Requirement: Single-PR Pi pin coverage
The Renovate custom manager SHALL match the Dockerfile so one bump PR updates the Go constant, the CI install line, `AGENTS.md`, and the Dockerfile together, and the Renovate app SHALL be installed on the repository so that the manager runs at all.

#### Scenario: pattern and line match
- GIVEN `.github/renovate.json5` and `Dockerfile`
- WHEN the `managerFilePatterns` and matchStrings are checked against the Dockerfile's npm install line
- THEN the Dockerfile pattern is present and the npm matchString matches the pinned version literal in the file

#### Scenario: renovate runs
- GIVEN the repository with the Renovate app installed
- WHEN Renovate completes onboarding
- THEN a Dependency Dashboard issue exists and a dependency update arrives as a pull request whose title starts with `chore(deps):`, which semantic-release maps to a patch release

### Requirement: Default chart install renders complete
The chart SHALL render, from default-shaped values, one ServiceAccount, one Secret carrying the Telegram token, allowed user, and every `providerEnv` entry as `stringData`, one PVC, and a one-replica StatefulSet that references the ServiceAccount, mounts the claim at `/data/pi/sessions` and `/data/pi/work` through two `subPath` entries, sets `TRYGALLE_SESSION_DIR` from `trygalle.sessionDir`, wires the three credential environment names through `valueFrom.secretKeyRef` plus `extraEnv` as literal env, and passes `extraVolumes`, `extraVolumeMounts`, `extraContainers`, and `extraInitContainers` through when they are set.

#### Scenario: default fixture template
- GIVEN `deploy/trygalle/ci/ci-default-values.yaml` with dummy credentials, one `providerEnv` entry, one `extraVolumes` entry, and one `extraContainers` entry
- WHEN `helm template` renders with those values
- THEN the output contains the StatefulSet with both subPath mounts, `serviceAccountName`, `TRYGALLE_SESSION_DIR: /data/pi/sessions`, the `secretKeyRef` env entries for `TRYGALLE_TELEGRAM_TOKEN`, `TRYGALLE_TELEGRAM_ALLOWED_USER`, and the provider key name, the chart-managed Secret with the matching keys, the PVC, and the passed-through volume and container

### Requirement: Existing-secret mode
When `secret.create` is false and `secret.existingSecret` is set, the chart SHALL create no Secret and SHALL reference the user's Secret for the two Telegram keys (via `secret.keys`) and one key per `providerEnv` entry (key name = environment name).

#### Scenario: existing-secret fixture template
- GIVEN `deploy/trygalle/ci/ci-existing-secret-values.yaml`
- WHEN `helm template` renders with those values
- THEN no Secret object is rendered and every `secretKeyRef` points at the named existing Secret with the fixture's key names

#### Scenario: negative — no chart Secret in existing mode
- GIVEN the same fixture
- WHEN the rendered output is searched for a chart-created Secret
- THEN only user-passed RBAC objects and the StatefulSet/ServiceAccount/PVC appear; no `kind: Secret` is rendered

### Requirement: RBAC passthrough with fail-safe default
The chart SHALL render each `rbac.roles`, `rbac.roleBindings`, `rbac.clusterRoles`, and `rbac.clusterRoleBindings` entry verbatim (defaults only for `metadata.name` and namespaced `metadata.namespace`), and SHALL render no RBAC objects when the lists are empty.

#### Scenario: populated fixture
- GIVEN `deploy/trygalle/ci/ci-rbac-values.yaml` with one Role (minimal fragment, no metadata), one RoleBinding, one ClusterRole, and one ClusterRoleBinding
- WHEN `helm template` renders with those values
- THEN all four objects appear with the defaulted names, the Role carries the user's `rules`, and the StatefulSet references `rbac.serviceAccount.name`

#### Scenario: negative — default values
- GIVEN default-shaped fixture values
- WHEN the rendered output is searched for `kind: Role`, `RoleBinding`, `ClusterRole`, or `ClusterRoleBinding`
- THEN no match is found

### Requirement: Values schema validation
`values.schema.json` SHALL require non-empty `telegram.token` and a numeric `telegram.allowedUser` when `secret.create` is true, require `secret.existingSecret` when it is false, and validate value shapes (string maps for `providerEnv`/`extraEnv`, arrays for the RBAC lists and passthroughs).

#### Scenario: incomplete values rejected
- GIVEN values that set neither `telegram.token` (with `secret.create: true`) nor `secret.existingSecret`
- WHEN `helm template deploy/trygalle --set secret.create=true --set telegram.token=` runs
- THEN the command exits non-zero with an error that names `values.schema.json`

### Requirement: Persistence toggle
When `persistence.enabled` is true and `persistence.existingClaim` is empty, the chart SHALL render a PVC with the configured size and storage class; when `existingClaim` is set it SHALL use that claim instead of creating one; when `persistence.enabled` is false it SHALL render no PVC and no `/data/pi` mounts.

#### Scenario: created claim
- GIVEN the default fixture values with `persistence.enabled: true` and `persistence.existingClaim: ""`
- WHEN `helm template` renders
- THEN one PVC is rendered with the configured `size` and `storageClass`, and the StatefulSet volume references that claim

#### Scenario: existing claim
- GIVEN the default fixture values with `--set persistence.existingClaim=my-claim`
- WHEN `helm template` renders
- THEN no PVC is rendered and the StatefulSet volume's `claimName` is `my-claim`

#### Scenario: negative — disabled persistence
- GIVEN fixture values with `persistence.enabled: false`
- WHEN `helm template` renders
- THEN no PVC is rendered and the StatefulSet has neither data volume nor `/data/pi` mounts

### Requirement: Automatic release from conventional commits
The release workflow SHALL, on every push to `main`, run semantic-release over the commits since the last tag, derive the version from the commit titles (`feat` minor; `fix` and `chore` patch; every other type no release), and publish the Git tag and GitHub release notes from the same job that builds the image.

#### Scenario: version mapping
- GIVEN the repository at a commit set that contains one `feat:` commit, or only `fix:`/`chore:` commits, or no release-warranting commit
- WHEN `npx semantic-release --dry-run` runs against that history
- THEN it reports a minor, a patch, or no release respectively

#### Scenario: negative — no release
- GIVEN a push to `main` whose only commits are `docs:` or `ci:`
- WHEN the release workflow runs
- THEN semantic-release publishes no tag and release notes, and every image step is skipped

### Requirement: Multi-platform publishing
The release workflow SHALL push `ghcr.io/maruina/trygalle:v<version>` and `ghcr.io/maruina/trygalle:latest` for every published release, as one manifest listing `linux/amd64` and `linux/arm64`, and only after `scripts/image-smoke.sh` passes, which includes comparing the Dockerfile npm literal with `harness.PinnedPiVersion`. It SHALL push no image when no release is published.

#### Scenario: workflow shape
- GIVEN `.github/workflows/release.yml`
- WHEN the workflow file is parsed and its build-push step inspected
- THEN the YAML is valid, `permissions` grants `contents: write` and `packages: write`, there is no `v*` tag trigger, the login, build, smoke, and push steps are gated on `new_release_published == 'true'`, the platforms are `linux/amd64,linux/arm64`, and the tags are `v<version>` and `latest`

### Requirement: Run and customization documentation
`README.md` SHALL gain a Run section with a `docker run` quickstart (three inputs plus a `/data/pi` volume) and a `helm install` quickstart (minimal values file), keeping the existing source-run instructions; `docs/customize.md` SHALL cover agent-directory extensions/skills/themes (with `PI_CODING_AGENT_DIR`, its Node-dependency caveat, and the Linux compatibility limit), `models.json` plus provider env, a derived image, the four RBAC lists with the restricted `ClusterRole` example, and the project-trust warning for project-directory resources.

#### Scenario: doc review
- GIVEN the two documents
- WHEN a reader follows the `docker run` and `helm install` commands exactly as written
- THEN every flag, path, and value name exists in the image or chart, and no real credentials appear

---

## Slices and tasks

### Slice 1: Image and pin surface (smallest user-feedback slice)
**Delivers:** a locally buildable, runnable image that passes the committed smoke script, plus Renovate coverage of the Dockerfile pin. User feedback: run `docker build` and `scripts/image-smoke.sh` locally and watch the checks pass.

### Task 1: Dockerfile, entrypoint, and smoke script
**Delivers:** `docker build -t trygalle:smoke .` succeeds and `scripts/image-smoke.sh trygalle:smoke` passes all checks (loud missing-token exit, pinned pi/trygalle/kubectl, required tools, UID 1000, writable `/data/pi` paths, entrypoint default/override args).
**Blocked by:** None
**Traces to:** Requirements "Loud credential failure", "Pinned toolchain", "Non-root runtime", "Session directory override and flag passthrough"; design Image and Entrypoint sections
**Files:** `Dockerfile`, `trygalle-entrypoint`, `scripts/image-smoke.sh`, `.github/workflows/ci.yml`

- [x] Write the smoke script first and run `scripts/image-smoke.sh trygalle:smoke`; expect failure because the image does not exist. (Failed at the Dockerfile pin extraction, the first check that needs a not-yet-existing file; the image checks behind it could not pass either.)
- [x] Implement the two-stage `Dockerfile`: `FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS build` with `ARG TARGETARCH`, `CGO_ENABLED=0 GOARCH=$TARGETARCH go build -trimpath`; runtime stage `node:24-bookworm-slim` with apt tools, the literal pinned npm line (`--ignore-scripts`), the checksummed kubectl at `ARG KUBECTL_VERSION=v1.37.2` downloaded from `https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl` (the `v` is part of the ARG, so the URL adds none), `LABEL org.opencontainers.image.source`, the copied binary and entrypoint, `mkdir`/`chown` for `/data/pi/{sessions,work}`, `USER node`, `WORKDIR /data/pi/work`, `ENTRYPOINT ["/usr/local/bin/trygalle-entrypoint"]`. (`ARG TARGETARCH` is declared in the runtime stage too, because kubectl's URL needs it there.)
- [x] Write `trygalle-entrypoint` as the design's POSIX script: `exec trygalle --pi-bin pi --session-dir "${TRYGALLE_SESSION_DIR:-/data/pi/sessions}" "$@"` with the deliberate sidecar-hook comment.
- [x] Implement `scripts/image-smoke.sh` (POSIX, `set -eu`): run with no inputs expecting non-zero + diagnostic; fake-`trygalle` PATH check for the entrypoint args (default and override); the pin cross-check (extract `PinnedPiVersion` from `internal/harness/harness.go` with `rg -o`, extract the npm literal from `Dockerfile`, and require all three — Go constant, Dockerfile literal, in-image `pi --version` — to be equal); `trygalle --help`; `kubectl version --client` against the `ARG KUBECTL_VERSION` literal; tool resolution checks; `id -u` = 1000; probe writes in both `/data/pi` paths. Every check captures output and matches it after an explicit exit-code check, because POSIX `sh` has no `pipefail`.
- [x] Set the exec bit on `trygalle-entrypoint` and `scripts/image-smoke.sh` (`COPY` preserves it).
- [x] Add the `image` job to `.github/workflows/ci.yml`: checkout, `docker build -t trygalle:smoke .`, `scripts/image-smoke.sh trygalle:smoke` on `ubuntu-latest` for every pull request and push to `main`.
- [x] Run `docker build -t trygalle:smoke . && scripts/image-smoke.sh trygalle:smoke`; expect every check green.
- [x] Run `go test -race -short ./...`; expect unchanged green.
- [x] Commit with `feat: add docker image, entrypoint, and image smoke script`.

### Task 2: Renovate manager covers the Dockerfile
**Delivers:** one bump PR will update all four Pi pin surfaces.
**Blocked by:** Task 1
The Renovate proof needs the pinned line to exist in `Dockerfile`.
**Traces to:** Requirement "Single-PR Pi pin coverage"; design Pi installation decision
**Files:** `.github/renovate.json5`

- [ ] Add `/^Dockerfile$/` to `managerFilePatterns` and `"semanticCommits": "enabled"`; change nothing else (the semantic commits setting is what gives semantic-release a conventional title to read).
- [ ] Prove the match without running Renovate: `rg -n '"Dockerfile"' .github/renovate.json5` and `rg -o 'pi-coding-agent@[0-9]+\.[0-9]+\.[0-9]+' Dockerfile` (the existing npm matchString matches this line by construction); expect both to output the expected lines.
- [ ] Run `go test -race -short ./...`; expect green (the pin gate's other surfaces are untouched).
- [ ] Commit with `build: let renovate manage the dockerfile pi pin`.

### Slice 2: Helm chart
**Delivers:** `ct lint` green over the `ci/` fixtures, `helm template` assertions passing for the default, existing-secret, RBAC, persistence, and schema-rejection cases, and a `chart` job in `ci.yml`. User feedback: run `ct lint --config ct.yaml` locally and render each fixture.

### Task 3: Chart scaffold, defaults, schema, ServiceAccount, PVC
**Delivers:** the chart lints green under `ct lint` with its `ci/` fixture and renders SA/PVC/defaults correctly.
**Blocked by:** None
The chart renders without the image.
**Traces to:** Requirements "Default chart install renders complete", "Values schema validation", "Persistence toggle"
**Files:** `deploy/trygalle/Chart.yaml`, `deploy/trygalle/values.yaml`, `deploy/trygalle/values.schema.json`, `deploy/trygalle/templates/_helpers.tpl`, `deploy/trygalle/templates/serviceaccount.yaml`, `deploy/trygalle/templates/pvc.yaml`, `deploy/trygalle/ci/ci-default-values.yaml`, `deploy/trygalle/.helmignore`, `ct.yaml`

- [ ] Run `ct lint --config ct.yaml`; expect failure because the chart and `ct.yaml` do not exist. Install chart-testing locally first (`brew install chart-testing`; it is not present on this machine).
- [ ] Create `Chart.yaml` (`apiVersion: v2`, `name: trygalle`, `version: 0.1.0`, `appVersion: "0.1.0"`, no dependencies) and `values.yaml` with the design's value block (image, telegram, secret + keys, providerEnv, extraEnv, trygalle sessionDir/args, rbac, persistence, podSecurityContext, and the `extraVolumes`/`extraVolumeMounts`/`extraContainers`/`extraInitContainers` passthroughs, all empty).
- [ ] Write `values.schema.json`: conditional requirements per the design (`telegram.token` non-empty string and numeric `telegram.allowedUser` when `secret.create` is true; `secret.existingSecret` non-empty when false; object/array shapes for the rest, including the four passthrough lists).
- [ ] Write `_helpers.tpl` (fullname, name, common labels, serviceAccountName resolver) and `serviceaccount.yaml` (created when `rbac.serviceAccount.create`, named `rbac.serviceAccount.name` or the fullname).
- [ ] Write `pvc.yaml` (claim when enabled and no `existingClaim`; size/storageClass from values).
- [ ] Create `deploy/trygalle/ci/ci-default-values.yaml` with dummy values only — `telegram.token: "123456:AaDUMMYTOKEN"`, `allowedUser: 42`, one dummy `providerEnv` entry, one `extraVolumes` and one `extraContainers` entry — and never a real credential.
- [ ] Create `ct.yaml` (`chart-dirs: [deploy]`, `target-branch: main`, `check-version-increment: false`) and run `ct lint --config ct.yaml`; expect green, with one lint per `ci/` fixture. Bare `helm lint deploy/trygalle` fails on the strict schema by design (Key Decision 5), so do not weaken the schema to make it pass.
- [ ] Commit with `feat: add trygalle helm chart scaffold`.

### Task 4: Secret, StatefulSet, RBAC renderers, and chart CI
**Delivers:** the full chart renders all fixtures correctly and CI guards it.
**Blocked by:** Task 3
**Traces to:** Requirements "Default chart install renders complete", "Existing-secret mode", "RBAC passthrough with fail-safe default", "Persistence toggle"
**Files:** `deploy/trygalle/templates/secret.yaml`, `deploy/trygalle/templates/statefulset.yaml`, `deploy/trygalle/templates/rbac-role.yaml`, `deploy/trygalle/templates/rbac-rolebinding.yaml`, `deploy/trygalle/templates/rbac-clusterrole.yaml`, `deploy/trygalle/templates/rbac-clusterrolebinding.yaml`, `deploy/trygalle/ci/ci-existing-secret-values.yaml`, `deploy/trygalle/ci/ci-rbac-values.yaml`, `.github/workflows/ci.yml`

- [ ] Write `secret.yaml`: when `secret.create`, one Secret with `stringData` for the two Telegram keys (quote the numeric allowed user) and one key per `providerEnv` entry.
- [ ] Write `statefulset.yaml`: one replica, `serviceName` = fullname (no Service object), image from values (tag defaults to `.Chart.AppVersion`), `podSecurityContext.fsGroup: 1000`, one PVC volume with two mounts (`/data/pi/sessions` subPath `sessions`, `/data/pi/work` subPath `work`; both mounts absent when persistence is disabled), env wiring: the three credential names via `valueFrom.secretKeyRef` (chart Secret or `secret.existingSecret` with `secret.keys` names; provider keys by their own key name), `TRYGALLE_SESSION_DIR` from `trygalle.sessionDir`, `extraEnv` as literal entries, `args` = `trygalle.args`, ServiceAccount reference, and `extraVolumes`/`extraVolumeMounts`/`extraContainers`/`extraInitContainers` rendered with `toYaml` so the four documented passthroughs are not inert.
- [ ] Write the four RBAC templates: `range` over each list, render via `toYaml` with `mergeOverwrite` supplying default `metadata.name` (fullname-derived) and default `metadata.namespace` (`.Release.Namespace`) for namespaced kinds only — user-provided fields win; empty lists render nothing.
- [ ] Add `ci-existing-secret-values.yaml` (`secret.create: false`, `secret.existingSecret: trygalle-creds`, `secret.keys` names, one `providerEnv` entry, plus dummy `telegram.allowedUser`) and `ci-rbac-values.yaml` (one Role fragment without metadata, one RoleBinding, one ClusterRole, one ClusterRoleBinding, one dummy `providerEnv`, plus dummy `telegram.token` and `telegram.allowedUser`). Every fixture must carry dummy credentials because `secret.create` defaults to true and the schema rejects the empty defaults.
- [ ] Add the `chart` job to `.github/workflows/ci.yml`: checkout with `fetch-depth: 0`, `azure/setup-helm` at the current release, `actions/setup-python` (Yamale and yamllint need it), `helm/chart-testing-action`, `ct lint --config ct.yaml` (lints once per `ci/` fixture), then `helm template -f <fixture>` per fixture with short `rg` assertions — default fixture: both subPath mounts, `serviceAccountName`, `TRYGALLE_SESSION_DIR: /data/pi/sessions`, the three `secretKeyRef` entries, Secret keys, PVC with the configured size, and the passed-through volume and container; existing-secret fixture: no `kind: Secret`, `secretKeyRef` name `trygalle-creds`, provider key read under its environment name; RBAC fixture: all four kinds present with defaulted names, `rules` carried through, and no `metadata.namespace` on the ClusterRole or ClusterRoleBinding; persistence: `-f .../ci-default-values.yaml --set persistence.enabled=false` renders no PVC and no `/data/pi` mounts, and `--set persistence.existingClaim=my-claim` renders `claimName: my-claim` with no PVC; schema: `helm template deploy/trygalle --set secret.create=true --set telegram.token=` exits non-zero with an error naming `values.schema.json`.
- [ ] Run `ct lint --config ct.yaml` and every `helm template` + assertion command locally; expect green.
- [ ] Run `go test -race -short ./...`; expect green.
- [ ] Commit with `feat: chart secret, statefulset, rbac renderers, and ci chart job`.

### Slice 3: Publishing and documentation
**Delivers:** every release-warranting merge to `main` produces a tag, release notes, and a two-platform image after the smoke gate, plus the chart-testing lint gate and the documentation that makes the image usable. User feedback: the release workflow goes green on the next push to `main` (workflow file in the PR), and the docs read correctly.

### Task 5: semantic-release workflow with smoke gate
**Delivers:** every push to `main` runs semantic-release; a published release pushes `v<version>` and `latest` as one `linux/amd64,linux/arm64` manifest, and nothing is pushed when no release is warranted. `release.yml` parses and grants `contents: write` plus `packages: write`.
**Blocked by:** Task 1 and Task 2
The workflow builds the slice-1 Dockerfile; the smoke script reads its pin.
**Traces to:** Requirements "Automatic release from conventional commits" and "Multi-platform publishing"; design Publishing section (with the approved deviation recorded in the brief)
**Files:** `.github/workflows/release.yml`, `.releaserc.json`

- [ ] Write `.releaserc.json`: `branches: ["main"]`; plugins `@semantic-release/commit-analyzer` with `releaseRules: [{type: "chore", release: "patch"}]`, `@semantic-release/release-notes-generator`, `@semantic-release/github`; no publish plugin, because the image push is the release artifact.
- [ ] Write the workflow: on `push` to `main` only (no tag trigger); `permissions: contents: write, packages: write`; `concurrency` keyed on the ref with `cancel-in-progress: false`; steps: checkout with `fetch-depth: 0` (semantic-release needs every commit and tag), QEMU, buildx, `cycjimmy/semantic-release-action` with `GITHUB_TOKEN`, then, gated on `new_release_published == 'true'`, GHCR login with `GITHUB_TOKEN`, build + load a single-platform image, `scripts/image-smoke.sh` on it (the script compares the Dockerfile npm literal with `harness.PinnedPiVersion` and the in-image `pi --version`, so the workflow holds no version literal), and `docker/build-push-action` pushing `linux/amd64` and `linux/arm64` tagged `ghcr.io/maruina/trygalle:v${{ steps.semantic.outputs.new_release_version }}` and `ghcr.io/maruina/trygalle:latest`.
- [ ] Validate the file: `python3 -c 'import yaml; yaml.safe_load(open(".github/workflows/release.yml"))'`; expect clean exit.
- [ ] Prove the version mapping without publishing: run the dry run from the repository root with the analyzer, notes, and GitHub plugins plus a token, for example `GITHUB_TOKEN=$(gh auth token) npx --yes -p semantic-release -p @semantic-release/commit-analyzer -p @semantic-release/release-notes-generator -p @semantic-release/github semantic-release --dry-run --no-ci`, once with a `chore(deps):` probe commit and once without it; expect a patch release and no release respectively. Record the resolved version and the first-release version the empty tag set produces in this plan. If the dry run cannot resolve the repository state (no `package.json`, token lacks push scope on the remote), record the blocker and verify the mapping at the first release instead.
- [ ] Run `go test -race -short ./...`; expect green.
- [ ] Commit with `ci: release images with semantic-release on main`.

### Task 6: Documentation
**Delivers:** a reader can run the bot from the image or the chart without touching the repository, and can customize it along the design's five paths.
**Blocked by:** Task 1, Task 3, Task 4
The documentation covers the image, entrypoint, and chart behavior.
**Traces to:** Requirement "Run and customization documentation"; design Documentation section
**Files:** `README.md`, `docs/customize.md`, `AGENTS.md`

- [ ] README: add a Run section above the source-run instructions — `docker run` with the three inputs and a volume for `/data/pi`, then `helm install` with a minimal values file; recommend the versioned image tag over `latest`; keep the throwaway-bot section as-is.
- [ ] Create `docs/customize.md` with one recommended path each: agent-directory extensions/skills/themes (mount + `PI_CODING_AGENT_DIR`, `npm install` inside the mounted path, the laptop-extension Linux-compatibility limit), `models.json` plus provider env, a derived image (`FROM ghcr.io/maruina/trygalle`), the four RBAC lists with the parent design's restricted `ClusterRole` as the worked example, and the project-trust warning (agent directory needs no trust; project-directory resources need the non-interactive `defaultProjectTrust` setting). Document what happens with a missing provider key in an existing Secret (loud non-zero startup exit).
- [ ] AGENTS.md: add a short image-and-chart section — `docker build -t trygalle:smoke . && scripts/image-smoke.sh trygalle:smoke`, `ct lint --config ct.yaml` with the chart-testing install prerequisite, the `helm template -f` commands, the note that CI installs the current Helm and chart-testing releases, the release model (semantic-release on `main`; `feat` minor, `fix` and `chore` patch, other types no release; Renovate must emit `chore(deps):` titles), and the complete pin-surface list (now including `Dockerfile`).
- [ ] Run `go test -race -short ./...`; expect green.
- [ ] Commit with `docs: add run quickstart, customization guide, and image instructions`.

### Task 7: Manual acceptance (operator: Matteo)
**Delivers:** the design's manual acceptance evidence, recorded in this file.
**Blocked by:** Task 5 and Task 6
Manual acceptance needs a published image (Task 5) and the documentation (Task 6).
**Traces to:** the design's Testing strategy (manual acceptance items) and Rollout section
**Files:** `plans/pi-environment-and-image/plan.md` (ledger only)

- [ ] Merge the feature PR; expect the release workflow to create the tag and release notes that the dry run predicted, and to publish exactly two image tags: `v<version>` and `latest`.
- [ ] `docker manifest inspect ghcr.io/maruina/trygalle:v<version>`; expect both `linux/amd64` and `linux/arm64` listed.
- [ ] Sync `Chart.yaml` `version` and `appVersion` to the released version in a `chore(release):` commit, so `image.tag: ""` resolves to an existing image; chart publication as an OCI artifact stays deferred.
- [ ] Credential rejection: run the container with a valid-format but revoked Telegram token against the real API; expect the Telegram rejection logged and a non-zero exit instead of retrying.
- [ ] Poll conflict: run two containers against the same token; expect the poll-conflict fatal exit on one of them.
- [ ] Talos install: `helm install trygalle ./deploy/trygalle -f <real-values>` with the parent design's restricted `ClusterRole` supplied through the four RBAC lists; expect a running pod writing sessions to the PVC and no Kubernetes API activity beyond that.
- [ ] Agent-directory mount: mount a test-extension directory and set `PI_CODING_AGENT_DIR`; expect Pi to load the extension.
- [ ] Record the results under `### Slice 3 execution notes` and commit with `docs: record pi-environment-and-image manual acceptance`.

### Task 8: Confirm Renovate is enabled (operator: Matteo)
**Delivers:** the single-bump-PR mechanism actually runs, instead of only being configured.
**Blocked by:** None
This task runs in parallel with Task 2.
**Traces to:** Requirement "Single-PR Pi pin coverage", scenario "renovate runs"; design Renovate decision
**Files:** none (GitHub App installation; `.github/renovate.json5` unchanged)

- [ ] Record the current gap: `.github/renovate.json5` landed in commit `0fc985d` (2026-11-11), but `gh issue list --search 'Dependency Dashboard'` returns nothing, no `renovate/*` branch exists, and no Renovate-authored pull request exists.
- [ ] Install the Renovate GitHub App for `maruina/trygalle` if it is absent, then wait for onboarding; expect a `Dependency Dashboard` issue.
- [ ] Expect the first update pull request to carry a `chore(deps):` title, which semantic-release maps to a patch release.
- [ ] If the app is installed but no dashboard appears, record the observed configuration (onboarding disabled, repository outside the app's scope) instead of assuming the automation works.

### Task 9: Final verification
**Delivers:** the feature-level acceptance gate that only the completed slices satisfy.
**Blocked by:** Task 1, Task 2, Task 3, Task 4, Task 5, Task 6, Task 7, Task 8
**Traces to:** the design's Requirements (all)
**Files:** none (verification only)

- [ ] `scripts/image-smoke.sh` green against a rebuild of the final `Dockerfile` (Requirements: credential failure, toolchain, non-root, session override; the script also proves the Dockerfile pin matches `harness.PinnedPiVersion`).
- [ ] `ct lint --config ct.yaml` plus all fixture template assertions green (Requirements: default install, existing secret, RBAC, schema rejection, persistence including the existing claim).
- [ ] The `ci.yml` `image` and `chart` jobs are green on the feature pull request, running the same commands as Tasks 1 and 4.
- [ ] The semantic-release dry run from Task 5 reports the version the release run published (Requirements: automatic release).
- [ ] Renovate pattern grep green and a Renovate-authored dependency pull request exists (Requirement: pin coverage, including the "renovate runs" scenario).
- [ ] `go test -race -short ./...` green across the whole feature branch.
- [ ] Feature-level criteria that only manual acceptance can prove (published manifest, real Telegram/provider behavior, Talos rollout, extension load) live in Task 7 and do not block code-slice delivery, per the design's validation limits.

---

## Validation limits

- arm64 is built and manifest-listed but never runtime-exercised: the smoke script runs on the native CI platform (amd64), and the manifest check is the only arm64 evidence. Accepted per the review session's multi-arch smoke gap; revisit when an arm64 runner is available.
- The live contract suite runs against a host Pi binary and does not exercise the image; no containerized live test exists. Accepted: the image changes no protocol behavior.
- Real Telegram delivery, real provider calls, and the cluster rollout stay manual (Task 7). Accepted: CI stays secret-free.
- The kubectl checksum is verified at build time only; runtime users inherit the pinned binary as-is.
- With `secret.existingSecret`, the schema cannot verify that the referenced Secret contains the required keys (Helm cannot read cluster Secrets); the guarantee is the documented loud startup failure, not a render-time check.
- `helm lint` does not read a chart's `ci/` directory, so `ct lint --config ct.yaml` is the lint gate over the fixtures and every `helm template` assertion passes its fixture with `-f`; the chart's bare default values intentionally fail the schema (Key Decision 5), so `helm lint` or `helm template` without fixture values is not a supported invocation.
- `ct install` is not run: the chart needs real Telegram and provider credentials at runtime, so a fixture-driven kind install exits non-zero on the missing token. Chart installation stays manual acceptance (Task 7).
- Helm and chart-testing are not version-pinned: CI installs the current releases and the local verification used Helm v4.3.1. A release that changes lint or schema behaviour is a plan amendment, not a silent drift.
- `Chart.yaml` `version` and `appVersion` are bumped deliberately after a release instead of by the workflow, because chart publication is deferred; a user installing a chart revision whose `appVersion` is not synced must pass `--set image.tag=<release>`.
- The commit-type-to-version mapping is proven by `semantic-release --dry-run` plus the first real release; a squash title that does not follow the conventional format silently publishes nothing.

## Learning candidates

- 2026-11-14: `helm lint` does not merge a chart's `ci/` fixture directory; that directory is a chart-testing convention, and `ct lint --config ct.yaml` is the tool that lints every `ci/` fixture. The plan's original lint mechanism and every lint step were wrong; they now use chart-testing with `check-version-increment: false`. — evidence: on a scratch chart whose `values.yaml` violated its own `values.schema.json`, `helm lint <chart>` exited 1 while a valid `ci/ci-default-values.yaml` existed, and an invalid `ci/` fixture was ignored (Helm v4.3.1); `ct lint` documents one lint per `ci/*-values.yaml`; `maruina/go-infrabin` PR #475 sets the same flag.
- 2026-11-14: Tags pushed by a workflow's default `GITHUB_TOKEN` do not trigger another workflow, so release artifacts must be built in the same job that runs semantic-release. — evidence: `maruina/go-infrabin` PR #474 design rationale; adopted in Task 5 of this plan.
- 2026-11-14: A configured Renovate file does not mean Renovate runs. `.github/renovate.json5` has existed since commit `0fc985d` with no Dependency Dashboard issue, no `renovate/*` branch, and no Renovate pull request. — evidence: `gh issue list --repo maruina/trygalle --search 'Dependency Dashboard'` returned nothing and `gh api repos/maruina/trygalle/branches` lists no `renovate/*` branch; Task 8 gates the release automation on a real dependency pull request.
