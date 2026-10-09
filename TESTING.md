# Testing the sandbox provider

A guide for testers of a running deployment. For what the project is, read
[SUMMARY.md](SUMMARY.md) first (five minutes). No deployment from us? Run it
on a Mac with `./up.sh` ([README quickstart](README.md#quickstart-macos)).
The API, dashboard, Docker and port URLs work the same there; Kata needs the
Azure cluster. Locally, `$SANDBOX` is `http://localhost:18080` after the
README's port-forward, your key is the admin key, there's no allowlist, and
SSH goes to `$(minikube ip)` on port 30022 (`-p 30022`).

For a running deployment, you need three things from the person running it:

- **The address**, like `https://sandbox-1a2b3c4d.westus2.cloudapp.azure.com`.
- **Your API key** (`sk_…`). It's personal: don't share it, and don't paste
  it into chats or tickets.
- **Whether your IP must be allowlisted.** If it is and yours isn't, every
  HTTPS request returns `403 forbidden: this address isn't on the
  allowlist`, and SSH connections just time out.

The deployment is temporary: it runs for a test session and is then deleted,
with everything in it.

## 1. Check you can reach it

```bash
export SANDBOX=https://sandbox-1a2b3c4d.westus2.cloudapp.azure.com   # the address you were given
curl -s $SANDBOX/healthz                                              # {"status":"ok"}
```

Put your key in a header file, so it doesn't show up in your shell
history or in `ps`:

```bash
printf 'Authorization: Bearer %s\n' 'sk_YOUR_KEY' > ~/.sandbox-auth && chmod 600 ~/.sandbox-auth
curl -s -H @$HOME/.sandbox-auth $SANDBOX/v1/me                        # your user, as JSON
```

## 2. The dashboard

Open `$SANDBOX/ui` in a browser and sign in with your key. You'll see your
sandboxes (create them, run a command in a running one from its page, delete
them) and your API keys. When the cluster offers both, the create form has
a Runtime choice (gVisor or Kata). Lists don't
refresh on their own; use the Refresh link.

## 3. A sandbox through the API (what an AI agent does)

```bash
ssh-keygen -t ed25519 -N "" -f ~/.ssh/sandbox-test      # a key pair for SSH (section 4)

# Create one: image ubuntu-base, ubuntu-dev (Python, Node.js) or ubuntu-docker
# (plus Docker and Compose), 10-minute lifetime. Add "runtime":"kata" for a
# sandbox in its own VM (gVisor is the default).
curl -s -H @$HOME/.sandbox-auth -X POST $SANDBOX/v1/sandboxes \
  -d "{\"image\":\"ubuntu-dev\",\"ttl_seconds\":600,\"ssh_public_key\":\"$(cat ~/.ssh/sandbox-test.pub)\"}"
# → {"id":"sb-…","status":"pending",…}
export ID=sb-…                                           # the id from the response

curl -s -H @$HOME/.sandbox-auth $SANDBOX/v1/sandboxes/$ID       # repeat until "status":"running" (a few seconds)

# Run commands (60 s timeout by default, up to 600):
curl -s -H @$HOME/.sandbox-auth -X POST $SANDBOX/v1/sandboxes/$ID/exec -d '{"command":"uname -a; whoami; python3 --version"}'

# Files: upload, use, download (up to 100 MiB per upload; paths are relative to /home/user):
echo 'print(sum(range(10)))' > job.py
curl -s -H @$HOME/.sandbox-auth -X PUT --data-binary @job.py "$SANDBOX/v1/sandboxes/$ID/files?path=job.py"
curl -s -H @$HOME/.sandbox-auth -X POST $SANDBOX/v1/sandboxes/$ID/exec -d '{"command":"python3 job.py > out.txt"}'
curl -s -H @$HOME/.sandbox-auth "$SANDBOX/v1/sandboxes/$ID/files?path=out.txt"

# A server inside, reached from outside through the API (any port). Port
# URLs need your key in the Authorization header like every call, so use
# curl (or a tool that sets headers), not a browser; WebSockets work too.
curl -s -H @$HOME/.sandbox-auth -X POST $SANDBOX/v1/sandboxes/$ID/exec \
  -d '{"command":"(setsid python3 -m http.server 8000 >/dev/null 2>&1 &); sleep 1"}'
curl -s -H @$HOME/.sandbox-auth "$SANDBOX/v1/sandboxes/$ID/ports/8000/"

# List yours (add ?include_ended=true for ended ones), then delete:
curl -s -H @$HOME/.sandbox-auth $SANDBOX/v1/sandboxes
curl -s -H @$HOME/.sandbox-auth -X DELETE $SANDBOX/v1/sandboxes/$ID
```

## 4. SSH in

You'll also be given the SSH gateway's address. The sandbox's page in the
dashboard shows the exact command:

```bash
ssh -i ~/.ssh/sandbox-test -o IdentitiesOnly=yes $ID@<gateway address>
```

The username is the sandbox's id; once in, you're `user`. Name the key with
`-i` unless it's one ssh offers on its own (such as `~/.ssh/id_ed25519`).
`IdentitiesOnly=yes` stops ssh offering other keys first: with many keys in
ssh-agent the gateway would otherwise give up ("Too many authentication
failures"). The first connection asks you to accept the gateway's host key.
To check it instead, ask the person running the deployment for the line to
put in `~/.ssh/known_hosts`. `scp` and `sftp` work the same way (same `-i`
and `-o` options).

## 5. Things worth trying

- **Isolation.** The sandbox has internet access but nothing else. From
  inside it, try to reach the Kubernetes API (`curl -k
  https://kubernetes.default.svc`), the cloud metadata service (`curl
  http://169.254.169.254/`), other sandboxes, or private addresses. All of
  them should time out. `uname -r` shows the gVisor kernel (or, with
  `"runtime":"kata"`, the sandbox VM's own Linux kernel), not the host's.
- **Inside the sandbox:** `sudo` works (you're `user`), and package installs
  work: `pip` (in a virtualenv) and `npm` (in `ubuntu-dev`).
- **Docker** (`ubuntu-docker`): `docker run`, `docker build`, `docker run -p
  8080:80 nginx` then `curl localhost:8080`, and `docker compose up` with
  `ports:` all work as `user`. Docker's storage is in the sandbox's memory
  (2 GB of its 4 GB), so very large images or builds can run out ("No space
  left on device"; `docker system prune` frees it). `ping` to the internet
  doesn't work on this cluster (Azure's outbound load balancer carries no
  ICMP). A nested `--privileged` container starts but gets no more than the
  sandbox's own capabilities.
- **Other people's sandboxes:** another tester's sandbox id returns `404`,
  and SSH with your key to their sandbox is refused.
- **Limits:**
  - **Quota:** 10 live sandboxes per user; the 11th returns `429`.
  - **Lifetime (`ttl_seconds`):** 60 s to 24 h.
  - **Idle:** a sandbox with no exec, file transfer or SSH session for 30
    minutes is deleted. Status polling doesn't count as activity.
  - **Exec timeout:** a command past its timeout returns
    `"timed_out": true`.
- **Capacity.** This test cluster is small (2 nodes): about 20 sandboxes at
  once across all testers under gVisor, about half that under Kata (each Kata
  VM reserves more). Past that, new ones stay `pending` until others end.
  That's expected, not a bug.

## 6. Reporting a problem

Include:

- the time;
- the request (method, path and body, with **no key**) and the response;
- the sandbox id;
- for SSH, the output of `ssh -v`.

Status codes:

| Code | Meaning |
|---|---|
| 400 | bad input |
| 401 | missing or wrong key |
| 403 | allowlist, or file permissions inside the sandbox |
| 404 | not found or not yours |
| 409 | not running yet or anymore |
| 413 | upload too large |
| 429 | quota, or too many port-URL requests to one sandbox at once (64) |
| 502 | the sandbox didn't answer, or nothing listens on that port (port URLs) |
| 503 | the cluster is busy |
