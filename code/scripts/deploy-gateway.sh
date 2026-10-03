#!/usr/bin/env bash
# Builds the platform image (API + gateway) inside minikube and rolls out the
# SSH gateway to sandbox-system. Also creates the gateway's own narrow
# Postgres role: read sandboxes, write ssh_activity, nothing else (D36).
# Run after deploy-postgres.sh and deploy-api.sh (the API
# creates the tables). Idempotent.
#
# Usage (from the project root): source ./env.sh && ./code/scripts/deploy-gateway.sh
# Then reach it from your Mac:   ssh -p 30022 sb-…@$(minikube ip)   (NodePort; see the printed line)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
: "${KUBECONFIG:?source ./env.sh first}"
mkdir -p "$ROOT/.devenv/state/tmp"
DIRTY="$(git -C "$ROOT" diff --quiet HEAD -- code || echo -dirty)"
TAG="$(git -C "$ROOT" rev-parse --short HEAD)${DIRTY}-$(date +%s)"
LOG="$ROOT/.devenv/state/tmp/gateway-build.log"

echo "==> narrow database role for the gateway"
PWFILE="$ROOT/.devenv/state/postgres-gateway-password"
[ -s "$PWFILE" ] || (umask 077 && openssl rand -hex 24 | tr -d '\n' >"$PWFILE")
# The password reaches psql on stdin as a variable (:'pw' quotes it safely),
# never on a command line where `ps` could show it; the Secret is built from
# the file for the same reason. (Postgres still receives it inside the
# ALTER ROLE statement, as with any password change; statement logging is
# off by default.)
{
  printf "\\set pw '%s'\n" "$(cat "$PWFILE")"
  cat <<'SQL'
DO $$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'sandbox_gateway') THEN
    CREATE ROLE sandbox_gateway LOGIN;
  END IF;
END $$;
ALTER ROLE sandbox_gateway PASSWORD :'pw';
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM sandbox_gateway;
GRANT USAGE ON SCHEMA public TO sandbox_gateway;
GRANT SELECT ON sandboxes TO sandbox_gateway;
-- SSH sessions count as activity for the idle reaper (D36): the gateway
-- writes its own table and can't change the sandboxes table at all (REVOKE
-- ALL above also drops any earlier grant). No function borrows anyone's
-- rights for it. SELECT is needed for INSERT … ON CONFLICT DO UPDATE.
GRANT SELECT, INSERT, UPDATE ON ssh_activity TO sandbox_gateway;
SQL
} | kubectl -n sandbox-system exec -i postgres-0 -- psql -q -v ON_ERROR_STOP=1 -U sandbox -d sandbox >/dev/null
kubectl -n sandbox-system create secret generic postgres-gateway-credentials \
  --from-file=password="$PWFILE" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

echo "==> building sandbox/api:$TAG (API + gateway)"
minikube image build -t "sandbox/api:$TAG" "$ROOT/code" >"$LOG" 2>&1 \
  || { tail -30 "$LOG"; echo "image build failed; full log: $LOG" >&2; exit 1; }

echo "==> deploying"
sed "s|sandbox/api:IMAGE_TAG|sandbox/api:$TAG|" "$ROOT/code/deploy/dev/gateway.yaml" | kubectl apply -f - >/dev/null
kubectl -n sandbox-system rollout status deployment/sandbox-gateway --timeout=180s >/dev/null
kubectl -n sandbox-system get pods -l app=sandbox-gateway -o wide | cut -c1-90
echo "==> sandbox-gateway ready (in-cluster: sandbox-system/sandbox-gateway:22)"
GW_IP=$(minikube ip)
echo "    from this Mac: ssh -p 30022 <sandbox-id>@$GW_IP"
echo "    gateway host key: $(ssh-keygen -lf "$ROOT/.devenv/state/gateway-host-key.pub" | cut -d' ' -f2)"
echo "    to pin it instead of trusting the first connection, add to ~/.ssh/known_hosts:"
echo "      [$GW_IP]:30022 $(cut -d' ' -f1,2 "$ROOT/.devenv/state/gateway-host-key.pub")"
