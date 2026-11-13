# Pi environment and image design

Status: Draft (pending design comprehension review)

Date: 2026-11-13

Parent design: `plans/high-level-design/design.md`, follow-up brainstorm #3 (Pi environment and image).

## Summary

This design defines the public, out-of-the-box runtime surface of Trygalle: one multi-stage Docker image containing the Trygalle binary, Pi, Node.js, and the command-line tools Pi needs, a Helm chart that installs the image on any Kubernetes cluster, a GitHub Container Registry publishing workflow, and the documentation that turns the image into a working Telegram-to-Pi bot with three user-supplied inputs.

Out of the box, Pi runs with its default harness: no extensions, no private skills, and an empty agent directory. The private dotfiles checkout, its chezmoi rendering, and the init container that maps it into Pi's configuration remain deferred to a separate sidecar design. This design prepares for that work with three seams but implements none of it.

## Problem

Trygalle's code is complete for the beta bridge behavior, but no runnable public artifact exists. An external user cannot turn the repository into a working bot without assembling Node.js, the pinned Pi release, and the compiled binary themselves. The approved high-level design's container architecture describes intent, not an implementable spec: it names no base image, no version pins, no runtime user, no working directory, and no credential-injection mechanism.

The beta also deferred Helm as a non-goal, so there is no defined path to install Trygalle on a Kubernetes cluster.

## User and audience

Open-source users who want their own single-user Telegram-to-Pi bridge, and Matteo as the maintainer who ships and operates it. Users bring a Telegram bot token, their numeric Telegram user ID, and one model-provider API key. They do not need the private dotfiles repository, and the default image works without it.

## Deviations from the high-level design

This design reverses one approved decision and adds one:

1. **A Helm chart is now a deliverable.** The parent design listed Helm under non-goals. Matteo approved this change during brainstorming: the chart is the primary Kubernetes install path, and the private-dotfiles init container from the parent's Kubernetes architecture remains deferred, so the chart intentionally ships less than that section describes.
2. **The image is published** to `ghcr.io/maruina/trygalle` from CI. The parent design did not address image distribution.

Both changes keep the parent's principles: the repository stays public and free of private configuration, Pi remains the reasoning engine, and Trygalle owns only adaptation, coordination, and delivery.

## Goals

- One multi-stage Docker image that builds the Trygalle binary and runs it with Pi 1.5.2, Node.js, `bash`, `git`, `curl`, `jq`, `ripgrep`, `ca-certificates`, and `kubectl`, with every tool and dependency version pinned.
- A container that starts with exactly three user inputs: `TRYGALLE_TELEGRAM_TOKEN`, `TRYGALLE_TELEGRAM_ALLOWED_USER`, and one provider API-key environment variable such as `ANTHROPIC_API_KEY`.
- A Helm chart with a one-replica StatefulSet, a PersistentVolumeClaim for Pi sessions, Secret-backed credential injection, no built-in RBAC objects, and four value-driven renderers the user can fill with custom `Role`, `RoleBinding`, `ClusterRole`, and `ClusterRoleBinding` manifests.
- Image publishing to `ghcr.io/maruina/trygalle` from GitHub Actions, with tags derived from Git refs.
- A Renovate-managed Pi pin: one bump PR moves the Go constant, CI, `AGENTS.md`, and the Dockerfile together.
- Documentation: a quickstart for `docker run` and `helm install`, and a customization guide for adding extensions, themes, skills, models, and custom RBAC.
- Seams for the future private-harness sidecar: a configurable agent directory, a small entrypoint hook, and chart passthroughs for extra volumes and containers.

## Non-goals

- The private dotfiles checkout, chezmoi rendering, the Git-cloning init container, and deploy-key handling (separate sidecar design)
- Readiness, liveness, and startup probes (deferred to the Kubernetes deployment and security follow-up, #4)
- Publishing the Helm chart as an OCI artifact (deferred; see Open questions)
- Media inputs, webhooks, a Service or Ingress, metrics, tracing, multi-user support (parent non-goals stand)
- A prompt-injection defense or application-level authorization (parent security model stands)

## Context reviewed

- `cmd/trygalle/main.go`: the flag surface (`--pi-bin`, `--session-dir`, `--no-continue`, `--heartbeat-interval`, `--shutdown-timeout`) and the environment surface (`TRYGALLE_TELEGRAM_TOKEN`, `TRYGALLE_TELEGRAM_ALLOWED_USER`, `TRYGALLE_TELEGRAM_POLL_TIMEOUT`) already cover everything the image needs. The binary requires `--session-dir`; no application-code change is required for this design.
- Pi documentation (`containerization.md`, `configuration.md`, `security.md`, `providers.md`):
  - The reference Pi image builds from `node:24-bookworm-slim` and installs Pi with `npm install -g --ignore-scripts @earendil-works/pi-coding-agent`.
  - Provider credentials work through environment variables (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, and others), so no `auth.json` is needed in the image.
  - The agent directory defaults to `~/.pi/agent` and can be relocated with `PI_CODING_AGENT_DIR`.
  - Agent-directory extensions, skills, prompts, and themes load without project trust. Project trust applies only to resources under the working directory; the image's working directory contains none, so no trust configuration is required on the default path.
  - RPC mode cannot show the interactive trust prompt; the customization guide documents `--approve`-equivalent configuration for users who later add project resources.
- `plans/pi-runtime-and-rpc/design.md`: Pi starts as `pi --mode rpc --continue --session-dir <dir>`; `--continue` resumes the most recent session for the current working directory, so the working directory must be a stable path across pod restarts. That design still contains one stale sentence referencing a "pinned Pi 1.3.0 path"; correcting it belongs to the parent-document maintenance, not this design.
- `.github/renovate.json5`: a custom regex manager keeps the Pi pin aligned across `internal/harness/harness.go`, `.github/workflows/ci.yml`, and `AGENTS.md`, and the full CI job runs the live suite against the new pin.
- Repository state: `go.mod` targets Go 1.27; no Dockerfile, chart, or publishing workflow exists yet.

### Advisory learnings

The learning **Renovate pins digests only through the docker datasource** applies to this design: `config:recommended` records base-image digests automatically through the `docker` datasource, so the Dockerfile should reference the Node and Go builder images by tag and let Renovate add and track digests, rather than hand-maintaining digest pins. The matched learning **Render piped chezmoi templates against an initialized config, not `--init`** concerns the deferred chezmoi sidecar; this design only records it there.

## Assumptions

- Pi 1.5.2 runs correctly as a non-root user inside the image with a writable session directory and writable working directory.
- Node.js 24 satisfies Pi 1.5.2 and the extension runtime it ships; the implementation rechecks the supported range against the pinned Pi release notes before the first image build.
- `kubectl` is the only Kubernetes tool Pi's `bash` tool needs; no Helm, `kustomize`, or cloud-provider CLIs in the beta image.
- The default model provider for out-of-the-box users is whichever provider key they inject; the image and chart are provider-agnostic and pass the variable through unchanged.
- A single PersistentVolumeClaim with subPath mounts serves both the session directory and the working directory; ReadWriteOnce is sufficient for one replica.

## Design overview

### Image

One `Dockerfile` at the repository root, multi-stage:

```dockerfile
# Stage 1: build the static Trygalle binary.
# Renovate records the digests of both base images through the docker datasource.
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -o /out/trygalle ./cmd/trygalle

# Stage 2: runtime.
FROM node:24-bookworm-slim
ARG PI_VERSION=1.5.2
ARG KUBECTL_VERSION=1.34.2
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       bash git curl jq ripgrep ca-certificates \
    && rm -rf /var/lib/apt/lists/*
# Pinned to harness.PinnedPiVersion; Renovate bumps all pin surfaces in one PR.
RUN npm install -g --ignore-scripts @earendil-works/pi-coding-agent@${PI_VERSION}
# kubectl is not in the Debian archive; install the pinned release binary and
# verify it against its published SHA256 checksum.
RUN curl -fsSL -o /usr/local/bin/kubectl \
      https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/linux/amd64/kubectl \
    && echo "$(curl -fsSL https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/linux/amd64/kubectl.sha256)  /usr/local/bin/kubectl" \
       | sha256sum --check \
    && chmod 0755 /usr/local/bin/kubectl
COPY --from=build /out/trygalle /usr/local/bin/trygalle
COPY trygalle-entrypoint /usr/local/bin/trygalle-entrypoint
# Pi's working directory and Trygalle's session directory are the two paths
# the process must be able to write; both live on the PVC in the chart, and
# the image creates them so a plain `docker run` with a bind mount also works.
RUN mkdir -p /data/pi/sessions /data/pi/work \
    && chown -R node:node /data/pi
USER node
WORKDIR /data/pi/work
ENTRYPOINT ["/usr/local/bin/trygalle-entrypoint"]
```

Decisions:

- **Base images:** `golang:1.27-bookworm` for the builder and `node:24-bookworm-slim` for the runtime, both referenced by tag with Renovate-managed digests. A distroless or Chainguard runtime was considered and rejected: Pi needs Node plus `bash`, `git`, and `jq` for the approved `kubectl` workflows, so the slimming gain is small while the image assembly cost is real.
- **Pi installation:** global npm install with `--ignore-scripts`, pinned to the exact version `harness.PinnedPiVersion` declares. The Renovate custom manager gains the Dockerfile in its `managerFilePatterns` so one bump PR moves every pin surface.
- **Tool versions:** apt-managed tools take the Debian security channel; `kubectl` is pinned explicitly with a checksum verification because it is not available from the archive and changes the pod's Kubernetes blast radius. `KUBECTL_VERSION` is pinned per image build and bumped deliberately.
- **Runtime user:** the image's `node` user (UID 1000). Pi and its extensions run with these operating-system permissions; this is the only sandbox the beta claims, matching the parent security model. The chart sets `podSecurityContext.fsGroup: 1000` so PVC-mounted paths are group-writable.
- **Working directory:** `/data/pi/work`. Because `--continue` selects sessions by current working directory, the path is fixed and PVC-backed through a `subPath` mount, so the resume set and any files Pi's tools create there survive pod restarts.
- **Multi-stage build:** the Go compiler and the build layer are absent from the runtime image, matching the parent design.

### Entrypoint

`trygalle-entrypoint` is a small POSIX shell script:

```sh
#!/bin/sh
# The future dotfiles sidecar maps the harness checkout into the agent
# directory here, before Trygalle starts Pi. deliberate: hook only; the
# sidecar design owns the mapping and this script stays minimal until then.
exec trygalle --pi-bin pi --session-dir "${TRYGALE_SESSION_DIR:-/data/pi/sessions}" "$@"
```

The environment variable `TRYGALE_SESSION_DIR` overrides the session directory without an application change; the binary keeps requiring `--session-dir`, which the script always supplies. All other Trygalle flags flow through `CMD` and chart values.

### Helm chart

The chart lives at `deploy/trygalle` with `apiVersion: v2`, one application name, and no dependencies.

Templates:

| Template | Purpose |
|---|---|
| `_helpers.tpl` | Names and labels |
| `serviceaccount.yaml` | One ServiceAccount, created by default |
| `secret.yaml` | Chart-managed Secret when `secret.create` is true |
| `statefulset.yaml` | One replica; PVC-backed `/data/pi/sessions` and `/data/pi/work` through subPath mounts |
| `pvc.yaml` | Claim when `persistence.enabled` and no `persistence.existingClaim` |
| `rbac-*.yaml` | Four renderers over user-provided lists |
| `values.schema.json` | Requires the Telegram inputs when `secret.create` is true; validates value shapes |

Key values:

```yaml
image:
  repository: ghcr.io/maruina/trygalle
  tag: ""            # defaults to .Chart.AppVersion
  pullPolicy: IfNotPresent

telegram:
  token: ""          # required unless secret.existingSecret is set
  allowedUser: 0     # required numeric user id
  existingSecret: "" # use a pre-created Secret instead of secret.create

# Arbitrary environment passed through to the container, e.g.
# ANTHROPIC_API_KEY: sk-ant-...
providerEnv: {}

trygalle:
  sessionDir: /data/pi/sessions
  args: []           # extra flags, e.g. --no-continue, --heartbeat-interval

rbac:
  serviceAccount:
    create: true
  # Each list renders user-provided objects verbatim. The chart fills in
  # defaults for name and namespace only when the user leaves them empty.
  roles: []
  roleBindings: []
  clusterRoles: []
  clusterRoleBindings: []

persistence:
  enabled: true
  size: 5Gi
  storageClass: ""
  existingClaim: ""

podSecurityContext:
  fsGroup: 1000

extraVolumes: []
extraVolumeMounts: []
extraContainers: []
extraInitContainers: []
```

RBAC decisions:

- The chart ships **no** `ClusterRole` or `Role` objects of its own. Out of the box the pod can talk to Telegram and the model provider and do nothing to the Kubernetes API, which is the fail-safe default Matteo approved.
- The four lists render each entry through `toYaml` after applying defaults, so users can paste complete manifests or minimal fragments (`rules` only, letting the chart default the name). The four lists are separate values because the parent design's security model treats namespaced and cluster-scoped grants differently.
- The StatefulSet references `rbac.serviceAccount.name`; when the user creates RBAC objects they bind them to that account in the rendered output's namespace.

The chart creates no Service, Ingress, probes, or resource-collection annotations: Telegram long polling needs no inbound networking, and probes belong to follow-up #4.

Secret handling: when `secret.create` is true, one chart-managed Secret holds `telegram.token`, `telegram.allowedUser`, and every `providerEnv` entry as stringData; the StatefulSet wires them as the exact environment names Trygalle and Pi read. When `secret.existingSecret` is set, the chart injects `valueFrom.secretKeyRef` entries for the documented key names and creates nothing.

### Publishing

`.github/workflows/release.yml`:

- On push to `main`: build the image and push `ghcr.io/maruina/trygalle:sha-<git-sha>`.
- On tag push `v*`: build and push the `v*` tag, the bare version, and `latest`.
- The workflow runs with `permissions: packages: write` and authenticates to GHCR with the workflow's GITHUB_TOKEN.

Chart consumption starts as clone-and-install (`helm install trygalle ./deploy/trygalle`); OCI chart publishing is deferred (Open questions). The workflow also runs the image smoke checks described under Testing strategy so a tag cannot publish an image whose Pi pin drifted.

### Documentation

- `README.md` gains a **Run** section: `docker run` with the three inputs and a volume for `/data/pi`, and `helm install` with a minimal values file, followed by the current throwaway-bot instructions retained for source runs.
- `docs/customize.md` covers, in one recommended path each:
  - **Extensions, skills, themes, prompts:** mount a directory at a custom agent directory and set `PI_CODING_AGENT_DIR`; document that extensions need their Node dependencies installed inside that directory (`npm install` in the mounted path or a derived image) and that laptop-specific extensions may fail on Linux.
  - **Models and provider endpoints:** `models.json` in the agent directory, plus provider environment variables.
  - **A derived image:** `FROM ghcr.io/maruina/trygalle` with baked-in harness content for users who prefer immutable images.
  - **Custom Kubernetes permissions:** the four RBAC value lists, with the parent design's restricted `ClusterRole` as the worked example.
  - **Project trust:** the agent directory needs no trust; project-directory resources require the non-interactive `defaultProjectTrust` setting or an approving override, documented with the trust-scope warning from Pi's security model.

### Sidecar seams (prepared, not built)

- `PI_CODING_AGENT_DIR` relocates the agent directory, so a future harness volume can be mounted at any path without image changes.
- The entrypoint script is the single place where the future sidecar inserts its mapping step.
- `extraVolumes`, `extraVolumeMounts`, `extraContainers`, and `extraInitContainers` passthrough values leave chart room for the init container and harness volume without schema changes.

## Smallest user-feedback slice

The committed design document is the first slice, as in the sibling designs. Implementation then lands in three reviewable slices, each independently verifiable:

1. Dockerfile, entrypoint, and Renovate manager change, proven by the CI image smoke checks.
2. Helm chart, proven by `helm lint` and `helm template` golden checks.
3. Publishing workflow and documentation.

Deliberately deferred with triggers: OCI chart publishing (needs evidence that users want no-clone installs), probes (follow-up #4), and the sidecar (separate design).

## Success criteria and validation

- A user following the README quickstart reaches a responding bot with three inputs and no repository build.
- The image contains Pi 1.5.2 and reports it through `pi --version`; one Renovate PR updates every Pi pin surface.
- `helm lint` and `helm template` pass with default and RBAC-populated test values; the rendered StatefulSet mounts both subPaths and references the ServiceAccount.
- The container starts as UID 1000, writes sessions under the mounted volume, and fails loudly on a bad Telegram token rather than hanging.
- The customization guide's mount path produces an agent directory Pi actually loads, verified manually against a test extension.

## Validation limits

- Automated CI checks prove image build, pin alignment, chart rendering, and startup failure behavior. Real Telegram delivery, real model-provider calls, and cluster rollout remain manual acceptance, consistent with the parent design's end-to-end strategy. This gap does not block delivery.
- The live contract suite runs against a host Pi binary and does not exercise the image; no containerized live test exists in this design. Accepted for the beta because the image changes no protocol behavior.
- The kubectl checksum is verified at build time only; runtime users inherit the pinned binary as-is.

## Alternatives considered

- **Distroless or Chainguard runtime base.** Merit: smallest attack surface and built-in non-root defaults. Rejected: Pi needs Node plus `bash`, `git`, and `jq` in the image for the approved workflows, so the gain shrinks while assembly and debugging cost grows. Revisit trigger: a future image-hardening pass.
- **Add an environment fallback for `--session-dir` in the Go binary.** Merit: removes the entrypoint script. Rejected: the binary's config surface is contract-tested and stable; the script supplies the flag without an application change and doubles as the sidecar hook.
- **Chart-managed default `ClusterRole` behind a toggle.** Merit: one value turns on the approved beta capability set. Rejected: Matteo chose fully user-supplied RBAC objects, which keeps the chart free of an embedded permission policy and keeps the parent's exact rule set in follow-up #4 where it belongs.
- **emptyDir instead of a PVC for sessions.** Merit: simpler chart, no storage class requirement. Rejected: it silently drops the parent design's resume-after-restart success criterion.
- **Bake the docs' extension flow into a derived image only.** Merit: immutable and reproducible. Rejected as the only path: the mount path gives faster feedback for trying an extension; both are documented, mount first.
- **Publish the chart as OCI in the same workflow.** Merit: no-clone installs. Deferred: no evidence users need it yet, and it adds a second publishing surface to maintain.

## Risks and mitigations

| Risk | Mitigation |
|---|---|
| Pi pin drifts between the image and the Go contract suite | Renovate manager covers the Dockerfile; the release workflow runs the pin smoke checks |
| Node supply-chain compromise through npm | Install with `--ignore-scripts` and an exact version pin; no runtime package installation |
| `kubectl` supply-chain compromise | Pinned version plus published SHA256 checksum verification at build time |
| PVC mount not writable by UID 1000 | Chart defaults `fsGroup: 1000`; docs cover the plain-Docker bind-mount ownership case |
| User mounts a laptop agent directory with Linux-incompatible extensions | Customization guide states the compatibility limit explicitly |
| GHCR publishing misconfigured (403 on first release) | Workflow grants `packages: write`; first tag run is verified before announcing the image |
| Model key or Telegram token leaks into image layers or logs | Secrets enter only through runtime environment; image contains no credentials; existing transport diagnostics never quote them |
| `latest` tag misleads users into silent upgrades | Quickstart pins the chart image tag; `latest` exists but docs recommend the versioned tag |

## Operability

- The image adds no new log surfaces; Trygalle's existing lifecycle logs and Pi's separated stderr carry diagnostics. Image labels (`org.opencontainers.image.*`) record source, version, and revision for image identification.
- Startup failures remain loud: missing token, malformed user ID, or unreachable Telegram exit non-zero with a non-secret diagnostic, and Kubernetes restarts the pod.
- The pinned `KUBECTL_VERSION` and Node major version are stated in the image and bumped deliberately, not through floating tags.

## Rollout and rollback

- Rollout: merge the implementation slices; publish a `v*` tag; install the chart on the Talos cluster with the parent's restricted RBAC supplied through the four value lists.
- Rollback: redeploy the previous image tag and chart revision; the PVC persists across both, so sessions survive. Removing the RBAC objects or scaling the StatefulSet to zero contains Kubernetes access immediately, matching the parent rollback ladder.
- No schema migration exists; the chart's first version is the rollback floor.

## Security and data handling

- The image and repository contain no tokens, keys, user IDs, or private harness content; example manifests use placeholders.
- Credentials enter the container only through environment variables sourced from a Secret the chart creates or the user supplies.
- The pod runs as UID 1000 without privileged mode, no host mounts, and no capabilities; Pi's effective permissions are the pod's, which is the parent design's stated boundary, not a sandbox.
- Kubernetes API access exists only when the user supplies RBAC objects; the default pod has none.
- Logs carry lifecycle metadata only; the existing redaction policy governs payload content.

## Testing strategy

- **Image smoke (CI, every build):** run the built image with no inputs and assert a fast non-zero exit with the missing-token diagnostic; run `pi --version`, `trygalle --help`, and `kubectl version --client` inside the image and assert the pinned versions.
- **Pin alignment (CI, every build):** the Renovate manager's coverage is checked by the existing full-suite pin gate plus a Dockerfile grep for the pinned Pi version.
- **Chart (CI, every build):** `helm lint`; `helm template` against a default-values fixture and an RBAC-populated fixture; assert the rendered StatefulSet mounts both subPaths and references the ServiceAccount.
- **Boot behavior (local or CI):** run the container with a syntactically valid but wrong Telegram token against the real API and assert the poll-conflict fatal exit path described in `AGENTS.md`'s composition contract.
- **Manual acceptance:** the README quickstart on a real bot; the customization guide's agent-directory mount with one test extension; the chart on the Talos cluster with the restricted RBAC example.
- **Unchanged suites:** `go test -race ./...` and the short job continue to gate code changes; this design adds no Go behavior.

## Skills loaded and used

| Skill | Source | Why loaded | How used |
|---|---|---|---|
| `skill-loader` | `prompt-required` | Route domain skills for the design area | Identified that no code files change in this design; recorded that implementation will load `script-best-practices` for the entrypoint and workflow scripts |
| `learning-lookup` | `prompt-required` | Advisory guidance before design decisions | Matched two sections; applied the Renovate digest-pinning learning to the base-image strategy and recorded the chezmoi learning against the deferred sidecar |
| `codebase-research` | `agent-selected` | Establish the current config surface and repo state | Verified the Trygalle flag/env surface, Renovate manager, sibling design contracts, and parent-design deferrals |
| `write` | `agent-selected` | Produce a concise, reviewable public design | Separated decisions, risks, validation limits, and deferred scope |

## Open questions

- Publish the chart as an OCI artifact (`oci://ghcr.io/maruina/trygalle/chart`) when users ask for no-clone installs?
- Which exact Node major track and `KUBECTL_VERSION` pin ship in the first build; the implementation rechecks Pi 1.5.2's supported Node range and the cluster's Kubernetes version before pinning.
- Does the chart need nodeSelector, affinity, or tolerations passthrough for Talos-specific scheduling? Deferred until the first cluster install shows a need.
