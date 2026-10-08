# Write-up: trade-offs and next steps

What was built, the choices behind it, where it stops, and what production
would add. The full decision log, with every alternative considered, is
[DESIGN.md](DESIGN.md); the numbers in brackets (D4, …) point into it.

## Reading of the brief

- **Users are agents.** An agent outside the sandbox drives it through an
  HTTP API with an API key and, optionally, SSH. People use the same SSH path
  and a dashboard. Running the agent *inside* the sandbox is out of scope (D5).
- **Strong isolation** means untrusted code: assume whatever runs in a sandbox
  is hostile to the host, the cluster, other sandboxes and the platform.
- **Durable** means the *platform* is durable: users, keys and sandbox
  records survive restarts, and a sandbox's reported status is accurate.
  A sandbox's files survive process crashes but not the loss of its pod or
  node (D8). Persistent disks are the next step below, not v1.
- **Startup time matters**, so the target is about a second for one sandbox
  on a warm node (met on the laptop; on AKS ~3 s with gVisor and 4-6 s
  with Kata), and no image download on the critical path.
- **"Proper Linux desktops"** (in the baseline) means full Linux machines an
  agent can do real work on; a graphical desktop is "seeing the remote
  desktop", which the brief lists as a stretch goal (D1).

The brief's own words, and the assigner's later answers on scope, are in
DESIGN.md §2.

## Compared with hosted sandbox providers

E2B, Modal and Daytona, from their documentation (October 2026), against
what this platform has.

**What we have, as they do:**
- create, list and delete sandboxes;
- commands with exit codes and timeouts, from the API and the dashboard;
- file upload and download;
- SSH (also scp/sftp);
- lifetime and idle timeouts;
- per-user limits;
- API keys and an admin view;
- isolation: gVisor (Modal's default runtime too) or a VM per sandbox
  (Kata, as E2B's Firecracker microVMs);
- Docker inside a sandbox (build, run, compose with ports);
- URLs to a port in the sandbox (preview an app the agent starts);
- internet access with private networks blocked.

**What they have and we don't:**

| Feature | Who offers it | Here |
|---|---|---|
| **Snapshot / restore / fork, pause / resume** | all three: files everywhere; memory and running processes in E2B's default pause/resume, experimental at Modal, VM sandboxes only at Daytona | not built: the assigner's follow-up (2026-10-08) names Docker inside sandboxes, not snapshots, as the requirement. gVisor can checkpoint a running sandbox (`runsc checkpoint`); the missing part is restoring it into a new pod: Kubernetes has no restore path for gVisor checkpoints (its checkpoint API is for runc containers) |
| **Streaming command output, background processes, stdin** | all three | commands run to completion and return their output; SSH streams |
| **Custom images / templates** (your own Dockerfile, start and ready commands) | all three | three fixed images; build and run any image *inside* `ubuntu-docker` |
| **Environment variables and secrets at create** | all three | none |
| **Per-sandbox network controls** (internet off, allowlists) | all three | one policy for all |
| **Per-sandbox CPU and memory** | Modal, Daytona | fixed per image: limits of 1 CPU / 1 GiB (requests 250m / 256 MiB); `ubuntu-docker` 2 CPU / 4 GiB |
| **Richer file API** (list, stat, mkdir, delete, watch) | E2B, Daytona | single-file upload and download |
| **Tags / metadata / labels** | all three (filtered lists at E2B and Modal) | none |
| **Volumes** shared or kept across sandboxes | all three | none (D8) |
| **Terminal in the API or browser** (PTY, web terminal) | E2B, Daytona | SSH, and one-off commands in the dashboard |
| **SDKs and a CLI** | all three | HTTP API only |
| **Metrics, lifecycle webhooks, log streaming, OpenTelemetry** | E2B, Daytona | server logs only |
| **Code interpreter, desktop / computer use, warm pools, teams and SSO, MCP, git/LSP helpers** | some | none |

**Most important next** (the ranking is in "Optional features"): streaming
output and background processes, then secrets, then custom images; snapshots
only if pause/resume becomes a requirement.

## Main trade-offs

**Two runtimes, chosen per sandbox: gVisor by default, Kata for a VM
boundary (D4, D60, D64, D65).** The assigner asked for Docker inside
sandboxes and pointed at Kata or Firecracker for a real VM boundary. We
tested gVisor first, as asked, then added Kata beside it rather than
instead of it.

| | **gVisor** (`gvisor`) | **Kata + Cloud Hypervisor** (`kata`) | **Firecracker directly** (not built) |
|---|---|---|---|
| Boundary | a user-space kernel (gVisor's Sentry) between the sandbox and the host kernel | a hardware-virtualized VM with its own Linux kernel | a microVM, as Kata |
| Runs on | any node, including arm64 and a laptop | x86 nodes with nested virtualization (Azure Dv5/Dsv5; Azure's Arm sizes have none) | the same as Kata |
| Start: 10 at once, create → `running` on AKS, p50 / p95 (end to end, D70) | 3.0–3.3 s / 3.7–4.3 s (with Docker: 3.6–4.0 / 4.0–5.9 s) | 5.5–5.7 s / 6.1–6.8 s (with Docker: 16.6 / 17.7–19.6 s) | not measured (its VM boot alone is ~125 ms, published) |
| Cost per sandbox | no declared overhead; ~90 MiB of host memory idle (gVisor's own processes, counted against the sandbox's limits) | +250m CPU, +130 Mi declared; ~250 MiB of host memory idle (measured) | lower than Kata (expected) |
| Docker inside | works, with gVisor's raw-socket flags, a separate runtime class and 6 extra capabilities (D61, D62); no ICMP NAT | works with a real kernel (Docker's own networking); the capabilities stay inside the VM (D64) | expected (not tested) |
| Compatibility | most software; some syscalls and `/proc` details differ | nearly everything Linux runs (a minimal guest kernel, no host devices) | the same as Kata |
| What it took us | a node installer we wrote (D54) | the official installer, one Helm chart (D64) | a VM orchestrator of our own, replacing the pod layer: weeks |

- **Why both:** gVisor starts faster, costs nothing extra per sandbox, runs
  on every node and on a laptop, so it's the default. Kata is the stronger
  boundary (a bug must escape a VM, not a user-space kernel) and runs
  nearly anything Linux runs, at 2-3 s more to start (much more for Docker
  sandboxes started in bulk on small nodes) and ~160 MiB more host memory
  per idle sandbox.
  The caller chooses per sandbox (`"runtime": "kata"`); both run on the
  same x86 nodes.
- **Why not Firecracker:** its strengths (fastest boot, smallest footprint,
  memory snapshots) matter at E2B-like density or for pause/resume, which
  v1 doesn't offer (D8). Used directly it means our own VM orchestration
  instead of Kubernetes pods; through Kata (`kata-fc`) it needs a
  devmapper pool on every node and has no shared filesystem (no virtio-fs:
  Kata copies Secret files into the VM instead). Kata can switch to it
  later by configuration, not redesign.
- **What Kata costs us in defence in depth:** under Kata the VM is the only
  boundary. Kata turns off seccomp in the guest and the guest has no
  AppArmor, so a Docker sandbox (SYS_ADMIN) is root in its VM; that's by
  design, and why the admission policy blocks Kata's pod annotations (they
  could reconfigure the VM, D64).

**Defence in depth around the runtime.** The runtime (gVisor or Kata) is one layer. The others:
- **Admission policy** (CEL), enforced by Kubernetes itself, even against
  the API's own service account:
  - only gVisor (`gvisor`, or `gvisor-docker` for sandboxes that run
    Docker) or Kata (`kata`), and each RuntimeClass checked to point at its
    own handler;
  - no service-account token;
  - only the capabilities sshd and sudo need, plus Docker's six only under
    `gvisor-docker` or `kata`, and `SYS_RESOURCE` only under `kata`;
  - no Kata annotations (they could reconfigure the VM);
  - no host namespaces, hostPath, host ports, sysctls or AppArmor/SELinux
    overrides (Pod Security `baseline`'s rules, which the namespace can no
    longer enforce itself because Docker needs capabilities it forbids);
  - limits required;
  - no debug containers.
- **NetworkPolicies:** sandboxes reach DNS and the public internet, not
  private ranges, the cluster, the nodes or the cloud metadata endpoint.
- **Least-privilege Postgres roles:** the API isn't a superuser; the gateway
  can only read sandboxes and record SSH activity.
- **Pinned host keys:** a per-sandbox host key, generated by the API, so the
  API and gateway can't be pointed at an impostor pod.

Each layer has attack tests in `code/scripts/`. One finding worth knowing: the
container runtime's seccomp profile is *not* applied inside gVisor (turning
it on broke thread creation), so gVisor's own syscall filtering is what
applies (D45).

**Control path: SSH into the sandbox, not an in-sandbox daemon (D7).**
- **Why:** sshd is already in the image and gives exec, exit codes, streaming
  and SFTP file transfer. The API and gateway reach pods with a
  platform-internal key, so the Kubernetes API (`kubectl exec`) isn't a
  bottleneck. Connections are pooled.
- **Cost:** a full sshd per sandbox, and SSH handshakes on cold paths.
- **Upgrade path:** a small purpose-built agent in the image, like E2B's
  `envd`, with an RPC API for processes, files and file watching, reached
  over mTLS. It would give faster calls, streaming output to the agent and
  richer file operations.

**One SSH gateway, not a port or load balancer per sandbox (D6).** Users run
`ssh <sandbox-id>@gateway`.
- **How it works:** the gateway checks the client's key against the one given
  at create, then proxies to the pod. It's stateless, so it scales
  horizontally behind one address. It also limits connections overall, per
  sandbox, and per client while they are still in the SSH handshake (so it
  stops unauthenticated floods, not many logged-in users behind one NAT).
- **Caveat 1, client IP:** the per-client limit needs the real client IP. The
  Service uses `externalTrafficPolicy: Local` for that. Behind a cloud load
  balancer that hides it, use the PROXY protocol or a network load balancer
  that preserves it.
- **Caveat 2, abuse:** the gateway is not a DDoS shield. Rate limiting at the
  edge (cloud load balancer or firewall) belongs in front of it.

**Own pod layer, not `kubernetes-sigs/agent-sandbox` (D27).**
- **Why:** about 120 lines of client-go that we control and tested with
  hand-installed gVisor on arm64.
- **Defence in depth:** every sandbox pod has `activeDeadlineSeconds` = TTL +
  60 s, so the kubelet kills it even if the API is down.
- **What production would look at:** the agent-sandbox `Sandbox` CRD, mainly
  for its warm pools.

**State in Postgres, not in Kubernetes objects.**
- **Why:** users, keys, quotas and history need queries and transactions.
  The quota check locks the user's row, so concurrent creates can't overshoot.
- **How status stays accurate:** a pod watcher plus a periodic reconcile keep
  each record in step with its pod, including pods lost to node failure or
  deleted outside the API, which are marked `lost`.

**Go (D11).**
- **Why:** it's the language of client-go and `x/crypto/ssh`, one static
  binary serves both the API and the gateway, and it's cheap to run with
  many concurrent connections.
- **The alternative:** Python (FastAPI + asyncssh) would have been quicker to
  write for some people and is fine for the API. But the gateway's
  long-lived proxied connections and the Kubernetes client favour Go.

**Dashboard rendered on the server.**
- **What it is:** Go templates, no JavaScript build, every list paged (50 per
  page).
- **No auto-refresh:** an earlier htmx version polled every few seconds. It
  was removed for simplicity (D50); a Refresh link is enough for v1.

## Optional features: what agents need most (D66)

The assigner left four to us: streaming, custom images, public URLs for
ports, and secrets. Ranked by how much an agent is stuck without them:

1. **URLs for sandbox ports: built.** An agent that runs `docker compose up`
   or a dev server needs to check the result from outside, and a person
   reviewing the agent's work needs to see it. Without this the app is
   reachable only from inside the sandbox. `/v1/sandboxes/{id}/ports/{port}/`
   proxies HTTP (and WebSockets) to it, with the owner's API key, over the
   sandbox's own SSH connection, so no new network path to sandboxes opens.
   *Production next step:* subdomain URLs (`8080-sb-x.sandboxes.example`,
   as E2B does) with a wildcard certificate and short-lived signed links
   for browsers; that needs a domain we control (Azure's free DNS names
   have no wildcards).
2. **Streaming output: next.** Today `exec` returns when the command ends
   (up to 10 minutes). Agents running builds or test suites benefit from
   seeing output as it comes (progress, early failure). SSH already streams
   it (`ssh sb-…@gateway cmd`), so this is an API transport choice (chunked
   NDJSON or server-sent events over the existing SSH session), not new
   plumbing.
3. **Secrets: after that.** Agents need tokens (GitHub, package registries,
   model APIs) inside the sandbox. Today they can pass them in `exec` or
   upload a file. A proper version stores them encrypted per user and
   mounts them per sandbox, with redaction in logs; it's mostly key
   management.
4. **Custom images: last.** `ubuntu-docker` covers most of it: an agent can
   build and run any image inside its sandbox. Letting users boot their own
   root images needs image scanning, size limits and a policy on what a
   sandbox image must contain (sshd, our entrypoint), and slows first starts
   (no pre-pull).

## Scaling to thousands (D67)

Measured at the scale the assigner asked for: 10 sandboxes at once, p50/p95
to `running`, per runtime and image, on two 4-vCPU AKS nodes (README, D70):
gVisor p50 3.0–3.3 s / p95 3.7–4.3 s, Kata 5.5–5.7 s / 6.1–6.8 s; with
Docker, gVisor 3.6–4.0 s / 4.0–5.9 s and Kata 16.6 s / 17.7–19.6 s, the one
CPU-bound case (Docker starting in 10 VMs at once). The rest is argued from the design: what each part
costs per sandbox, what limits it first, and how it grows. No part keeps
state that another replica couldn't rebuild from Postgres and the
Kubernetes API.

**Nodes: the real cost.** A sandbox *requests* 250m CPU and 256 Mi (Docker
sandboxes 1 Gi); Kata adds a declared 250m and 130 Mi per VM (its
RuntimeClass overhead); gVisor declares none (its own processes count
against the sandbox's limits). Measured on a 4-vCPU AKS node (3.86 CPUs
allocatable): per-node pods (AKS agents, CSI drivers, the gVisor and Kata
installers, the pre-pull DaemonSet) request 0.37 CPU; one-off cluster pods
(CoreDNS, metrics-server, our API, gateway, Postgres and HTTPS proxy)
request 1.01 CPU in all. On a one-node cluster that leaves room for about
9 gVisor or 4 Kata sandboxes, so 10 at once of each needs a second node.
On 16-vCPU nodes only the per-node 0.37 repeats: about 60 gVisor or 30
Kata sandboxes per node by CPU (Docker sandboxes hit their 1 Gi memory
requests at about the same count on a 64 GiB node), so 1,000 live
sandboxes is roughly 17 gVisor nodes or 34 Kata nodes. Requests are low
because most sandboxes idle far below their 1-2 CPU limits. Growth: the
cluster autoscaler adds nodes when sandboxes stay `pending`; the pre-pull
DaemonSet (D46) puts the images on a new node while its first sandboxes
wait; a warm pool (pods started ahead, assigned at create) would hide the
node and image time entirely.

**Kubernetes API.** A create is three writes (the host-key Secret, the pod,
the Secret's owner reference) and a delete two. The first software limit
was our own: client-go's default client-side rate limit of 5 requests/s
(burst 10) per API replica, shared with the reaper and reconcile: under
1.7 creates/s once the burst is spent, and at 10 at once the create request
itself waited 2-4 s. It's now 50/s (burst 100); measured at 10 at once on
the laptop, the create request dropped from p50 2.3-4.2 s to 0.1 s and p95
to `running` from 4.9-5.2 s to 3.6-4.7 s (D68). Above that, the API
server's own fairness limits and the control plane's size apply: AKS's
Free tier is for testing, its Standard tier is for this. Two costs grow
with the number of sandboxes rather than requests:
- the pod watch (one informer per API replica on the sandboxes namespace),
  a few KB per pod: tens of thousands are fine;
- the reconcile (D28): every 30 s each API replica reads every live
  sandbox from Postgres and LISTs every sandbox's host-key Secret (to
  delete orphans). At 10,000 sandboxes that's a 10,000-object LIST per
  replica per 30 s, the most expensive kind of API call. The fix is to run
  the reconcile on one replica (leader election) and to read Secrets
  through a label-filtered informer instead of LISTing them.

**API replicas: stateless.** Each request is independent and the replicas
share Postgres; the reaper claims rows atomically (D32). Per sandbox, a
replica keeps at most one pooled SSH connection (sshd allows 64 sessions on
it; port URLs are capped at 64 requests in flight) for exec, files and
port URLs, closed after 5 minutes idle. CPU goes to SSH and to copying exec
output, files and proxied responses, so replicas scale with request volume;
the reconcile above is the part that scales with sandbox count.

**SSH gateway: stateless.** Each SSH session is two TCP connections and a
few goroutines. A replica caps itself at 1,000 connections, 20 unfinished
handshakes per client and 10 connections per sandbox (D49). More replicas
go behind the one load balancer; if a replica goes away, its clients
reconnect and land on another.

**Postgres: small and boring.** Writes: one row per create, one update per
status change, one activity touch per exec, file call or port request (and
per minute on long port connections), and one `ssh_activity` upsert per
open SSH session per minute. Reads: the reconcile above. At 10,000 live
sandboxes that's a few hundred small writes and reads per second, one
modest instance. What it needs at that point is availability: a managed,
highly available Postgres (a next step below), and a connection pooler
once API and gateway replicas add up (each has its own pool).

**Image pulls.** 145-350 MB per image (`ubuntu-base`, `ubuntu-dev`,
`ubuntu-docker`), pulled once per node and kept (pre-pull). A cold node took
5-10 s per image from a registry in the same region (D56, arm64 nodes);
with autoscaling that's on the critical path only for the first sandboxes
on a brand-new node. Registry replication or image streaming help further.

**Beyond one cluster.** One cluster can run thousands of sandboxes
(Kubernetes supports 5,000 nodes and 150,000 pods; AKS allows 250 pods per
node with this network setup). Past that, or for regions and failure
isolation: more clusters ("cells"), each with its own gateway; Postgres
records which cell a sandbox lives in, and the API sends each call there.
Every call already carries the sandbox ID, so that's an addition, not a
redesign.

**What would happen at 1,000 creates at once** (with quotas raised: the
default of 10 live sandboxes per user stops one user long before this):
1. **Node capacity.** Sandboxes beyond what fits stay `pending` until the
   autoscaler adds nodes (minutes), then the images download.
2. **The create request.** Each create makes its pod inside the HTTP
   request. With 50 requests/s per replica that's ~17 creates/s per
   replica; a client that gives up first (say after 30 s) cancels its
   create, which is recorded as `failed` with a 503. Creating the pod
   after replying (a queue) would make bursts cheap for clients.
3. **The reconcile LIST**, as the count of sandboxes grows (above).
4. **The control plane tier.**

## Reliability

- **Platform:** API and gateway run 2 replicas each and are stateless. On
  shutdown the API drains in-flight requests; the gateway keeps accepting
  for 5 s while it leaves the Service, then ends its open SSH sessions
  (clients reconnect to another replica). Migrations are safe with several
  replicas.
- **Postgres:** one replica on a persistent volume. A managed, highly
  available Postgres is the production step.
- **Sandbox lifetime:** expired or idle sandboxes are reaped every 30 s, and
  the pod deadline is the backstop.
- **Capacity:** quota is per user, 10 live sandboxes. Past the cluster's
  capacity, creates succeed but stay `pending` with no reason shown yet (D48).

## Startup time

- **Measured** on a laptop (README):
  - one sandbox reaches `running` in about 1.1 s;
  - 10 at once: p50 2.4 s, p95 4.4 s.
- **What got it there:**
  - the readiness probe runs every second;
  - each sandbox requests 1 GiB of ephemeral storage, not the full 5 GiB
    limit, which had capped the node at 6 sandboxes;
  - the images are pre-pulled on every node (a DaemonSet).
- **What pre-pull is worth:** proven and measured (D51, D56). Without it,
  the first `ubuntu-dev` sandbox (218 MB) on a node waits for the image:
  about 5 s from a registry on the same machine, **7.6–9.3 s from Azure's
  registry onto a fresh AKS node**. That matters most on new autoscaled
  nodes.
- **Next steps:**
  - **Warm pool:** pods already running, assigned at create. That brings
    startup to roughly the API round trip, at the cost of idle resources.
  - **Scheduling-aware creates:** say "waiting for capacity" from the pod's
    scheduling condition.
  - **Per-sandbox quota locking:** when one user creates many at once, the
    per-user row lock serializes them.

## Known limits of v1

- **Persistence:** no persistent disks or pause/resume. The workspace is an
  `emptyDir`, lost with the pod.
- **SSH keys:** one key per sandbox, given at create. SSH certificates
  (short-lived, issued by the platform after API-key auth) would allow
  several users, expiry and revocation without storing keys.
- **API and TLS:** the API is plain HTTP inside the cluster. On AKS a Caddy
  proxy adds a public HTTPS address (Let's Encrypt; D57). Other deployments
  bring their own Ingress and TLS, or turn on `publicApi`. One proxy
  replica, so a restart is a brief outage.
- **Dashboard sessions:** signing in stores the API key itself in the
  session cookie (HttpOnly, SameSite=Strict, 12 h; revoking the key ends the
  session). A server-side sessions table, so the browser holds only a
  random session ID, is the upgrade (D40).
- **Admins and SSH:** admins reach anyone's sandbox through the API only;
  the gateway accepts only the sandbox's own key (D39). Admin SSH would come
  with SSH certificates (below) or a separate admin key the gateway trusts.
- **Install:** Helm only via `helm upgrade --install`. Generated secrets rely
  on `lookup`, which GitOps rendering doesn't run (D52).
- **Postgres:** no external or managed Postgres option in the chart yet.
- **Images:** three fixed images (D2, D62). User-supplied images would need image
  scanning and policy.
- **Tenancy:** users and admins only, no organizations, teams or per-sandbox
  sharing.
- **Observability:** JSON logs only; no metrics or traces yet (the next
  thing to add: Prometheus metrics for create latency, exec latency, gateway
  connections).

## Next steps

1. **Cloud: done on Azure AKS (Phase 7, D54–D56).** `./cloud-up.sh` builds
   it from nothing in about 10 minutes and `./cloud-down.sh` removes it; the
   demo and every test suite pass there. Running it found three things the
   laptop couldn't:
   - **AKS's containerd config is a hybrid:** it declares `version = 2` but
     uses containerd 2.x's section names, so gVisor's runtime, added under
     the old names, was silently ignored ("no runtime for runsc"). The
     installer now picks names by what the file uses and checks what
     containerd actually loaded, rolling back otherwise.
   - **The Kubernetes API has a public address on AKS.** The `kubernetes`
     Service forwards there, and the "public internet" egress rule let
     sandboxes open connections to it (no credentials, so requests were
     refused, but it broke the isolation rule). The chart now blocks that
     address automatically (as of each install or upgrade).
   - **Capacity:** AKS's own system pods request a third of two small
     nodes and the platform another 0.4 vCPU, so 7 sandboxes fit, not 10.
   Since then (7.6) the API and dashboard have a public HTTPS address there
   (Caddy with a Let's Encrypt certificate on an Azure DNS name, optional
   IP allowlist), so testers need only a key. Phase 8 moved the nodes to
   x86 (D63) for Kata, with images cross-built for amd64 on the Mac. Still
   to do: the cluster autoscaler for a real cold-node load test.
2. **Managed Postgres** (RDS/Azure Database) as a chart option, with the
   db-setup Job pointed at it.
3. **Warm pool**, through agent-sandbox or our own pod layer.
4. **Pause/resume and persistent workspaces:** snapshot `/home/user` to
   object storage on pause; a volume per sandbox only where needed, since
   cloud disks slow startup and pin a sandbox to a zone.
5. **In-sandbox daemon** instead of exec over SSH (see above), keeping SSH for
   people.
6. **SSH certificates** instead of a stored public key per sandbox.
7. **Tailscale (D16):**
   - *Option 1:* put the gateway, API and dashboard on a tailnet through one
     Tailscale sidecar, so the platform has no public address at all.
   - *Option 2:* every sandbox joins the tailnet for a direct `ssh sb-…`. It
     costs a node and an auth key per sandbox, so it doesn't scale like
     option 1.
8. **Firecracker** under Kata (`kata-fc`) or directly, if density, faster
   starts or pause/resume become requirements (see the runtime table).

## How it was built

- **Steps:** a step per pull request, each logged in DESIGN.md, each
  reviewed by a separate reviewer agent. Findings were checked before
  fixing.
- **Batch reviews:** after each phase, and attack-style tests for every
  security boundary.
- **Testing in a real cluster:** Go unit tests, tests against a real
  Postgres, and end-to-end scripts on the cluster: API smoke test, gateway,
  sandbox hardening, network policy, load and pre-pull. The Helm chart was
  verified both by a from-scratch install on a second cluster and by taking
  over an existing deployment without losing data.
