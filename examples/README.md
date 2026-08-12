# Demo manifests

Five pods that intentionally fail in different ways, so you can see kwatch's crash
diagnosis in action without needing a real cluster or a broken app. Each has been run
against a fresh [kind](https://kind.sigs.k8s.io/) cluster to confirm it produces the
expected diagnosis block.

| Manifest | Triggers | Diagnosis you'll see |
|---|---|---|
| `crashloopbackoff.yaml` | Container exits 1 on every start | `CRASH DETECTED [Error]` / `[CrashLoopBackOff]` with the container's stderr |
| `oomkilled.yaml` | Container allocates more memory than its limit | `CRASH DETECTED [OOMKilled]` |
| `imagepullbackoff.yaml` | References a nonexistent image | `CRASH DETECTED [ErrImagePull]` / `[ImagePullBackOff]` (events only — the container never ran, so there are no logs) |
| `init-crashloopbackoff.yaml` | Init container exits 1 | `CRASH DETECTED [Init:Error]` / `[Init:CrashLoopBackOff]` with the init container's stderr |
| `silent-restart.yaml` | Liveness probe fails, kubelet restarts the container — the pod itself stays `Running` the whole time | A `↺ restarted (N → N+1)` line, then a `RESTART DETECTED` block — this is the case a plain `kubectl get pods --watch` gives you no visual signal for at all |

## Prerequisites

- [Docker](https://docs.docker.com/get-docker/)
- [kind](https://kind.sigs.k8s.io/docs/user/quick-start/#installation) — runs a real
  Kubernetes cluster inside a Docker container, no cloud account needed
- `kubectl`
- kwatch itself: `make install` from the repo root (see the main [README](../README.md))

## Run it

```bash
# From the repo root
kind create cluster --name kwatch-demo

kubectl apply -f examples/

# In another terminal
kwatch
```

Within about 30–45 seconds you should see all five pods cycle through their failure
states and kwatch print a diagnosis block for each. `demo-crashloopbackoff`,
`demo-oomkilled`, and `demo-init-crashloopbackoff` keep restarting on a loop, so you'll
see repeated blocks — that's expected, not a bug in the demo.

To watch just one scenario in isolation:

```bash
kwatch pods demo-oomkilled
```

## Clean up

```bash
kubectl delete -f examples/
kind delete cluster --name kwatch-demo
```
