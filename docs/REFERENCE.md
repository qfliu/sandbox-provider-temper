# Reference

Detail moved out of [README.md](../README.md) and [WRITEUP.md](../WRITEUP.md)
so those stay short. Nothing here is needed to understand the design; it's
for running, testing and checking numbers. Decision IDs (D4, …) point into
[DESIGN.md](../DESIGN.md).

## Development scripts

Run `source ./env.sh` first (like a Python venv; `deactivate_sandbox_env`
undoes it).

| Script | What it does |
|---|---|
| `code/scripts/cluster-up.sh` | minikube (vfkit, containerd, Calico) + gVisor + sandbox images |
| `code/scripts/deploy.sh` | builds the platform image, `helm upgrade --install` |
| `code/scripts/test-db.sh` | all Go tests, including the ones against Postgres |
| `code/scripts/smoke-api.sh` | API end to end (31 checks) |
| `code/scripts/test-gateway-e2e.sh` | SSH gateway end to end, incl. attacks (16 checks) |
| `code/scripts/test-sandbox-hardening.sh` | what code inside a sandbox can and can't do, and the admission policy (39 checks on minikube; 46 on AKS, with the Kata and runc probes) |
| `code/scripts/test-agent-workflow.sh` | agent workflows end to end: an app with Redis, Postgres, a multi-stage Go build, WebSockets through a port URL, Docker storage full (17 checks) |
| `code/scripts/test-ports.sh` | port URLs: proxying, credential stripping, refusals (13 checks) |
| `code/scripts/test-docker.sh` | Docker in an `ubuntu-docker` sandbox: build, run, `-p`, compose with ports, `docker exec`; isolation of nested containers; forged packets stay on the node (23 checks) |
| `code/scripts/test-network-policy.sh` | sandbox network isolation (15 checks on minikube; 18 on AKS, with the load balancer and proxy checks) |
| `code/scripts/load-test.sh N [image]` | N sandboxes at once, startup percentiles |
| `code/scripts/test-prepull.sh`, `measure-prepull.sh` | image pre-pull proof and timing (minikube only) |

Every test script that creates sandboxes takes `SANDBOX_RUNTIME=kata` to run them
under Kata (on a cluster with Kata, i.e. AKS); `load-test.sh` too.

## API details

- **Create**
  - `image` is `ubuntu-base`, `ubuntu-dev` or `ubuntu-docker`.
  - `runtime` (optional) is `gvisor` (the default: a user-space kernel,
    fastest start) or `kata` (a lightweight VM with its own Linux kernel,
    the stronger boundary). `kata` exists only where Kata is installed (AKS
    on x86 nodes); elsewhere it's refused with "isn't available on this
    cluster". Every image works under both; responses include `runtime`.
  - `ttl_seconds` runs from 60 to 86400; the default is 3600.
  - `ssh_public_key` is the only key the gateway accepts for this sandbox.
- **Ports** (D66)
  - Any method, any path under the prefix, WebSockets too; the server must
    listen on `127.0.0.1` or `0.0.0.0` inside the sandbox (Docker's
    published ports do). Nothing listening → **502**.
  - Needs the owner's (or an admin's) API key, like every call. The key and
    any cookies are removed before the request reaches the sandbox;
    responses lose `Set-Cookie` and get `Content-Security-Policy: sandbox`.
    The app sees its prefix in `X-Forwarded-Prefix`; a redirect to `/x`
    stays under the prefix. At most 64 requests at once per sandbox
    (429 beyond).
  - It goes over the sandbox's own SSH connection (a forward to its
    loopback), so no other network path to the sandbox exists.
- **Exec**
  - `command` runs in `bash -c` as `user`, who has passwordless `sudo`.
  - `timeout_seconds` can be up to 600 (default 60); a command that runs out
    of time is stopped and returns `timed_out: true` (exit code 124, or 137 if
    it ignored SIGTERM).
  - stdout and stderr are capped at 1 MiB each (`truncated: true`).
- **Lifetime**: a sandbox ends on `DELETE`, after 30 minutes idle, or when
  `ttl_seconds` runs out. Idle means no exec, no file transfer and no SSH
  session; reading its status doesn't count, so an agent that only polls is
  still reaped.
- **Status**:
  - Live: `pending` → `running` (→ `deleting`).
  - Ended: `deleted`, `failed` (couldn't start) or `lost` (the pod disappeared).
  - `status_reason` says why. Ended sandboxes stay listed for history.
- **Status codes**:
  - 400: invalid input.
  - 401: missing, unknown or revoked API key.
  - 403: the sandbox's own file permissions refused a file operation.
  - 404: not yours, no such sandbox, or no such file.
  - 409: not running yet or anymore.
  - 413: upload over 100 MiB.
  - 429: quota reached, or 64 port-URL requests to one sandbox already in flight.
  - 502: the sandbox didn't answer.
  - 503: the cluster is busy.
- **Admins** can do all of this to anyone's sandbox, and see every user and
  sandbox in the dashboard. A first admin is made with
  `kubectl -n sandbox-system exec deploy/sandbox-api -- /api create-user --name <name> --admin`
  (it prints the key once); `create-key --user <name>` issues more keys.

SSH:

```bash
source ./env.sh && ssh -i ~/.ssh/KEY -p 30022 sb-1a2b3c4d5e6f@$(minikube ip)   # minikube NodePort
ssh -i ~/.ssh/KEY sb-1a2b3c4d5e6f@gateway.example.com                           # behind a LoadBalancer
```

To pin the gateway's host key instead of trusting the first connection:
`kubectl -n sandbox-system exec deploy/sandbox-gateway -- /api public-key /keys/host/ssh_host_ed25519_key`
prints it (`ssh-ed25519 AAAA…`). Put `[host]:port ` in front of it for a
`known_hosts` line; `deploy.sh` prints the whole line.

## Measurements

### Laptop

On a MacBook (minikube VM with 4 CPUs and 8 GB, vfkit, gVisor), images
already on the node:

| What | Time |
|---|---|
| One sandbox, create → `running` | **1.1 s** (first exec 1.2 s) |
| 10 at once, `ubuntu-base`, → `running` | **p50 2.5–2.8 s, p95 3.6–4.7 s** (10/10; the create request 0.1 s, D68) |
| … measured earlier (D48), before the client rate-limit fix | p50 2.4 s, p95 4.4 s; `ubuntu-dev` p50 3.1 s, p95 4.3 s |
| First `ubuntu-dev` sandbox on a node **without** the pre-pull | ~5.5 s (4.8–5.1 s of it is the image download + unpack, from a registry on the same VM) |
| … **with** the pre-pull DaemonSet | 0.6 s |
| `up.sh` from a fresh clone / again (VM stopped), before `ubuntu-docker` | 305–367 s / 84 s |
| `deploy.sh` (platform image build + `helm upgrade --install`) on a cluster with gVisor and images | ~20 s |

Capacity on this VM is about 10 sandboxes (250m CPU requested each); past
that, creates succeed but stay `pending` until room frees up (D48). The
download time is a lower bound: from a real registry it is
bandwidth-bound, which is why the images are pre-pulled on every node
(D46, D51).

### Azure AKS, both runtimes

2 × `Standard_D4s_v6` (x86, 4 vCPU / 16 GiB, gVisor and Kata; images
pre-pulled; 3 runs of each), D70. The 10-at-once table is in
[README § Measured](../README.md#measured); the rest:

| Other | |
|---|---|
| One `ubuntu-docker` sandbox alone: gVisor / Kata | ~3 s / ~4 s |
| The create request itself (the 10-at-once runs) | p50 0.3–0.5 s |
| Host memory per idle `ubuntu-base` sandbox (RSS of its processes on the node): gVisor / Kata | ~90 MiB / ~250 MiB |
| `./cloud-up.sh` from nothing (2 nodes, gVisor and Kata, amd64 images cached) | 884–975 s |

Kata with Docker at 10 at once is the outlier: starting Docker in 10 VMs at
once on 8 vCPUs of nested virtualization is CPU-bound (dockerd took 8–12 s
to answer in each VM; ~1.75 s in an otherwise idle VM). It was 26–27 s
before Docker's nftables backend replaced its few hundred `iptables` calls
(D70). Bigger nodes or a warm pool are the fix.

A later fresh deployment (the final check before submission) measured, 10 at
once: gVisor p50 3.2 s / p95 3.6 s, with Docker 3.2 / 3.8 s; Kata 5.1 / 5.9 s,
with Docker 13.8 / 14.4 s. `cloud-up.sh` took 884 s.

### Earlier AKS deployment (arm64, gVisor only)

An **earlier deployment**, gVisor only (2 × `Standard_D2ps_v5`, arm64, 2 vCPU /
8 GiB; images pre-pulled from the cluster's registry), D56. Kept for the
cold-node pre-pull numbers:

| What | Time |
|---|---|
| `./cloud-up.sh` from nothing (cluster, gVisor, images, install) / `./cloud-down.sh` | 581 s (633 s on the run before the fixes) / 378–394 s |
| Pre-pull from the registry onto a fresh node: `ubuntu-dev` (218 MB) / `ubuntu-base` (145 MB) | **7.6–9.3 s** / 4.6–8.1 s |
| One sandbox, create → `running` | 3.1 s (first exec 3.4 s) |
| 10 at once → 7 running: `ubuntu-base` / `ubuntu-dev` | p50 3.7 s, p95 5.2 s / p50 3.8 s, p95 4.8 s |

On those small nodes only 7 of 10 fit: of the 3.8 allocatable vCPUs, AKS's own system pods
request 1.26 and the platform 0.42, leaving 0.97 and 1.13 on the two nodes,
room for 3 + 4 sandboxes at 250m each; the other 3 stay `pending` until
room frees up. More or larger nodes (or the cluster autoscaler) raise that.

### What brought startup down

- the readiness probe runs every second;
- each sandbox requests 1 GiB of ephemeral storage, not the full 5 GiB
  limit, which had capped the node at 6 sandboxes;
- the images are pre-pulled on every node (a DaemonSet);
- the API's own client-side limit toward Kubernetes was raised, which
  took the create request from 2–4 s to 0.1 s at 10 at once (D68);
- under Kata, Docker uses its nftables backend (27 → 16.6 s for Docker
  sandboxes at 10 at once, D70).

What pre-pull is worth, proven and measured (D51, D56): without it, the
first `ubuntu-dev` sandbox (218 MB) on a node waits for the image: about
5 s from a registry on the same machine, **7.6–9.3 s from Azure's registry
onto a fresh AKS node**. That matters most on new autoscaled nodes.

## Azure (AKS) in detail

Needs:

- **An Azure subscription with 8 vCPUs of quota** in the region for the node
  size's family (2 x86 `Standard_D4s_v6` nodes, which have nested
  virtualization for Kata). A free account starts at 4 and can't raise it:
  upgrade to pay-as-you-go (the credit still applies), then check
  Quotas → Compute. If one family's increase is refused (DSv5 was, here),
  pick a size whose family already has quota with `AZURE_NODE_SIZE`; the
  quota check says which before anything is created.
  `AZURE_NODE_SIZE=Standard_D2ps_v5` gives 2 small arm64 nodes, gVisor only.
- **The local cluster from `./up.sh`**, which builds the images. For x86
  nodes `cloud-up.sh` builds amd64 images on it under QEMU emulation, about
  5 minutes the first time (D63).

Node size and count are remembered only while the cluster exists. The
commands are in [README § On Azure](../README.md#on-azure-aks). The cost,
about $0.45/hour, is an estimate for 2 × `Standard_D4s_v6` pay-as-you-go in
West US 2, plus disks and public IPs; the AKS control plane (Free tier)
costs nothing.

`cloud-up.sh` checks the login, the region's quota and that the node size
is offered, creates the cluster and registry with Terraform (Kubernetes API
open to your IP only), installs gVisor on the nodes
(`code/deploy/cloud/gvisor-installer.yaml`) and, on x86 nodes, Kata
Containers (`kata-deploy`, `code/deploy/cloud/kata-values.yaml`), pushes the
images, and installs the chart. That gives:

- a public SSH gateway on port 22;
- the API and dashboard on **`https://sandbox-<id>.<region>.cloudapp.azure.com`**,
  with a Let's Encrypt certificate, served by Caddy in front of the API;
- an admin key in `.devenv/state/aks/admin-api-key` (copy it with `pbcopy <`
  that path to sign in to the dashboard).

`source ./env.sh aks` / `source ./env.sh local` switch kubectl, `demo.sh`
and the test scripts between the two clusters.

### Letting testers in

1. **Optionally limit who can connect:**
   `ALLOWED_CIDRS="203.0.113.0/24,198.51.100.7/32" ./cloud-up.sh`. Only those
   ranges, plus your own IP, can then reach the HTTPS API, the dashboard and
   the SSH gateway; others get 403 (HTTPS) or no connection (SSH). Without
   it, anyone can connect but still needs a key.
   - **To change it during a session:**
     `ALLOWED_CIDRS="..." ./cloud-up.sh --allowlist-only` (`none` opens it
     up). Nothing is rebuilt and testers' sessions keep running.
   - **Settings are remembered:** a plain re-run of `./cloud-up.sh` (which
     redeploys the code and ends SSH sessions) keeps the last allowlist,
     region and size, and refuses changing region, name or node size/count on a running cluster.
2. **Give each tester their own key:**
   `source ./env.sh aks && ./code/scripts/add-tester.sh alice`. That writes
   the key to `.devenv/state/aks/testers/alice.key` (it isn't printed). Send
   it through a password manager share, not chat or email, then delete the
   file.
3. **Send them [TESTING.md](../TESTING.md)** with the HTTPS address and the SSH
   gateway address (both printed by `cloud-up.sh`). For a checked first SSH
   connection, also send the gateway's `known_hosts` line (in an `aks` shell;
   GATEWAY_IP is the SSH address):
   `echo "GATEWAY_IP $(kubectl -n sandbox-system exec deploy/sandbox-gateway -- /api public-key /keys/host/ssh_host_ed25519_key | cut -d' ' -f1,2)"`.
4. **Watch:** the admin dashboard (`/ui/admin`, with your key) shows every
   user and sandbox.
5. **End someone's access:**
   `source ./env.sh aks && ./code/scripts/add-tester.sh --revoke alice`
   (revokes their keys, quota 0; the script prints which cluster it acted
   on). Adding the same name again gives them a
   new key and their quota back. Then `./cloud-down.sh` when the session is
   over: it all costs about $0.45/hour while it runs.

Let's Encrypt issues at most 5 certificates a week for one name, and each
`cloud-up.sh` after a `cloud-down.sh` needs a new one. Past that,
`AZURE_DNS_LABEL=sandbox-<new> ./cloud-up.sh` uses another address.

## Deploying to another cluster

The chart in `code/deploy/helm` needs:

- **gVisor with two handlers on every node:** `runsc` (RuntimeClass
  `gvisor`) and `runsc-docker` with `net-raw` and `allow-packet-socket-write`
  (RuntimeClass `gvisor-docker`, for `ubuntu-docker`). The chart can't install
  node software. Use `code/scripts/install-gvisor.sh` (minikube) or the
  installer DaemonSet in `code/deploy/cloud/gvisor-installer.yaml` (used on
  AKS; it handles containerd 1.x and 2.x config formats and rolls back a
  config containerd won't load), which set up both.
- **Kata (optional):** `kata-deploy` with `code/deploy/cloud/kata-values.yaml`
  on x86 nodes with nested virtualization, then
  `--set 'sandboxRuntimes={gvisor,kata}'`.
- **A CNI that enforces NetworkPolicies.**
- **The images in a registry the nodes can pull from.** That's the platform
  image (`code/Dockerfile`) and the three sandbox images
  (`code/images/sandbox`, targets `base`, `dev` and `docker`, tagged
  `sandboxImages.tag`, now `0.3`).

```bash
helm upgrade --install sandbox code/deploy/helm -n sandbox-system --create-namespace \
  --set image.repository=REGISTRY/sandbox/api --set image.tag=TAG \
  --set sandboxImages.repository=REGISTRY/sandbox/ \
  --set gateway.service.type=LoadBalancer \
  --set api.sshGatewayAddr=GATEWAY_HOST
```

Values (`values.yaml`) also cover replicas, resources, sandbox TTL and idle
limits, gateway connection limits, Postgres storage, and the egress ranges
sandboxes may not reach. If the `kubernetes` Service forwards to a public
address (as on AKS), the chart blocks it for sandboxes automatically, as of
the install or last upgrade (re-run the upgrade if the control plane's IP
changes; `test-network-policy.sh` checks it). A public API endpoint that
isn't behind that Service (possibly EKS) goes in
`networkPolicies.extraBlockedRanges`. Passwords and keys are generated on first install
and kept across upgrades. Install only with `helm upgrade --install`, not
`helm template` (generated secrets rely on `lookup`, D52). The gateway's
per-client handshake limit needs the real client IP (`externalTrafficPolicy:
Local`); behind a load balancer that hides it, use the PROXY protocol or a
network load balancer that preserves it. For a public HTTPS address, either use your own Ingress
and TLS, or turn on the chart's Caddy front: `publicApi.enabled=true`,
`publicApi.host=<a DNS name pointing at its load balancer>`, and optionally
`publicApi.allowedSourceRanges`.

## Scaling to thousands: the full walkthrough (D67)

The short version is in [WRITEUP.md](../WRITEUP.md#scaling-to-thousands-d67).

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
the laptop, the create request dropped from p50 2.3–4.2 s to 0.1 s and p95
to `running` from 4.9–5.2 s to 3.6–4.7 s (D68). Above that, the API
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
highly available Postgres (a next step in WRITEUP), and a connection pooler
once API and gateway replicas add up (each has its own pool).

**Image pulls.** 145 MB (`ubuntu-base`), 218 MB (`ubuntu-dev`), about
350 MB (`ubuntu-docker`), pulled once per node and kept (pre-pull). A cold node took
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

## Compared with hosted sandbox providers: the full table

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
| **Snapshot / restore / fork, pause / resume** | all three: files everywhere; memory and running processes in E2B's default pause/resume, experimental at Modal, VM sandboxes only at Daytona | not built. On 2026-10-06 Temper's follow-up noted gVisor can't give the snapshot/restore hosted providers offer; the 2026-10-08 follow-up made Docker inside sandboxes the requirement and didn't list snapshots, so the time went there. gVisor can checkpoint a running sandbox (`runsc checkpoint`); the missing part is restoring it into a new pod: Kubernetes has no restore path for gVisor checkpoints (its checkpoint API is for runc containers) |
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
