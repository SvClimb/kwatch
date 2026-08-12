# kwatch

[![CI](https://github.com/svclimb/kwatch/actions/workflows/ci.yml/badge.svg)](https://github.com/svclimb/kwatch/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

`kubectl get --watch` with color output and automatic crash diagnosis.

Feels like kubectl — same flags, same muscle memory. Adds colorized status, and when a pod falls into an error state, automatically surfaces logs and events inline without leaving the watch stream.

---

## Install

**From source (requires Go 1.25+):**

```bash
git clone https://github.com/svclimb/kwatch
cd kwatch
make install
```

The binary is placed in `$GOPATH/bin` (usually `~/go/bin`). Make sure it's in your `$PATH`.

---

## Usage

```
kwatch [resource] [name] [flags]
```

Default resource is `pods`. All flags mirror kubectl so there's nothing new to learn.

### Examples

```bash
# Watch pods in the current namespace
kwatch

# Exit after 10 minutes regardless of state
kwatch deploy my-service --timeout=10m

# Watch a specific resource type
kwatch deployments
kwatch statefulsets
kwatch jobs

# Namespace and context
kwatch -n production
kwatch --context staging-eu

# Label selector
kwatch -l app=payment-service
kwatch -l app=api,env=prod

# Specific pod
kwatch pods payment-service-abc123 -n production

# All namespaces
kwatch -A

# Wide output
kwatch -o wide

# Combine flags freely — same as kubectl
kwatch deployments -n production --context prod-eu -o wide
```

---

## Flags

| Flag | Short | Default | Description |
|---|---|---|---|
| `--namespace` | `-n` | — | Kubernetes namespace |
| `--context` | — | — | kubeconfig context |
| `--kubeconfig` | — | — | Path to kubeconfig file |
| `--selector` | `-l` | — | Label selector |
| `--field-selector` | — | — | Field selector |
| `--all-namespaces` | `-A` | false | Watch across all namespaces |
| `--output` | `-o` | — | Output format: `wide`, `json`, `yaml`, `name` |
| `--color` | — | `auto` | Color output: `auto`, `always`, `never` |
| `--diagnose` | — | `true` | Show logs and events when a pod enters an error state |
| `--diagnose-delay` | — | `15s` | Grace period before diagnosing (transient errors that recover within this window are ignored) |
| `--log-lines` | — | `50` | Number of log lines to fetch per diagnosis |
| `--timeout` | — | `0` | Exit after this duration (0 = watch indefinitely) |
| `--version` | — | — | Print version and exit |

`json`, `yaml`, and `name` print a single snapshot and exit — they don't stream a live
watch, so they're safe to pipe into `jq` or another tool without hanging. `wide` (and the
default table output) stream continuously, like `kubectl get --watch`.

---

## Color output

Statuses are colorized for quick scanning:

| Status | Color |
|---|---|
| `Running` | Green |
| `Pending`, `ContainerCreating`, `PodInitializing` | Yellow |
| `Init:*` | Cyan |
| `Terminating` | Magenta |
| `Completed` | Gray |
| `Error`, `CrashLoopBackOff`, `OOMKilled`, `ImagePullBackOff`, … | Red bold |

Color mode is auto-detected from the terminal — disabled automatically when piping to a file or another command. Override with `--color=always` or `--color=never`.

---

## Crash diagnosis

When a pod enters an error state and stays there past the grace period, kwatch fetches logs and events and prints them inline:

```
──────────────────────────────────────────────────────────────────
 ⚠  CRASH DETECTED: payment-service-abc123  [CrashLoopBackOff]
──────────────────────────────────────────────────────────────────
 ▶ LOGS (previous container):
   [INFO]  Starting payment-service v2.3.1
   [ERROR] Failed to connect to postgres://db.internal:5432/payments
   [ERROR] dial tcp: lookup db.internal: no such host
   [FATAL] Cannot start without database connection. Exiting.

 ▶ EVENTS:
   LAST SEEN   TYPE      REASON    OBJECT                        MESSAGE
   5s          Warning   BackOff   pod/payment-service-abc123    Back-off restarting failed container
──────────────────────────────────────────────────────────────────
```

**Deduplication:** statuses in the same family (`Error` + `CrashLoopBackOff` + `OOMKilled` = `crash` family) produce a single block per error cycle, not one per event.

**Transient errors:** if a pod recovers within `--diagnose-delay` (default `15s`), the diagnosis is silently cancelled — no noise from health checks that pass on the second try.

**Recovery:** once a pod stays `Running` for the full grace period, its slate is cleared — a future crash will be diagnosed fresh.

**Silent restarts:** kwatch also watches the `RESTARTS` column. If a pod's restart count increases without ever showing an error status in `STATUS` (e.g. a failing `startupProbe` that recovers by itself), kwatch prints a one-line notice and, no more than once per grace period, a full diagnosis block from the previous container.

Disable entirely with `--diagnose=false`. Reduce noise during deploys with a longer delay: `--diagnose-delay=60s`.

Want to see it live without a real cluster or a broken app? [`examples/`](examples/) has
five ready-to-apply manifests (CrashLoopBackOff, OOMKilled, ImagePullBackOff,
Init:CrashLoopBackOff, and a silent-restart case) plus instructions for running them on
a local [kind](https://kind.sigs.k8s.io/) cluster.

---

## Shell completion

Generate a completion script for your shell:

```bash
# zsh — add to ~/.zshrc
source <(kwatch completion zsh)

# bash — add to ~/.bashrc
source <(kwatch completion bash)

# fish
kwatch completion fish | source

# PowerShell
kwatch completion powershell | Out-String | Invoke-Expression
```

**What gets completed:**

- First argument — resource type: `pods`, `deployments`, `services`, `secrets`, `configmaps`, `nodes`, and their short aliases (`po`, `deploy`, `svc`, …)
- `--namespace` / `-n` — live namespaces from the current cluster
- `--context` — all contexts from your kubeconfig

---

## Development

```bash
make build    # build to bin/kwatch
make install  # install to $GOPATH/bin
make test     # run tests with race detector
make lint     # go vet + gofmt check
make clean    # remove bin/
```

Tests cover the diagnoser's timing and deduplication logic, color mapping, and output formatting. No live cluster required.

See [CONTRIBUTING.md](CONTRIBUTING.md) for the full contribution guide.

---

## Requirements

- Go 1.25+
- A kubeconfig file (`~/.kube/config` or `$KUBECONFIG`)

---

## Required RBAC

`--diagnose` (enabled by default) fetches pod logs and events via the Kubernetes API. The user running kwatch needs these permissions in the target namespace:

```yaml
rules:
  - apiGroups: [""]
    resources: ["pods/log"]
    verbs: ["get"]
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["get", "list"]
```

If permissions are missing, kwatch will still detect the crash but show an empty diagnosis block with no logs or events. Use `--diagnose=false` to disable if you don't have these permissions.

---

## Known limitations

**Non-pod resources:** `--diagnose` only works when watching pods. Running `kwatch deployments --diagnose` is valid but diagnosis will never trigger — deployments have no `STATUS` column in the format kwatch parses.

**Multi-container pods:** kwatch picks a container automatically — those with a known error state, a non-zero exit code, or a prior restart are tried first, then the rest — and shows the first one with non-empty log output. If more than one container is crashing at the same time, only that first container's logs are shown; run `kubectl logs <pod> -c <container>` to see a different one.

---

## License

[MIT](LICENSE)
