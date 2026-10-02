# Agent Sandbox Provider — Design Doc

> Living document. Every decision is logged in **§3 Decision Log**; update it whenever a new decision is made.
> Status legend: ✅ Decided · 🟡 Proposed (not yet confirmed) · ❓ Open

---

## 1. Overview

A self-hosted, scalable **sandbox provider**: AI agents (and humans) use an API key to create isolated Linux machines, run commands and move files in them, SSH in, and then destroy them. An admin dashboard shows and manages everything.

Comparable products: E2B, Modal Sandboxes, Daytona. This is a smaller self-built version of the same idea.

```
  Agent / User ──API key──▶  Control Plane (API)  ──▶  Kubernetes  ──▶  Sandbox pods (gVisor)
  Human ─────────SSH──────▶  SSH Gateway  ───────────────────────────▶  sandbox sshd
  Admin ─────────browser──▶  Dashboard  ──▶  Control Plane
```

---

## 2. Requirements (from task brief)

### Must have
- R1. Sandbox provider for agents to securely run code.
- R2. Kubernetes- (or other scheduler-) based; scales to hundreds or thousands of sandboxes; not a single box.
- R3. Strong isolation between sandboxes and the host.
- R4. Cloud-portable (AWS, Azure, …); no managed sandbox services from the cloud provider.
- R5. API key auth; agents can create, interact with, and delete sandboxes.
- R6. SSH access into sandboxes (explicitly a primary requirement).
- R7. Dashboard / admin interface to see and interact with sandboxes.
- R8. Reliability, durability, fast startup time.

### Stretch (from brief)
- S1. Remote desktop viewing — **deferred** (see D1).
- S2. Private network access via Tailscale.

---

## 3. Decision Log

| ID | Date | Area | Decision | Rationale |
|---|---|---|---|---|
| D1 | 2026-10-01 | Environment | ✅ Headless Linux with SSH only. No GUI, X11, VNC or remote desktop. | Cuts the heaviest part of scope; SSH is the stated primary requirement. |
| D2 | 2026-10-02 | Images | ✅ Fixed choice of 2 images, both Ubuntu 24.04: `ubuntu-base` (git, curl, build-essential, openssh-server) and `ubuntu-dev` (base + Python 3, Node.js, package managers). | Simple; shared base layers keep pulls and startup fast. |
| D3 | 2026-10-02 | SSH auth | ✅ The user supplies `ssh_public_key` when creating a sandbox; the provider injects it into `authorized_keys`. | Simplest, familiar pattern. Provider-generated keys and short-lived SSH certificates are deferred; certificates are the production path (mention in write-up). |
| D4 | 2026-10-02 | Isolation | ✅ gVisor (`runsc`) via Kubernetes `RuntimeClass`. No Kata or Firecracker. | Runs on any cloud VM without nested virtualization, Kubernetes-native, fast startup, low effort. Right fit for a take-home. Note Kata/Firecracker as the hardening path in the write-up. |
| D5 | 2026-10-02 | Agent model | ✅ Agents run **outside** the sandbox (on the customer's side) and control it remotely: primarily through the HTTP API, optionally over SSH via the same path humans use. Running the agent *inside* the sandbox is out of scope. | Matches the brief ("agent… with an API key… spin up these sandboxes, interact with them"). Keeps the platform focused on lifecycle, exec, files and SSH. |
| D6 | 2026-10-02 | SSH routing | ✅ A single **SSH gateway** routes by username: `ssh sb-123@gateway.example.com`. The gateway checks the client's key against the `ssh_public_key` stored for that sandbox (D3), then opens an internal connection to the pod and pipes the two together. It is stateless, so several replicas can sit behind one load balancer. sshpiper vs. a custom Go gateway is decided with the tech stack (D11). | Port-per-sandbox caps at ~2.7k NodePorts or needs a load balancer per sandbox. A bastion gives users a shared shell, which is a security hole. One gateway scales, gives a clean UX and is one audited entry point. |
| D7 | 2026-10-02 | Exec / files | ✅ For v1, the API server reaches pods over **internal SSH** using a platform-internal key that users never see: exec over SSH sessions, files over SFTP. Connections are pooled to avoid handshake cost. The gateway (D6) uses the same internal key, so pods trust **only** the internal key, and users' keys (D3) are verified at the gateway. A NetworkPolicy allows port 22 into pods only from the API server and gateway. | No requirement justifies more complexity. Reuses sshd (D1), bypasses the Kubernetes API bottleneck, and gives streaming, exit codes and file transfer for free. Upgrade path: an in-sandbox daemon like E2B's envd (mention in write-up). |
| D8 | 2026-10-02 | Persistence | ✅ The workspace (`/home/user`) is a Kubernetes `emptyDir` volume with a size limit. It survives container crashes and restarts, and is lost only if the pod or node is lost. If a node dies, the sandbox status becomes `lost` and it is **not** silently recreated; the agent creates a new one. No persistent volumes or pause/resume in v1. | No startup cost, survives the common failure (process crash). Cloud disks slow startup, cap density per node and pin to one zone. "Durable" in the brief is read as *platform* durability (API, DB, gateway and accurate state), not per-sandbox files. State this assumption in the write-up; persistent disks and pause/resume are the upgrade path. |
| D9 | 2026-10-02 | Lifetime | ✅ Three ways a sandbox ends: (1) explicit `DELETE`; (2) **idle timeout** of 30 min with no API calls or SSH sessions, using `last_activity` recorded by the API and gateway; (3) **max TTL set by the user** on create via `ttl_seconds` (default 1 h; platform cap 24 h, configurable by an admin). A reaper loop in the control plane deletes expired sandboxes about every 30 s. | Agents forget to clean up or crash; resources must be reclaimed automatically. Users control how long their sandbox may live. |
| D10 | 2026-10-02 | Deployment | ✅ Two steps. **Step 1 (now):** develop and test locally on minikube with the gVisor add-on, on the user's **Apple M1 Pro Mac (arm64)**. Local images are built for arm64 only; **multi-arch (arm64 + amd64)** builds with `docker buildx` are added only if the optional cloud step targets amd64 nodes. Alternatively, use arm64 cloud nodes (AWS Graviton / Azure Ampere) in step 2. gVisor supports arm64. Ship a Helm chart plus a one-command setup script. **Step 2 (later):** test on AWS or Azure. The user has no cloud account yet, so the provider and approach (managed EKS/AKS vs. k3s on VMs via Terraform) are decided then. Never use GKE Sandbox or AKS Pod Sandboxing (ruled out by the brief). | Fast, free iteration first. The Helm chart is the portable unit. A cloud test proves "runs on AWS/Azure" and enables a real load test. |
| D11 | 2026-10-02 | Tech stack | ✅ **Go** for both the control plane API and the SSH gateway (one repo, shared packages): `client-go` for Kubernetes, `golang.org/x/crypto/ssh` + `github.com/pkg/sftp` for SSH/SFTP. **Postgres** for state (shared by API replicas). Dashboard: see D13. | Go is the native language of the Kubernetes and SSH ecosystems and fits infrastructure work. The user reads Python fluently and Go partially; Claude writes the code with comments on non-obvious parts. Postgres (not SQLite) so several API replicas can run. |
| D12 | 2026-10-02 | Time budget / scope | ✅ Target about **7 hours** of the user's time, flexible upward. It's a take-home, so it does **not** need to be fully production-deployable: a working local system plus a clear write-up is the bar. Step 2 (cloud deploy) is optional and described in the write-up. Warm pools, persistent disks, SSH certs, Tailscale and Kata/Firecracker are write-up only. | The user is preparing for other interviews. Prioritize a working, demonstrable core and well-reasoned trade-offs over breadth. |
| D13 | 2026-10-02 | Dashboard | ✅ Server-rendered **Go `html/template` + htmx**, embedded in the API binary with `//go:embed` and served at `/ui`. A small classless CSS framework for styling. No Node or React toolchain. | Covers everything the dashboard needs (lists, detail view, create/delete, API keys, admin view, live refresh) with no frontend build. One binary and one language to explain. Saves about 30–45 min compared with React + Vite. |
| D14 | 2026-10-02 | Dev environment | ✅ All tools (go 1.27.1, kubectl v1.37.1, helm v4.3.0, minikube v1.39.0, lima 2.2.0, colima v0.10.3, docker CLI 29.8.2; pinned in `code/scripts/bootstrap-tools.sh`) plus every config, cache and VM disk live in **one folder, `.devenv/`**. `source ./env.sh` redirects each tool's home (KUBECONFIG, MINIKUBE_HOME, COLIMA_HOME, LIMA_HOME, DOCKER_CONFIG, GOPATH, GOCACHE, GOENV, TMPDIR, Helm dirs); a `go` wrapper sets HOME so Go telemetry stays inside too. Docker engine is provided by **colima** (free) instead of Docker Desktop. Verified 2026-10-02 with a before/after snapshot of `~`: no new files left behind. Uninstall: `colima stop; rm -rf .devenv`. | The user asked for nothing to touch the system, with everything under one folder. No sudo needed. |
| D15 | 2026-10-02 | Repo layout | ✅ Standalone project at `~/Documents/sandbox-provider/` (**not** part of `ethan_project`), a git repo with remote `origin` = https://github.com/qfliu/sandbox-provider-temper (private), branch `main`. Root holds docs (`DESIGN.md`, later `README.md`), `env.sh` and `.devenv/` (tools + state, gitignored). **All source code lives in `code/`**; **all build output (executables) goes to `code/bin/`** (gitignored). Planned structure: `code/cmd/{api,gateway}`, `code/internal/…`, `code/web/` (templates), `code/images/sandbox` (D23), `code/deploy/helm/`, `code/scripts/`. | The user wants code in one folder, executables in one output folder, and the take-home separate from unrelated projects. |
| D16 | 2026-10-02 | Tailscale (S2) | ✅ **Write-up only.** Describe two options: (1) put the gateway, API and dashboard on a tailnet (one Tailscale sidecar, so the platform is invisible to the public internet), or (2) each sandbox joins the tailnet (direct `ssh sb-123`, but one node and auth key per sandbox, which doesn't scale well). If time remains at the end, option 1 could be built (~1 h). | Stretch goal; needs a Tailscale account and auth keys; outside the time budget. |
| D17 | 2026-10-02 | Multi-tenancy | ✅ Users, each with one or more API keys, and an `is_admin` flag. Users see only their own sandboxes; admins see all. No orgs or teams. Per-user quota: max **10** concurrent sandboxes. | Simplest model that covers "agent with an API key" and an admin interface. |
| D18 | 2026-10-02 | Network policy | ✅ Sandboxes **may** reach the internet (pip, npm, git clone). **Blocked:** sandbox-to-sandbox, sandbox-to-control-plane/Postgres/cluster services, and the cloud metadata endpoint `169.254.169.254`. **Ingress** to sandbox port 22 is allowed only from the API server and SSH gateway pods. Enforced with Kubernetes NetworkPolicy (requires a CNI that enforces it, e.g. Calico/Cilium; minikube `--cni=calico`). | Agents need package installs; untrusted code must not reach neighbours, platform internals or cloud credentials. |
| D19 | 2026-10-02 | Startup optimization | ✅ Pre-pull both images on every node with a small DaemonSet, so sandbox start never waits on an image download. Measure and report startup time (p50/p95) in the README. Warm pool of pre-created pods: **write-up only**. | Largest startup win for least effort. |
| D20 | 2026-10-02 | Local VM | ✅ minikube runs its own VM with the **vfkit** driver (`--driver=vfkit`, macOS Virtualization.framework, NAT networking, no sudo) instead of colima + Docker. Started with `--container-runtime=containerd --cni=calico --cpus=4 --memory=8192 --disk-size=40g`. vfkit v0.6.4 added to the bootstrap script. Images build inside the node with `minikube image build` (BuildKit), so **colima, lima and the docker CLI were removed** from the toolchain. | One fewer layer (macOS → VM → gVisor rather than macOS → VM → Docker → node container → gVisor). gVisor runs in a real VM, not nested in a container. |
| D21 | 2026-10-02 | gVisor install | ✅ **Don't use `minikube addons enable gvisor`.** Its download URL is stale (gVisor now publishes one `gvisor.tar.bz2` bundle per arch, not separate binaries), so it saved an XML "NoSuchKey" error page as `runsc`. This affects all architectures, not just arm64. Instead, `code/scripts/install-gvisor.sh` downloads the pinned release (20260928), verifies its SHA-512, copies `runsc` + `containerd-shim-runsc-v1` + `gvisor-bin/` into the node's persistent `/data/gvisor`, symlinks them into `/usr/bin` (the node's `/` is tmpfs), adds the `runsc` runtime to containerd and applies the `gvisor` RuntimeClass. Re-run after every `minikube start`. | Verified: a pod with `runtimeClassName: gvisor` reports kernel `4.19.0-gvisor` and dmesg "Starting gVisor…" (normal pods see `6.6.152`), and apt-get reaches the internet. |
| D22 | 2026-10-02 | sudo in sandboxes | ✅ gVisor's `allow-suid` is enabled (`/etc/containerd/runsc.toml`, wired via the runsc runtime options) so setuid binaries like `sudo` work; `user` has passwordless sudo. gVisor ignores SUID bits by default. | Agents often need `apt install`. Root *inside* the sandbox stays contained by gVisor, the real isolation boundary; E2B-style sandboxes also give sudo. Reversible: drop the flag and the sudoers file for a stricter mode. |
| D23 | 2026-10-02 | Image layout | ✅ One Dockerfile with two targets at `code/images/sandbox/` (`base` → `sandbox/ubuntu-base:0.1`, `dev` → `sandbox/ubuntu-dev:0.1`) instead of two folders, so `dev` builds on `base` without a registry. sshd runs as root and only allows `user`, key-only, trusting only `/etc/sandbox/authorized_keys` (the internal key from a Secret, D7). The account password is `*`, not `!`: with UsePAM=no, sshd rejects "locked" accounts even for valid keys. | Simple local builds; no registry in the cluster. |

---

## 4. Open Questions

_None open. New questions get added here as they come up._

---

## 5. Architecture (draft — update as open questions are resolved)

### Components
1. **Control Plane API**: authenticates API keys, exposes REST endpoints, stores sandbox state, and talks to the Kubernetes API to create and delete pods. Runs exec and file operations over pooled internal SSH/SFTP connections to pods (D7). Stateless, so it can be scaled horizontally.
2. **State store** 🟡: database of API keys, sandboxes, owners and status.
3. **Kubernetes cluster**: worker nodes with gVisor installed. Each sandbox is one pod with `runtimeClassName: gvisor`, running one of the two images.
4. **SSH Gateway** ✅ (D6): a public SSH entry point (port 22, one load balancer) that routes `sb-<id>@gateway` to the right pod after checking the user's key. Used by humans and by SSH-based agents.
5. **Dashboard** ✅ (D13): Go templates + htmx served by the API binary at `/ui`. Lists sandboxes, status, owner and age; supports create and delete; manages API keys.

### Draft API 🟡
```
POST   /v1/sandboxes                 { image, ssh_public_key, ttl_seconds? } → { id, status, ssh, expires_at }
GET    /v1/sandboxes                 → list (own sandboxes; admin sees all)
GET    /v1/sandboxes/{id}            → status
DELETE /v1/sandboxes/{id}
POST   /v1/sandboxes/{id}/exec       { cmd, timeout } → { stdout, stderr, exit_code }
PUT    /v1/sandboxes/{id}/files      upload file
GET    /v1/sandboxes/{id}/files?path download file
```
All requests use `Authorization: Bearer <api_key>`.

### Isolation (D4)
- Pod `runtimeClassName: gvisor`.
- 🟡 Also planned: NetworkPolicy denying sandbox-to-sandbox and sandbox-to-control-plane traffic, non-root user, CPU and memory limits per pod.

---

## 6. Repository Layout (D15)

```
sandbox-provider/
├── DESIGN.md                 design doc + decision log (this file)
├── README.md                 (later) how to run + write-up
├── env.sh                    source to activate the project-local toolchain
├── .devenv/                  tools + VM/cluster state (gitignored)
└── code/                     ALL source code
    ├── go.mod
    ├── cmd/api/              control plane API + dashboard entry point
    ├── cmd/gateway/          SSH gateway entry point
    ├── internal/             shared Go packages (db, k8s, sshx, auth, …)
    ├── web/                  dashboard templates + CSS (embedded via go:embed)
    ├── images/sandbox/       Dockerfile (targets: base, dev), sshd_config, entrypoint (D23)
    ├── deploy/dev/           hand-written manifests for manual testing
    ├── deploy/helm/          Helm chart
    ├── scripts/              bootstrap-tools.sh, cluster setup, demo scripts
    └── bin/                  BUILD OUTPUT: compiled executables (gitignored)
```

---

## 7. Implementation Steps

Update checkboxes as work progresses. **Workflow:** one branch and one PR per phase (`phaseN/<name>`); never push directly to `main`. Each phase ends with a PR containing a summary and how to test it; the user reviews and merges before the next phase starts.

### Phase 0: Decisions
- [x] Scope: SSH-only, no GUI (D1)
- [x] Images (D2)
- [x] SSH auth (D3)
- [x] Isolation: gVisor (D4)
- [x] Agent model: agents run outside the sandbox (D5)
- [x] SSH routing: gateway (D6)
- [x] Exec / files: internal SSH + SFTP (D7)
- [x] Persistence: emptyDir workspace, `lost` status on node failure (D8)
- [x] Lifetime: delete + 30-min idle + user-set TTL (D9)
- [x] Deployment: local minikube first, cloud later (D10)
- [x] Tech stack: Go + Postgres (D11)
- [x] Time budget: ~7 h (flexible), local working system + write-up (D12)
- [x] Dashboard: Go templates + htmx (D13)
- [x] Tailscale: write-up only (D16)
- [x] Multi-tenancy, network policy, startup (D17–D19)

### Phase 1: Local cluster + gVisor (~1.5 h; riskiest) — branch `phase1/local-cluster`
- [x] 1.0 Install tools project-locally (no Homebrew, no sudo): `./code/scripts/bootstrap-tools.sh` → everything under `.devenv/`; activate with `source ./env.sh` (D14)
- [x] 1.1 ~~colima VM~~ → minikube's own VM via vfkit (D20); home-folder check found kubectl's `~/.kube/cache` leak, fixed with `KUBECACHEDIR` in env.sh
- [x] 1.2 minikube: `--driver=vfkit --container-runtime=containerd --cni=calico` (Calico needed to enforce NetworkPolicy, D18)
- [x] 1.3 gVisor on arm64 working via `code/scripts/install-gvisor.sh` (the minikube add-on is broken, D21)
- [x] 1.4 Sandbox images: one Dockerfile, two targets, built inside minikube (D23)
- [x] 1.5 `code/deploy/dev/sandbox-pod.yaml` + `code/scripts/test-sandbox-ssh.sh`: 7 checks pass (SSH as user on a gVisor kernel, pip, npm, sudo, SFTP; root login and unknown keys rejected). **Finding:** `kubectl port-forward` can't reach gVisor pods (it dials loopback in the pod's network namespace, but gVisor's network stack lives inside the sandbox), so the test SSHes from a helper pod to the pod IP, the same path the API and gateway use (D6/D7)
- [x] 1.6 `code/scripts/cluster-up.sh` (minikube + gVisor + images, idempotent) and `cluster-down.sh [--delete]`. Verified from a deleted cluster: up in **94 s**, then all checks pass; sandbox pod ready in 2–6 s
- **Done when:** from a clean state, `cluster-up.sh` brings up the cluster and you can SSH into a gVisor-isolated Ubuntu pod. → **PR 1**

### Phase 2: Control plane API (~1.5–2 h) — branch `phase2/control-plane`
- [ ] 2.1 `go mod init` in `code/`; skeleton `cmd/api`, `internal/{config,db,auth,k8s,sshx,sandbox}`
- [ ] 2.2 Postgres in the cluster plus a schema: `users` (with `is_admin`), `api_keys` (stored hashed), `sandboxes` (id, owner, image, status, ssh_public_key, ttl, expires_at, last_activity)
- [ ] 2.3 API key auth middleware; a CLI command to bootstrap the first admin user and key
- [ ] 2.4 Sandbox lifecycle → Kubernetes pods via client-go: `POST/GET/LIST/DELETE /v1/sandboxes`; image limited to the 2 allowed (D2); quota of 10 per user (D17); TTL default 1 h, cap 24 h (D9)
- [ ] 2.5 Watch pod events to keep status accurate (`pending → running → deleted / failed / lost`, D8)
- [ ] 2.6 Platform-internal SSH key in a Kubernetes Secret; sandbox pods trust only that key (D7)
- [ ] 2.7 `POST /exec` over pooled SSH sessions (stdout, stderr, exit code, timeout); `PUT/GET /files` over SFTP (D7)
- [ ] 2.8 Reaper loop (~30 s): idle 30 min (from `last_activity`), TTL expiry
- [ ] 2.9 Unit tests for auth, quota and the reaper; `code/scripts/smoke-api.sh` (curl: create → exec → upload/download → delete)
- **Done when:** the smoke script passes against the local cluster. → **PR 2**

### Phase 3: SSH gateway (~1 h) — branch `phase3/ssh-gateway`
- [ ] 3.1 `cmd/gateway`: SSH server (`x/crypto/ssh`) with a persistent host key from a Secret; username = sandbox id
- [ ] 3.2 Auth: the client's public key must match the sandbox's stored `ssh_public_key` (D3) and the sandbox must be running
- [ ] 3.3 Proxy: dial the pod with the internal key and pipe shell, exec, PTY/window-resize and the `sftp` subsystem (so `ssh`, `scp` and `sftp` all work)
- [ ] 3.4 Update `last_activity` while sessions are open (so idle reaping respects SSH users)
- [ ] 3.5 Expose it locally (Service + `minikube tunnel` or port-forward); tests: correct key ✔, wrong key ✘, someone else's sandbox ✘, `scp` ✔
- **Done when:** `ssh sb-xxx@localhost -p <port>` drops you into your sandbox. → **PR 3**

### Phase 4: Dashboard (~1 h) — branch `phase4/dashboard`
- [ ] 4.1 `code/web/` templates + CSS embedded with `go:embed`, served at `/ui`; log in with an API key (session cookie)
- [ ] 4.2 Sandbox list (htmx auto-refresh), detail page (status, image, copyable SSH command, expires at), create form, delete button
- [ ] 4.3 API key management: create (shown once) and revoke
- [ ] 4.4 Admin view: all users and sandboxes, delete any sandbox
- **Done when:** everything the API can do for sandboxes and keys can be done from the browser. → **PR 4**

### Phase 5: Hardening + scale (~1 h) — branch `phase5/hardening`
- [ ] 5.1 NetworkPolicies per D18, with a test proving sandbox-to-sandbox and sandbox-to-Postgres are blocked while the internet works
- [ ] 5.2 Pod hardening: no service-account token, CPU/memory/ephemeral-storage limits, drop capabilities except those sshd needs
- [ ] 5.3 Image pre-pull DaemonSet (D19)
- [ ] 5.4 Run 2 API replicas; reaper made safe with a Postgres advisory lock, so only one replica reaps at a time
- [ ] 5.5 Load test script: create N sandboxes concurrently (as many as the laptop allows, e.g. 30–50); record p50/p95 startup time
- **Done when:** policies are verified by test and startup numbers are recorded. → **PR 5**

### Phase 6: Package + write-up (~1 h) — branch `phase6/package`
- [ ] 6.1 Helm chart in `code/deploy/helm/` (API, gateway, Postgres, RBAC, NetworkPolicies, pre-pull DaemonSet)
- [ ] 6.2 One-command flow: `up.sh` (cluster + build + install), `demo.sh` (end-to-end walkthrough), `down.sh`
- [ ] 6.3 `README.md`: quickstart, API reference, architecture diagram, measured startup numbers, uninstall steps
- [ ] 6.4 Write-up of trade-offs and next steps: gVisor vs. Kata/Firecracker, SSH certificates, in-sandbox daemon, warm pool, persistent disks / pause-resume, Tailscale (D16), cloud deployment plan (D10 step 2), Python-vs-Go note
- **Done when:** a reviewer can clone the repo, run the scripts and reproduce the demo. → **PR 6**

### Optional (outside the time budget)
- [ ] Cloud deployment (D10 step 2): user creates an AWS/Azure account; choose managed Kubernetes vs. k3s on VMs; Terraform; deploy, load test, tear down
- [ ] Tailscale sidecar for the gateway/API (D16 option 1)
