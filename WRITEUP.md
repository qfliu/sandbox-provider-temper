# Write-up: trade-offs and next steps

The choices behind the platform, where it stops, and what production would
add. IDs in parentheses (D4, …) point into the decision log,
[DESIGN.md](DESIGN.md); longer versions of some sections are in
[docs/REFERENCE.md](docs/REFERENCE.md).

## Reading of the brief

- **Users are agents** outside the sandbox, using an HTTP API with an API key
  and, optionally, SSH. People use the same SSH path and a dashboard (D5).
- **Strong isolation** means whatever runs in a sandbox is assumed hostile to
  the host, the cluster, other sandboxes and the platform.
- **Durable** means the platform: users, keys and sandbox records survive
  restarts and statuses stay accurate. A sandbox's files don't survive the
  loss of its pod (D8).
- **Startup matters**: the target is about a second for one sandbox on a
  warm node (met on a laptop; ~3 s with gVisor and ~4 s with Kata on AKS),
  with no image download on the critical path.

## Compared with hosted providers

Against E2B, Modal and Daytona (their documentation, October 2026), this
has the core: create, exec with exit codes and timeouts, files, SSH,
lifetime and idle limits, per-user quotas, API keys and an admin view,
gVisor or VM isolation, Docker inside, URLs to ports, internet with private
networks blocked.

It doesn't have: snapshots or pause/resume, streaming output and background
processes in the API, custom images, secrets at create, per-sandbox network
and CPU/memory settings, a richer file API, volumes, SDKs, metrics and
webhooks. The full table is in
[REFERENCE](docs/REFERENCE.md#compared-with-hosted-sandbox-providers-the-full-table).
Snapshots were left out on purpose: the 2026-10-08 follow-up made Docker
inside sandboxes the requirement, and Kubernetes has no way to restore a
gVisor checkpoint into a new pod.

## Main trade-offs

**Two runtimes, chosen per sandbox: gVisor by default, Kata for a VM
boundary (D4, D60, D64, D65).** The follow-up asked for Docker inside
sandboxes, said gVisor "struggles there, so check that first", and left
Kata or Firecracker to us. We tested gVisor first (it works, with the
changes in D61–D62), then added Kata beside it.

| | **gVisor** (`gvisor`) | **Kata + Cloud Hypervisor** (`kata`) | **Firecracker directly** (not built) |
|---|---|---|---|
| Boundary | a user-space kernel between the sandbox and the host kernel | a VM with its own Linux kernel | a microVM, as Kata |
| Runs on | any node, including arm64 and a laptop | x86 nodes with nested virtualization | the same as Kata |
| 10 at once on AKS, p50 / p95 (D70) | 3.0–3.3 s / 3.7–4.3 s (Docker: 3.6–4.0 / 4.0–5.9 s) | 5.5–5.7 s / 6.1–6.8 s (Docker: 16.6 / 17.7–19.6 s) | not measured (VM boot ~125 ms, published) |
| Cost per idle sandbox | ~90 MiB host memory, no declared overhead | ~250 MiB, +250m CPU / 130 Mi declared | lower than Kata |
| Docker inside | works: a separate runtime class, 6 extra capabilities, no ICMP NAT | works with a real kernel; capabilities stay inside the VM | expected, not tested |
| What it took | a node installer we wrote (D54) | the official installer, one Helm chart | our own VM orchestrator instead of pods: weeks |

- **Why both:** gVisor is fast, free per sandbox and runs everywhere, so
  it's the default. Kata is the stronger boundary and runs nearly anything
  Linux runs, for about 1 s more to start alone (2–2.5 s more at 10 at
  once, much more for Docker sandboxes started in bulk on small nodes) and
  ~160 MiB more memory. The caller picks per sandbox.
- **Why not Firecracker:** its strengths (density, pause/resume) aren't v1
  requirements, and using it directly means replacing Kubernetes pods with
  our own VM orchestration. Kata can switch to it later by configuration.
- **Kata's cost in defence in depth:** the VM is the only boundary (no
  seccomp or AppArmor in the guest), so the admission policy blocks the pod
  annotations that could reconfigure the VM.

**The other choices**, one line each:
- **Defence in depth** (D37, D45): an admission policy enforced by
  Kubernetes even against our own API, NetworkPolicies, least-privilege
  Postgres roles and pinned host keys, each with attack tests.
- **Control path over SSH, not an in-sandbox daemon** (D7): sshd gives exec,
  streaming and SFTP for free; a purpose-built daemon like E2B's `envd` is
  the upgrade.
- **One SSH gateway, not a port per sandbox** (D6): stateless, scales
  behind one address; not a DDoS shield.
- **Own pod layer, not `agent-sandbox`** (D27): a few hundred lines of
  client-go we control; its CRD is worth it later for warm pools.
- **State in Postgres, not Kubernetes objects:** quotas and history need
  transactions; a pod watch plus a reconcile keep statuses accurate.
- **Go** (D11): the language of client-go and `x/crypto/ssh`, one binary for
  the API and the gateway.
- **Dashboard rendered on the server** (D50, D69): no JavaScript build;
  create, run a command, delete, keys, admin view.

## Optional features: what agents need most (D66)

Ranked by how stuck an agent is without them:
1. **URLs for sandbox ports: built.** An agent that runs `docker compose up`
   needs to check the result from outside, and a reviewer needs to see it.
   `/v1/sandboxes/{id}/ports/{port}/` proxies HTTP and WebSockets over the
   sandbox's own SSH connection, so no new network path opens. Next:
   subdomain URLs with signed links for browsers.
2. **Streaming output: next.** `exec` returns when the command ends; builds
   and test runs benefit from seeing output as it comes. SSH already
   streams, so this is an API transport choice.
3. **Secrets: after that.** Today agents pass tokens through `exec` or a
   file; a proper version stores them encrypted and mounts them per sandbox.
4. **Custom images: last.** `ubuntu-docker` already builds and runs any
   image inside; booting user root images needs scanning and policy, and
   loses the pre-pull.

## Scaling to thousands (D67)

Measured at the scale asked for (10 at once, table above); the rest is
argued from the design. No part keeps state another replica couldn't
rebuild from Postgres and the Kubernetes API. The full walkthrough, with
the arithmetic, is in [REFERENCE](docs/REFERENCE.md#scaling-to-thousands-the-full-walkthrough-d67).

- **Nodes are the real cost.** On 16-vCPU nodes about 60 gVisor or 30 Kata
  sandboxes fit per node by CPU request, so 1,000 live sandboxes is roughly
  17 gVisor or 34 Kata nodes. The cluster autoscaler adds nodes; pre-pull
  puts images on them; a warm pool would hide both.
- **Kubernetes API:** a create is three writes. Our own client-side rate
  limit was the first ceiling (5/s); it's now 50/s, which took the create
  request from 2–4 s to 0.1 s at 10 at once (D68).
- **API and gateway replicas** are stateless and scale with request
  volume.
- **Postgres** sees a few hundred small writes per second at 10,000 live
  sandboxes: one modest instance, made highly available.
- **Beyond one cluster:** more clusters ("cells"), with Postgres recording
  where each sandbox lives; every call already carries the sandbox ID.

**What breaks first at 1,000 creates at once:** node capacity (sandboxes
wait `pending` for new nodes), then creates held inside HTTP requests (~17/s
per replica; a queue fixes it), then the reconcile's full LIST every 30 s
(leader election and an informer fix it), then the control plane tier.

## Reliability and startup

API and gateway run 2 replicas each and drain on shutdown; Postgres is one
replica on a volume (managed HA Postgres is the production step). Expired
or idle sandboxes are reaped every 30 s, with a pod deadline as backstop.
Startup was brought down by a 1 s readiness probe, a smaller
ephemeral-storage request, pre-pulled images (without them a fresh AKS
node waits 7.6–9.3 s for `ubuntu-dev`), the rate-limit fix (for bursts)
and, under Kata, Docker's nftables backend (27 → 16.6 s at 10 at once,
D70). One sandbox reaches `running` in 1.1 s on a laptop. Past the
cluster's capacity, creates stay `pending` with no reason shown yet (D48).

## Known limits

- No persistent disks or pause/resume: the workspace is lost with the pod.
- One SSH key per sandbox; SSH certificates would add expiry and revocation,
  and admin SSH (admins reach others' sandboxes through the API only, D39).
- The dashboard cookie holds the API key itself (HttpOnly, 12 h; D40).
- The API is plain HTTP inside the cluster; on AKS one Caddy replica adds
  HTTPS, so its restart is a brief outage (D57).
- Install with `helm upgrade --install` only: generated secrets rely on
  `lookup`, which GitOps rendering doesn't run (D52).
- Three fixed images; Docker's storage is a 2 GB tmpfs, so very large builds
  run out.
- Port URLs need the key in a header, so browsers can't open them directly.
- No organizations or teams; logs only, no metrics yet.

## What running it on Azure found

`./cloud-up.sh` builds everything in about 15 minutes and every suite passes
there under both runtimes. The cloud found what the laptop couldn't: AKS's
hybrid containerd config silently ignored gVisor (D56); sandboxes could
reach the public Kubernetes API address until the chart blocked it (D56);
small nodes fit 7 sandboxes, not 10; and Docker under Kata needed its own
cgroup, capability and nftables setup (D64, D70, D71).

## Next steps

1. Cluster autoscaler and a cold-node load test.
2. Managed Postgres as a chart option.
3. A warm pool.
4. Pause/resume and persistent workspaces.
5. An in-sandbox daemon instead of exec over SSH, plus streaming output.
6. SSH certificates.
7. Tailscale, so the platform needs no public address (D16).
8. Firecracker, if density or pause/resume become requirements.

## How it was built

A step per pull request, each logged in DESIGN.md and checked by a separate
reviewer agent, with attack-style tests for every security boundary. The
end-to-end suites (API, gateway, hardening, network policy, Docker, port
URLs, load, pre-pull and five realistic agent workflows) run under both
runtimes on AKS; running an agent's workflow end to end found a bug the
feature suites missed (D71). CI runs the Go tests with Postgres, Helm,
Terraform and script checks (D59).
