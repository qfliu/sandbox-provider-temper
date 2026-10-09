# Summary

**Start here.** A five-minute read; everything else is linked.

Isolated Linux sandboxes for AI agents, on Kubernetes. An agent with an API
key creates a sandbox, runs commands in it, moves files in and out, opens
URLs to apps it starts there, and can SSH in. Each sandbox is an Ubuntu pod
under **gVisor** (a user-space kernel, the default) or, per sandbox,
**Kata Containers** (a lightweight VM with its own kernel, on x86 nodes with
nested virtualization). It runs on a Mac (minikube, gVisor only) and on
Azure AKS, where every end-to-end suite passes under both runtimes.

## What was asked, and where it's answered

| Ask | Answer | Details |
|---|---|---|
| Sandboxes for agents on Kubernetes, strongly isolated | gVisor or Kata per sandbox; an admission policy Kubernetes enforces even against our own API; network policies that allow DNS and the public internet only | [README](README.md#architecture), [WRITEUP](WRITEUP.md#main-trade-offs) |
| Docker inside sandboxes: build, run, compose with ports, a recent version; "check gVisor first" | `ubuntu-docker` image, Docker Engine 29 with Compose and Buildx; works under gVisor (with changes) and under Kata | [WRITEUP](WRITEUP.md#main-trade-offs), [DESIGN D61–D64](DESIGN.md) |
| Kata or Firecracker: "your call, just explain the tradeoffs" | Kata built beside gVisor, chosen per sandbox; Firecracker argued, not built | [WRITEUP](WRITEUP.md#main-trade-offs) |
| Optional features: build what matters, say why | port URLs built; streaming output, secrets, custom images ranked as next | [WRITEUP](WRITEUP.md#optional-features-what-agents-need-most-d66) |
| A dashboard to interact (baseline UI) | create, run a command, SSH command to copy, delete; API keys; admin view | [README](README.md#architecture) |
| About 10 at once with p50/p95; argue the rest from the design | measured on AKS under both runtimes; scaling to thousands argued part by part | [below](#headline-numbers), [WRITEUP](WRITEUP.md#scaling-to-thousands-d67) |
| Runs on a cloud's Kubernetes; macOS is fine for local | `./cloud-up.sh` builds it all on Azure AKS in ~15 minutes; `./up.sh` on a Mac | [README](README.md#quickstart-macos) |

## Headline numbers

10 sandboxes created at once on AKS (2 × 4-vCPU x86 nodes), create request
to `running`, 3 runs each (D70):

| Runtime, image | p50 | p95 |
|---|---|---|
| gVisor, `ubuntu-base` | 3.0–3.3 s | 3.7–4.3 s |
| gVisor, `ubuntu-docker` (Docker answering) | 3.6–4.0 s | 4.0–5.9 s |
| Kata, `ubuntu-base` | 5.5–5.7 s | 6.1–6.8 s |
| Kata, `ubuntu-docker` (Docker answering) | 16.6 s | 17.7–19.6 s |

On a laptop one sandbox reaches `running` in 1.1 s. Kata with Docker in bulk
is CPU-bound on small nodes (Docker starting in 10 VMs at once); bigger
nodes or a warm pool fix it.

## The runtime decision

gVisor is the default: it starts fastest, costs nothing extra per sandbox
and runs on any node, including a laptop. Kata is the stronger boundary (an
escape must break out of a VM) and runs nearly anything Linux runs, for
about 1 s more to start alone (about 2.5 s more at 10 at once) and ~160 MiB more memory per sandbox; the caller
picks it with `"runtime": "kata"`. Firecracker would mean building our own
VM orchestration for gains (density, pause/resume) v1 doesn't need, and Kata
can switch to it later by configuration.

## How to run it

```bash
./up.sh      # a Mac: minikube VM with gVisor, images, the platform, an admin key
./demo.sh    # a narrated walkthrough
./down.sh    # stop the VM
```

This runs everything except Kata. **Why not Kata on a Mac:** the
Kubernetes node there is itself a VM, so Kata would have to start its VMs
inside it (nested virtualization). Apple allows that only on M3 or newer
chips with macOS 15+, and minikube doesn't turn it on; on the M1 this was
built on, macOS reports it as unsupported (D74). Kata runs, and was tested,
on the Azure cluster's x86 nodes.

For Azure, see [README § On Azure](README.md#on-azure-aks).

## The documents

- [README.md](README.md): quickstart, architecture, API, measurements, Azure.
- [WRITEUP.md](WRITEUP.md): trade-offs, optional features, scaling, limits, next steps.
- [TESTING.md](TESTING.md): the guide for testers.
- [docs/REFERENCE.md](docs/REFERENCE.md): detail moved out of the above (the main scripts, full API notes, every measurement, tester access, other clusters, the full scaling walkthrough).
- [DESIGN.md](DESIGN.md): the full decision log (D1–D75), for reference.
