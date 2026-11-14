# Customize the image and chart
The image contains the pinned Pi plus Trygalle, kubectl, and the supporting tools. It carries no agent-directory content, no model configuration, and no Kubernetes permissions, so everything below is opt-in. Each section covers one recommended path; the `docker run` inputs come from the [README](../README.md).

## Add extensions, skills, and themes
Pi loads extensions, skills, prompts, and themes from its agent directory, which defaults to `~/.pi/agent` inside the container. The supported way to supply them is a mounted directory plus the `PI_CODING_AGENT_DIR` environment variable. On the chart, point `PI_CODING_AGENT_DIR` at a subPath of the session PVC, so the content survives pod restarts:

```yaml
extraEnv:
  PI_CODING_AGENT_DIR: /data/pi/agent
extraVolumeMounts:
  - name: data
    mountPath: /data/pi/agent
    subPath: agent
```

The volume named `data` is the StatefulSet's PVC volume, rendered by the chart itself.

Extensions that import npm packages need those packages installed inside the mounted directory: run `npm install` in that path on the host before the pod starts, or bake the install into a [derived image](#build-a-derived-image). Laptop-specific extensions may fail on Linux when they reference macOS paths or macOS commands; fix the paths or keep those extensions on the laptop only.

## Use a different model or provider endpoint
Provider credentials enter through environment variables, so no `auth.json` is needed. Pass one entry per provider through `providerEnv` on the chart (each key becomes both a Secret key and a container environment variable) or `-e` flags on `docker run` — for example `ANTHROPIC_API_KEY` or `OPENAI_API_KEY`.

To add a provider or point Pi at a compatible endpoint (Ollama, LM Studio, vLLM, proxies), add a `models.json` file to the agent directory from the previous section. Values in `models.json` can reference environment variables with `$NAME` interpolation, so an endpoint's `apiKey` can read the same `providerEnv` entry the container already receives.

## Build a derived image
If you prefer an immutable image over a mounted directory, build on top of the published one:

```dockerfile
FROM ghcr.io/maruina/trygalle:<release>
COPY agent/ /data/pi/agent/
ENV PI_CODING_AGENT_DIR=/data/pi/agent
```

The base image already runs as UID 1000 and keeps the pinned Pi, Trygalle, and kubectl. A derived image must not change those pins; the entrypoint and the smoke script assume they are intact.

## Grant Kubernetes access
Out of the box the pod talks to Telegram and the model provider and does nothing to the Kubernetes API: the chart renders no RBAC objects when its four `rbac.*` lists are empty. When you need kubectl capabilities inside the agent, supply objects through the four value lists — `rbac.roles`, `rbac.roleBindings`, `rbac.clusterRoles`, and `rbac.clusterRoleBindings`.

The chart renders each entry verbatim. It only defaults `metadata.name` (derived from the chart fullname) and, for namespaced kinds, `metadata.namespace` (the release namespace). The default ServiceAccount name is the chart fullname, `trygalle`.

The recommended starting point is the restricted cluster-wide role from the deployment design: inspect pods across namespaces, read pod logs, and restart a Deployment, and nothing else:

```yaml
rbac:
  clusterRoles:
    - rules:
        - apiGroups: [""]
          resources: ["pods", "pods/log"]
          verbs: ["get", "list", "watch"]
        - apiGroups: ["apps"]
          resources: ["deployments"]
          verbs: ["get", "list", "watch", "patch"]
  clusterRoleBindings:
    - roleRef:
        apiGroup: rbac.authorization.k8s.io
        kind: ClusterRole
        name: trygalle-cluster-role
      subjects:
        - kind: ServiceAccount
          name: trygalle
          namespace: trygalle
```

This renders a `ClusterRole` named `trygalle-cluster-role` (the chart default) and a `ClusterRoleBinding` named `trygalle-cluster-role-binding`, with no `metadata.namespace` on either because both kinds are cluster-scoped. The binding's `subjects` entry must carry the ServiceAccount's release namespace explicitly. Remove the binding to contain Kubernetes access immediately.

## Project trust
Resources in the agent directory load without project trust. Project trust applies only to resources under the working directory, and the image's default working directory (`/data/pi/work`) contains none, so the default path needs no trust configuration.

If you later add project-directory resources, keep one property in mind: RPC mode cannot show Pi's interactive trust prompt, so a project whose trust is unanswered blocks resource loading. Set `defaultProjectTrust` in `settings.json` in the agent directory to `"always"` for projects you have reviewed, or leave the default (`"ask"`) and expect no project resources to load until a user approves them interactively.

## When a provider key is missing
With `secret.create: false` and `secret.existingSecret` set, the chart references your Secret at render time but cannot inspect its contents, because Helm cannot read cluster Secrets. If a required key is absent — for example the Secret has no `ANTHROPIC_API_KEY` key while `providerEnv` names it — the pod starts and Trygalle exits non-zero at startup with the missing-credential error in the logs. The failure is loud and immediately visible in `kubectl logs`; fix it by adding the key to the Secret.
