#!/usr/bin/env bash
# Deploys Postgres into the local cluster (namespace sandbox-system).
# The password is generated once and kept in .devenv/state/postgres-password
# so it is never committed. Idempotent.
#
# Usage (from the project root): source ./env.sh && ./code/scripts/deploy-postgres.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
PWFILE="$ROOT/.devenv/state/postgres-password"

[ -s "$PWFILE" ] || openssl rand -hex 24 > "$PWFILE"

kubectl apply -f - >/dev/null <<'EOF'
apiVersion: v1
kind: Namespace
metadata: {name: sandbox-system}
EOF
kubectl -n sandbox-system create secret generic postgres-credentials \
  --from-literal=username=sandbox --from-file=password="$PWFILE" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null

kubectl apply -f "$ROOT/code/deploy/dev/postgres.yaml" >/dev/null
kubectl -n sandbox-system rollout status statefulset/postgres --timeout=180s >/dev/null
echo "==> postgres ready (sandbox-system/postgres:5432, db=sandbox, user=sandbox)"
