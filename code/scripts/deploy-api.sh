#!/usr/bin/env bash
# Builds the API image inside minikube and rolls it out to sandbox-system,
# after making sure its database role (sandbox_app, not a superuser) exists.
# Each build gets a unique tag, so Kubernetes always picks up the new code.
#
# Usage (from the project root): source ./env.sh && ./code/scripts/deploy-api.sh
# Then reach it from your Mac:   kubectl -n sandbox-system port-forward svc/sandbox-api 18080:80
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
# <commit>[-dirty]-<time>: "-dirty" flags builds with uncommitted changes, so
# the commit in the tag never misdescribes what's running.
DIRTY="$(git -C "$ROOT" diff --quiet HEAD -- code || echo -dirty)"
TAG="$(git -C "$ROOT" rev-parse --short HEAD)${DIRTY}-$(date +%s)"
LOG="$ROOT/.devenv/state/tmp/api-build.log"

echo "==> building sandbox/api:$TAG"
minikube image build -t "sandbox/api:$TAG" "$ROOT/code" >"$LOG" 2>&1 \
  || { tail -30 "$LOG"; echo "API image build failed; full log: $LOG" >&2; exit 1; }

echo "==> platform database role (not a superuser, D47)"
# The API connects as sandbox_app: it owns the sandbox database and the
# platform's tables (so it can run its migrations) but isn't a superuser,
# so an SQL bug in the API can't take over the database server. The
# superuser "sandbox" is only used here and by the deploy scripts. Idempotent.
APP_PWFILE="$ROOT/.devenv/state/postgres-app-password"
[ -s "$APP_PWFILE" ] || (umask 077 && openssl rand -hex 24 | tr -d '\n' >"$APP_PWFILE")
{
  printf "\\set pw '%s'\n" "$(cat "$APP_PWFILE")"
  cat <<'SQL'
DO $$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'sandbox_app') THEN
    CREATE ROLE sandbox_app LOGIN;
  END IF;
END $$;
ALTER ROLE sandbox_app LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD :'pw';
-- Owning the database makes it pg_database_owner, which owns schema public
-- (Postgres 15+), so new migrations can create tables there.
ALTER DATABASE sandbox OWNER TO sandbox_app;
-- Hand over what earlier deploys created as the superuser (tables bring
-- their indexes; ACLs such as the gateway's grants are kept).
DO $$ DECLARE r record; BEGIN
  FOR r IN
    SELECT format('ALTER %s %I.%I OWNER TO sandbox_app',
                  CASE c.relkind WHEN 'r' THEN 'TABLE' WHEN 'S' THEN 'SEQUENCE'
                                 WHEN 'v' THEN 'VIEW' ELSE 'MATERIALIZED VIEW' END,
                  n.nspname, c.relname) AS stmt
    FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public' AND c.relkind IN ('r', 'S', 'v', 'm')
      AND pg_get_userbyid(c.relowner) <> 'sandbox_app'
  LOOP EXECUTE r.stmt; END LOOP;
  FOR r IN
    SELECT format('ALTER FUNCTION %s OWNER TO sandbox_app', p.oid::regprocedure) AS stmt
    FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
    WHERE n.nspname = 'public' AND pg_get_userbyid(p.proowner) <> 'sandbox_app'
  LOOP EXECUTE r.stmt; END LOOP;
END $$;
SQL
} | kubectl -n sandbox-system exec -i postgres-0 -- psql -q -v ON_ERROR_STOP=1 -U sandbox -d sandbox >/dev/null
kubectl -n sandbox-system create secret generic postgres-app-credentials \
  --from-file=password="$APP_PWFILE" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

echo "==> deploying"
# The dashboard shows users how to SSH in through the gateway's NodePort (D38).
# (Assigned first so a failing `minikube ip` stops the script under set -e.)
GATEWAY_IP=$(minikube ip)
sed -e "s|sandbox/api:IMAGE_TAG|sandbox/api:$TAG|" \
    -e "s|\"SSH_GATEWAY_ADDR\"|\"$GATEWAY_IP:30022\"|" \
    "$ROOT/code/deploy/dev/api.yaml" | kubectl apply -f - >/dev/null
kubectl -n sandbox-system rollout status deployment/sandbox-api --timeout=180s >/dev/null
kubectl -n sandbox-system get pods -l app=sandbox-api -o wide | cut -c1-90
echo "==> sandbox-api ready (sandbox-system/sandbox-api:80)"
echo "    dashboard: kubectl -n sandbox-system port-forward svc/sandbox-api 18080:80"
echo "               then open http://localhost:18080/ui and sign in with an API key"
