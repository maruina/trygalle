# Trygalle high-level design
Status: Approved

Date: 2026-08-13

Scope: High-level product and system design. Detailed design belongs in the follow-up brainstorms defined below.

## Summary
Trygalle is a personal Telegram-to-Pi bridge. It runs Matteo's existing Pi harness inside a Talos Kubernetes cluster and exposes that harness through a single-user Telegram bot.

The beta focuses on cluster operations such as inspecting pod health, reading pod logs, and restarting Deployments. Trygalle does not interpret or authorize those operations itself. Pi reasons about the request and invokes its existing tools; Kubernetes Role-Based Access Control (RBAC) determines whether the pod may perform the resulting operation.

Trygalle is not a new agent platform. Pi remains the reasoning and execution engine and continues to own extensions, skills, prompts, settings, context, tools, and sessions. Trygalle owns Telegram adaptation, request coordination, Pi subprocess lifecycle, and response delivery.

The repository is public. Private harness configuration and credentials remain external runtime inputs.

## Problem
Matteo wants to operate his Kubernetes cluster remotely through the same Pi harness he uses on his personal and work laptops. Existing agent platforms would introduce another runtime and configuration model instead of reusing his current extensions, skills, prompts, and operating knowledge.

The first useful version needs to provide remote access without creating a generic multi-user agent service or granting unrestricted Kubernetes privileges.

## User and ownership
Matteo is the sole beta user, operator, and maintainer. Trygalle is personal infrastructure without a formal service-level objective or on-call rotation.

The public repository owns the generic bridge, deployment examples, and documentation. The private dotfiles repository owns Matteo's Pi configuration and harness resources.

## Goals
- Forward Telegram text from one allowed user to the existing Pi harness and return Pi's final response.
- Support `/new`, `/status`, and `/abort` as Trygalle control commands.
- Forward every other slash-prefixed message unchanged so Pi can invoke extension commands, prompt templates, and skills through its normal command syntax.
- Reuse the existing Pi extensions, skills, prompts, settings, and context without teaching Trygalle about individual resources.
- Resume the latest persisted Pi session after a pod restart; use `/new` for an explicit fresh conversation.
- Allow cluster-wide pod inspection, pod log access, and Deployment restarts through explicit, restricted Kubernetes RBAC.
- Keep the source repository safe to publish.
- Preserve clear component boundaries so later Telegram media inputs can still use Pi as the main engine.
- Emit enough application and Pi subprocess logs to diagnose beta failures.

## Non-goals
- Replacing Pi or implementing a second reasoning or tool-execution engine
- A generic agent, workflow, or plugin platform
- Multiple Telegram users, per-user routing, or multiple Pi sessions
- Parallel prompts or a request queue
- Application-level authorization of natural-language operations
- `cluster-admin` access
- Screenshot or voice-message handling in the beta
- Interactive Pi extension user interfaces over Telegram in the beta
- A user-facing `/commands` catalog in the beta
- Telegram webhooks, streaming message edits, or a Kubernetes Service or Ingress
- A database, dashboard, automatic session expiry, or internal process supervisor
- Metrics, distributed tracing, dashboards, alerts, or formal availability objectives in the beta
- A general prompt-injection defense or approval workflow
- Helm, Talos CLI, Flux, Argo CD, GitHub CLI, SOPS, or other tools not required by the beta

## Known future use cases
The design must leave room for these inputs without implementing them now:

- Send a screenshot and ask Pi to transform it into a note.
- Send a voice message for processing by Pi.
- Add metrics and distributed tracing.
- Bridge Pi extension dialogs to Telegram messages, replies, or inline buttons.
- Expose a user-facing command catalog discovered from Pi.

These use cases test the separation between Telegram adaptation, request coordination, and the Pi runtime. They do not justify a generic plugin framework. Future media support may preprocess or normalize input, but Pi remains the main reasoning and execution engine.

## Context reviewed
The repository currently contains only a short `README.md`, an MIT `LICENSE`, and `.gitignore`; there is no application architecture to preserve.

The design was checked against the installed Pi documentation for:

- RPC commands, responses, events, JSONL framing, command discovery, and extension UI requests
- Session storage and continuation
- Containerization
- Project trust and Pi's lack of a built-in sandbox

Relevant confirmed Pi behavior:

- RPC uses line-feed-delimited JSON over standard input and output.
- A successful `prompt` response means Pi accepted the prompt; it does not mean processing finished.
- `agent_settled` indicates that retries, compaction retries, and queued continuations have settled.
- `get_commands` reports loaded extension commands, prompt templates, and skills.
- Skill and prompt commands are expanded by Pi when sent through `prompt`.
- Built-in interactive-only TUI commands are not exposed by `get_commands` and do not execute through RPC prompts.
- Pi extension dialogs use a separate RPC request/response protocol and can block an extension until answered or timed out.
- RPC mode does not display an interactive project-trust prompt.
- Pi has no built-in sandbox and inherits the permissions and credentials of its process environment.

Inspection of the current private harness confirmed that extension UI dialogs are a real compatibility concern, not only a theoretical protocol feature. Private resource names and paths are intentionally omitted from this public design.

### Advisory learnings
The advisory learning **Separate remote API connectivity, authentication, and authorization** informed the security model: Kubernetes connectivity, ServiceAccount authentication, and RBAC authorization remain distinct controls.

Two other matched learnings—**Derive untrusted identity into storage paths and persist full mutable state** and **Check slices/maps stdlib before hand-rolling sort/clone helpers, but verify nil semantics first**—did not materially affect this high-level design.

## Skills loaded and used
| Skill | Source | Why loaded | How used |
|---|---|---|---|
| `obsidian-cli` | `prompt-required` | Read advisory engineering learnings | Retrieved complete matched learning sections before confirming the security framing |
| `codebase-research` | `agent-selected` | Establish the repository baseline and authoritative Pi behavior | Verified the repository state, reviewed Pi documentation, and inspected current harness interaction patterns |
| `write` | `agent-selected` | Produce a concise, reviewable public design | Separated goals, constraints, decisions, risks, and deferred details |

## Assumptions
- The private dotfiles repository is trusted.
- A valid Node.js environment is sufficient to load the current Pi extensions, skills, and prompts.
- The private harness can be mapped into Pi's normal user configuration location without changing Trygalle's code.
- The container's installed tools, mounted credentials, network access, and RBAC may differ from a laptop even when the Pi harness configuration is identical.
- Model credentials can be supplied through a Kubernetes Secret or another Pi-supported authentication mechanism without entering the public repository or container image.
- Telegram, the model provider, the Git host, and the Kubernetes API are external dependencies.
- One StatefulSet replica and one active Pi operation are sufficient for the personal beta.
- Updated dotfiles may require a pod restart.

## System context
```text
Allowed Telegram user
        |
        | Telegram Bot API (long polling)
        v
Trygalle Go process
  - Telegram transport
  - request coordinator
  - Pi process owner
        |
        | strict JSONL RPC over stdin/stdout
        v
Long-lived Pi process
  - existing extensions, skills, prompts, and tools
  - persisted session state
        |
        | kubectl and other installed tools
        v
Kubernetes API
  - ServiceAccount authentication
  - restricted cluster-wide RBAC authorization
```

A private dotfiles checkout supplies Pi's harness at startup. A PersistentVolumeClaim stores Pi sessions independently of the ephemeral checkout and pod filesystem.

## Component responsibilities
### Telegram transport
The Telegram transport:

- Uses long polling.
- Receives updates while Pi is working so `/abort` remains available.
- Silently ignores messages whose numeric user ID does not match the configured allowlist.
- Distinguishes Trygalle's reserved commands from messages that must pass through to Pi.
- Delivers final responses and control-command results to Telegram.
- Does not interpret the operational intent of a prompt.

### Request coordinator
The request coordinator:

- Allows one active Pi operation.
- Rejects new prompts and `/new` as busy while an operation is active.
- Allows `/status` and `/abort` while an operation is active.
- Associates Telegram work with Pi RPC request identifiers and lifecycle events.
- Keeps only transient operation state in memory; it does not add a database or durable queue.

The exact state machine, timeout policy, and synchronization mechanism belong in the Pi runtime and Telegram follow-up designs.

### Pi runtime boundary
The Pi runtime boundary:

- Starts one Pi subprocess for the lifetime of the Trygalle process.
- Owns Pi's standard input, standard output, and standard error streams.
- Sends supported RPC commands with request IDs.
- Continuously decodes strict JSONL from standard output without a 64 KiB line limit.
- Treats standard output as protocol data and logs standard error separately.
- Queries `get_commands` after startup to confirm that Pi discovered harness resources.
- Observes Pi events until an accepted prompt or command has actually completed.
- Exits Trygalle if Pi exits unexpectedly, allowing Kubernetes to restart the pod.

Trygalle does not implement Pi command expansion, skill selection, tool execution, compaction, or session-file parsing as competing sources of behavior.

### Existing Pi harness
Pi loads the private harness through its standard configuration mechanisms. The harness remains the source of:

- Extensions and custom tools
- Skills
- Prompt templates
- Settings and model configuration
- Context files and durable operating guidance

Trygalle must not hardcode a list of these resources. Startup command discovery confirms that resource loading works; beta acceptance compares the returned inventory with the expected harness externally rather than embedding private names in the public application.

### Kubernetes authorization boundary
Pi invokes `kubectl` through its existing shell tooling. The pod uses an in-cluster ServiceAccount and does not mount a personal kubeconfig.

The initial authorization model is cluster-scoped but restricted by resource and verb. It must support at least:

| Resource | Verbs | Purpose |
|---|---|---|
| Deployments | `get`, `list`, `watch`, `patch` | Inspect and restart Deployments |
| Pods | `get`, `list`, `watch` | Inspect pod health across namespaces |
| Pod logs | `get` | Read pod logs |

A `ClusterRoleBinding` grants the `ClusterRole` to the dedicated ServiceAccount. Trygalle does not pre-approve commands or convert authorization failures into application policy. The Kubernetes API accepts or rejects each operation according to the configured RBAC.

The exact API groups, subresources, and final rule set belong in the Kubernetes deployment and security follow-up design.

## Startup flow
1. Kubernetes schedules the single StatefulSet pod.
2. The init container mounts a read-only SSH deploy key and clones the trusted private dotfiles repository into a shared `emptyDir` volume.
3. A clone failure prevents the main container from starting.
4. The deploy key is not mounted into the main container.
5. The main container maps the checkout into Pi's normal user configuration location through a small, configurable bootstrap mechanism.
6. Trygalle starts Pi in RPC mode with the PersistentVolumeClaim-backed session directory and requests continuation of the latest session.
7. Trygalle calls `get_state` and `get_commands` to validate basic RPC operation, session state, and harness discovery.
8. Trygalle begins Telegram long polling only after Pi is ready to accept commands.

The exact checkout mapping, project-trust setting, resume command, and readiness mechanism are deferred to their focused designs.

## Prompt flow
1. The Telegram transport receives a text message.
2. Trygalle compares the sender's numeric user ID with the configured allowlist.
3. Unauthorized messages receive no response.
4. Trygalle classifies an exact reserved control command or treats the entire message as Pi input.
5. If another Pi operation is active, Trygalle rejects a new prompt as busy.
6. Otherwise Trygalle sends a `prompt` RPC command with the Telegram text unchanged.
7. The prompt response confirms acceptance or reports immediate rejection.
8. Trygalle continues consuming Pi events through retries, compaction, tool calls, and subsequent turns until the operation is settled.
9. Trygalle extracts the authoritative final assistant text and sends it to Telegram.
10. Delivery failures are logged and do not cause an automatic second Pi execution.

Exact behavior for extension commands that complete without an agent run, empty assistant responses, Telegram formatting, and Telegram message-size limits belongs in the Pi runtime and Telegram follow-up designs.

## Slash commands and command discovery
Trygalle reserves only these Telegram commands:

| Telegram command | Pi RPC action |
|---|---|
| `/new` | `new_session` |
| `/status` | `get_state` |
| `/abort` | `abort` |

Every other message, including a message beginning with `/`, passes unchanged through Pi's `prompt` RPC command. Pi then performs its normal extension-command dispatch, prompt-template expansion, or explicit skill expansion such as `/skill:name`.

Trygalle uses `get_commands` for startup validation. The beta does not expose `/commands` and does not dynamically mirror Pi commands into Telegram's command menu. Telegram command-name restrictions do not need to match Pi's syntax because Trygalle reads and forwards the raw message text.

Trygalle's reserved names take precedence if the private harness defines commands with the same names.

### Extension UI behavior
The beta does not bridge Pi's interactive extension UI to Telegram.

- Blocking `select`, `confirm`, `input`, and `editor` requests are answered immediately as cancelled so an extension cannot hang indefinitely.
- The cancellation is logged without logging sensitive dialog content.
- Fire-and-forget extension UI notifications are not guaranteed to become Telegram responses in the beta.

This preserves safe process progress but does not provide full interactive parity with the laptop TUI. Telegram dialog support is a separate future feature.

## Control flows
### `/new`
When no operation is active, Trygalle sends `new_session` to the existing Pi process. It does not restart Pi. The new session becomes the session that will resume after a later pod restart.

When an operation is active, Trygalle returns a busy response rather than queuing or implicitly aborting work.

### `/status`
Trygalle sends `get_state` and returns a small human-readable summary. `/status` remains available during active work. The exact fields and wording are deferred to the Telegram interface design.

### `/abort`
Trygalle sends `abort` for the active Pi operation and reports the result. Telegram polling must therefore continue independently of the active operation. Abort completion and races with natural completion belong in the Pi runtime design.

## Session and process lifecycle
- One Pi process runs for the lifetime of the pod's main process.
- Pi's automatic compaction remains enabled.
- Pi session files live under `/data/pi/sessions` on a PersistentVolumeClaim.
- Pod restart resumes the latest session rather than silently starting a fresh conversation.
- `/new` is the only beta mechanism for intentionally starting a fresh conversation.
- Trygalle stores no duplicate conversation state in a database.
- An unexpected Pi exit is fatal to Trygalle; Kubernetes, not an internal supervisor, restarts the pod.
- The single-replica design avoids concurrent writers to the active Pi session.

The exact definition of “latest,” handling of an incomplete session after abrupt termination, and shutdown sequencing require focused design and tests.

## Container and bootstrap architecture
One application image contains:

- The Trygalle Go binary
- Pi
- The Node.js runtime required by Pi and current extensions
- Bash
- `kubectl`
- Git
- Curl
- `jq`
- Certificate authorities

The image uses a multi-stage build so the Go compiler is absent from the runtime image. Major tool and dependency versions are pinned rather than following floating `latest` versions. The Trygalle binary is the main process and owns Pi as a child process.

An init container clones the private dotfiles repository on each pod startup into `/bootstrap/dotfiles` on a shared `emptyDir`. The main container can read the checkout but cannot access the SSH deploy key used to clone it. Updated dotfiles take effect after a pod restart.

The exact base image, version pins, runtime user, filesystem permissions, Node dependency installation, working directory, project trust, and mapping into `~/.pi/agent` belong in the Pi environment and image design.

## Kubernetes deployment architecture
The beta uses:

- One-replica StatefulSet
- PersistentVolumeClaim mounted for `/data/pi/sessions`
- Shared `emptyDir` for the startup dotfiles checkout
- Git-cloning init container
- Dedicated ServiceAccount
- Restricted `ClusterRole` and `ClusterRoleBinding`
- Kubernetes Secrets for the Telegram token, Git deploy key, model credentials, and other sensitive runtime configuration

Telegram long polling removes the need for a Service or Ingress. No personal kubeconfig is mounted.

The deployment namespace does not limit the ServiceAccount's authorized resource scope; the `ClusterRole` intentionally supports the approved cluster-wide troubleshooting use cases.

## Public repository and data handling
The repository is treated as public from the first change.

The repository, image, tests, examples, and documentation must not contain:

- Telegram bot tokens or allowed user IDs
- Git deploy keys or private repository URLs
- Model credentials or Pi authentication files
- Personal kubeconfigs
- Cluster names, endpoints, certificate data, or other personal infrastructure identifiers
- Private dotfiles or copied private harness content

Example manifests use obvious placeholders. Secret values enter only through deployment-time configuration. The Git deploy key is read-only and scoped to the private repository.

Application logs must not intentionally include credentials, complete prompt bodies, complete tool output, or extension dialog contents. The detailed logging and redaction policy belongs in the operability design because Telegram messages and Pi output may still contain sensitive operational data.

## Observability and operability
The beta uses logs as its diagnostic surface. Trygalle logs enough lifecycle metadata to understand:

- Configuration and startup failures
- Dotfiles bootstrap consequences visible to the main process
- Pi process start and exit
- Session resume and reset outcomes
- RPC command rejection and protocol failures
- Request start, completion, abort, and busy rejection
- Unsupported extension UI cancellation
- Telegram polling and response-delivery failures

Pi standard error is logged separately from its protocol standard output. Request identifiers should connect Telegram handling and Pi RPC lifecycle messages without exposing message content.

Metrics, traces, dashboards, alerts, and formal service-level objectives are deferred. The component boundaries leave room to instrument the Telegram transport, coordinator, and Pi runtime independently later.

## Failure behavior
| Failure | High-level behavior |
|---|---|
| Dotfiles clone fails | Init container fails; main container does not start |
| Required configuration or secret is missing | Trygalle fails startup with a non-secret diagnostic |
| Pi cannot start, resume, or answer basic RPC validation | Trygalle exits; Kubernetes restarts the pod |
| Pi exits unexpectedly | Trygalle logs the exit and exits |
| Unauthorized Telegram user sends a message | Ignore silently |
| Prompt arrives while Pi is active | Return busy; do not queue |
| Unsupported blocking extension UI appears | Cancel immediately and log metadata |
| Kubernetes denies an operation | Pi observes and reports the authorization failure |
| Model or tool execution fails | Pi's final failure response is returned when available |
| Telegram response delivery fails | Log the failure; do not repeat the Pi operation automatically |
| Telegram polling fails transiently | Follow the Telegram library's bounded retry behavior; finalize in the Telegram design |
| RPC framing or decoding fails | Treat as a process/protocol integrity failure; exact shutdown behavior belongs in the Pi runtime design |

## Security model
The beta uses layered but intentionally small controls:

1. **Telegram identity:** accept messages only from one configured numeric user ID.
2. **Secret handling:** inject bot, Git, and model credentials at runtime; commit none of them.
3. **Credential separation:** expose the Git deploy key only to the init container.
4. **Container boundary:** run Pi and all extensions inside the pod rather than on a personal laptop.
5. **Kubernetes identity:** use a dedicated in-cluster ServiceAccount.
6. **Kubernetes authorization:** grant explicit cluster-wide resource and verb permissions, not `cluster-admin`.
7. **Single active operation:** prevent parallel prompts and uncontrolled in-process fan-out.
8. **Public-source hygiene:** keep personal configuration and infrastructure details outside the repository.

Pi extensions execute with the same operating-system permissions as Pi. Project trust is an input-loading guard, not a sandbox. Prompt injection and model mistakes remain possible within the permissions granted to the pod.

## Tradeoffs and explicit downsides
The chosen design has real costs:

- A cluster-wide Deployment `patch` permission has meaningful blast radius even without `cluster-admin`.
- The single process and single active operation create head-of-line blocking and no availability during restart.
- Tight dependence on Pi's RPC and extension protocols makes Pi version compatibility an ongoing maintenance concern.
- Startup depends on the Git host, private repository, Pi configuration, model credentials, and Telegram.
- Resuming one long conversation can preserve stale context despite automatic compaction; `/new` remains a manual operator decision.
- Automatically cancelling extension dialogs prevents hangs but reduces compatibility with interactive laptop workflows.
- Keeping observability to logs makes intermittent latency and retry problems harder to analyze than with metrics and traces.
- Loading trusted private extensions gives those extensions every permission available to the pod.

These downsides are accepted for a personal beta because they keep the implementation small and preserve the central goal of reusing Pi.

## Alternatives considered
### Adopt Hermes, OpenClaw, or another remote agent platform
Merit: these systems may already provide remote messaging, media handling, session management, and operational integrations.

Decision: rejected because their runtime and configuration model would not make Matteo's existing Pi harness the primary engine.

### Use Pi's TypeScript SDK
Merit: direct typed APIs would avoid subprocess framing and may track Pi's internal types more closely.

Decision: rejected for the beta because application logic should remain in Go and Pi's documented RPC mode already provides the required process boundary.

### Start one Pi process per Telegram message
Merit: process isolation would simplify per-request cleanup and reduce long-lived process state.

Decision: rejected because it would repeatedly initialize the harness, complicate session continuity, and conflict with the desired long-lived Pi runtime.

### Use Telegram webhooks
Merit: webhooks can reduce polling latency and fit horizontally scaled services.

Decision: rejected because they require externally reachable networking, a Service or Ingress, and request authentication infrastructure that a one-replica personal bot does not need.

### Add a generic Trygalle handler or plugin framework now
Merit: a plugin system could isolate future media and workflow features behind stable extension APIs.

Decision: rejected because Pi already provides the extension model. Clear internal responsibilities are enough to add input adapters without creating a second plugin ecosystem.

### Restrict Kubernetes access to one namespace
Merit: namespaced RBAC would materially reduce the blast radius of model mistakes or compromised credentials.

Decision: rejected because the approved beta needs cluster-wide troubleshooting and Deployment restarts. The narrower compromise is cluster-wide scope with explicit resources and verbs.

### Add metrics and tracing in the beta
Merit: telemetry would make latency, retries, process churn, and provider failures easier to diagnose.

Decision: deferred because logs are sufficient for an initial personal deployment and telemetry would add configuration and operational dependencies before the bridge is proven useful.

### Bridge all extension UI through Telegram in the beta
Merit: interactive dialogs would provide closer parity with the laptop harness and make more extension commands usable remotely.

Decision: deferred because selections, confirmations, free-form replies, timeouts, and stale Telegram interactions introduce a separate conversation-state problem. Immediate cancellation prevents hangs while preserving a focused beta.

## Risks and mitigations
| Risk | Mitigation |
|---|---|
| Telegram bot or allowed account is compromised | Rotate the token, revoke access, and remove or scale down the workload; rely on restricted RBAC for containment |
| Model mistakes or prompt injection trigger unwanted operations | Restrict Kubernetes resources and verbs; do not grant `cluster-admin` or mount personal credentials |
| Private credentials enter the public repository | Use placeholders, automated secret scanning where available, and runtime Secrets only |
| Pi protocol changes break the bridge | Pin Pi's major version, test the consumed RPC contract, and keep protocol handling narrow |
| Pi hangs on an extension dialog | Cancel unsupported blocking extension UI requests immediately |
| Telegram or Pi emits a response larger than supported | Define bounded splitting or truncation in the Telegram design |
| Session resume selects the wrong or damaged session | Define deterministic resume and recovery behavior in the Pi runtime design |
| Logs expose operationally sensitive content | Log lifecycle metadata by default and define payload/redaction rules before implementation |
| Dotfiles update breaks startup | Require a pod restart to adopt changes and roll back the referenced revision or image/configuration |
| Cluster-wide patch permission is abused | Restrict the rule to Deployments and required verbs; remove the binding for immediate containment |
| Single active work item blocks urgent input | Keep `/abort` and `/status` available while rejecting additional work rather than building an unbounded queue |

## Beta success criteria
The beta is successful when:

- The allowed Telegram user can send a plain-text operational request and receive Pi's completed answer.
- The user can inspect pods across namespaces, read pod logs, and restart a Deployment when RBAC permits it.
- Unauthorized Telegram users receive no response.
- A second prompt receives a busy response while Pi is active, while `/status` and `/abort` remain usable.
- `/new` creates a fresh Pi session without restarting Pi.
- A pod restart resumes the latest session from `/data/pi/sessions`.
- Pi loads the private harness through its normal configuration mechanisms, and `get_commands` exposes the expected extension commands, prompts, and skills.
- A non-reserved slash command reaches Pi unchanged.
- An unsupported extension dialog cannot block Pi indefinitely.
- Pi failure causes the pod to restart rather than leaving a live but unusable bot.
- The deployed ServiceAccount can do no more than its reviewed RBAC grants.
- Public source and built artifacts contain no private configuration or credentials.
- Logs are sufficient to identify which boundary failed during manual beta testing.

## Testing strategy
Detailed test design belongs in the five focused brainstorms, but the high-level strategy requires evidence at each boundary:

- **RPC contract:** exercise prompt acceptance, completion, command discovery, state, new session, abort, extension UI cancellation, malformed output, and process exit against a controlled Pi process or protocol fixture.
- **Telegram behavior:** verify allowlist rejection, reserved-command routing, raw slash-command pass-through, busy behavior, and response-delivery failures without contacting a real user in routine tests.
- **Harness compatibility:** start the built image with a trusted test harness and verify `get_commands` reports expected resource categories; manually validate the private harness before cluster rollout.
- **Session lifecycle:** prove new-session behavior, clean restart resume, and abrupt process termination recovery against a temporary session directory.
- **RBAC:** use Kubernetes authorization checks and representative `kubectl` operations to prove required access and denied out-of-scope access.
- **Public-repository safety:** scan tracked files and built image metadata for credentials, private URLs, and personal cluster data.
- **End to end:** manually run pod-health inspection, pod-log retrieval, Deployment restart, `/status`, `/abort`, `/new`, and pod restart in the Talos cluster.

## Rollout and rollback
The initial rollout is limited to Matteo's personal Talos cluster and one bot identity. The implementation should be exercised locally against Pi before receiving Kubernetes credentials, then validated with the restricted ServiceAccount.

Rollback remains simple:

- Revert to the previous application image and public configuration.
- Revert or pin the previous trusted dotfiles revision when a harness update breaks startup.
- Preserve the session PersistentVolumeClaim unless session data itself is the failure source.
- Remove the `ClusterRoleBinding` for immediate Kubernetes-access containment.
- Scale the StatefulSet to zero or revoke the Telegram token to stop remote access.

No database or schema migration complicates rollback. Session-format compatibility across Pi versions must be checked before changing the pinned Pi version.

## Follow-up brainstorms
Each follow-up should produce its own focused design rather than expanding this document into an implementation plan.

### 1. Pi runtime and RPC
Scope:

- Process startup, shutdown, and fatal exit behavior
- Strict JSONL reader and writer boundaries
- Request IDs, responses, events, and operation state
- Prompt and slash-command completion semantics
- `agent_settled`, retries, compaction, and empty responses
- `get_commands` startup validation
- Extension UI cancellation and fire-and-forget requests
- Session resume, `/new`, abort races, and recovery
- Timeouts, malformed protocol data, and tests

Suggested invocation:

```text
/brainstorm Pi runtime and RPC in plans/high-level-design/design.md
```

### 2. Telegram interface
Scope:

- Go Telegram library selection and long-polling lifecycle
- Numeric user allowlist
- Reserved-command parsing and raw slash-command pass-through
- Busy, status, abort, and error responses
- Formatting, message-size limits, splitting, and delivery retries
- Future command discovery and extension UI interactions
- Future screenshot and voice-message adaptation seams

Suggested invocation:

```text
/brainstorm Telegram interface in plans/high-level-design/design.md
```

### 3. Pi environment and image
Scope:

- Runtime base image and pinned Go, Node.js, Pi, and tool versions
- Multi-stage build and runtime user
- Private dotfiles checkout mapping into Pi's standard configuration
- Node dependencies for existing extensions
- Working directory and non-interactive project trust
- Model authentication and filesystem permissions
- Harness compatibility validation

Suggested invocation:

```text
/brainstorm Pi environment and image in plans/high-level-design/design.md
```

### 4. Kubernetes deployment and security
Scope:

- StatefulSet, volumes, PersistentVolumeClaim, and init container
- Read-only Git deploy key isolation
- ServiceAccount, exact `ClusterRole`, and `ClusterRoleBinding`
- Secret injection and security context
- Resource requests and limits
- Startup, readiness, and liveness behavior
- Immediate containment and rollback procedures

Suggested invocation:

```text
/brainstorm Kubernetes deployment and security in plans/high-level-design/design.md
```

### 5. Operability and beta acceptance
Scope:

- Log events, levels, correlation, payload policy, and redaction
- Manual and automated acceptance evidence
- Dependency-failure diagnostics
- Deployment validation and rollback rehearsal
- Pi and dotfiles upgrade procedure
- Future metrics and tracing boundaries

Suggested invocation:

```text
/brainstorm Operability and beta acceptance in plans/high-level-design/design.md
```

## Open questions
The high-level architecture is approved. The following questions remain intentionally unresolved until their owning brainstorm:

- Which exact Pi version and RPC behaviors form the supported contract?
- How does a slash command that performs no agent run signal completion and produce a Telegram response?
- How is the latest resumable session selected after clean and abrupt restarts?
- What bounded timeouts apply to Pi, Telegram polling, model calls, and shutdown?
- Which fire-and-forget Pi extension UI requests should become Telegram messages?
- How are long, formatted, empty, or partially failed Telegram responses represented?
- What exact dotfiles mapping and project-trust configuration loads the harness non-interactively?
- Which runtime user and filesystem permissions satisfy Pi, extension, and PVC requirements?
- What are the final `ClusterRole` API groups, resources, subresources, and verbs?
- What log metadata is useful without retaining sensitive prompt or tool content?
- What evidence gates a Pi, Node.js, `kubectl`, or dotfiles upgrade?

## Self-review notes
The design was reviewed skeptically against the approved scope.

Material findings incorporated:

- “Forward slash commands” was insufficient without command discovery and extension UI behavior; `get_commands` validation and immediate dialog cancellation are now explicit.
- “Persistent sessions” was insufficient without restart continuation; resume-latest behavior and recovery questions are now explicit.
- “One request at a time” would have made `/abort` unusable if polling stopped; polling and control commands remain available during active work.
- A namespaced `Role` would not satisfy the approved cluster-wide use case; the design now states the deliberate `ClusterRole` tradeoff.
- Public-repository constraints now cover examples, tests, image metadata, logs, and private harness references, not only committed Secret manifests.

Material suggestions rejected or deferred:

- Adding metrics and tracing now would improve diagnosis, but logs are sufficient to validate a personal beta before adopting telemetry dependencies.
- Full Telegram extension UI would improve harness parity, but it introduces durable interaction state and is not required to prove text-to-Pi operation.
- A generic future-input plugin system would make extension points explicit, but Pi already owns extensibility and simple component boundaries avoid a second framework.

No blocking high-level design issue remains. The unresolved questions are bounded by the five follow-up brainstorms.