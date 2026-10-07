# Sourced by up.sh and cloud-up.sh (needs kubectl on the target cluster and
# TMPDIR from env.sh).
#
# ensure_admin_key <keyfile> <key label>: makes sure <keyfile> holds a
# working API key for the user "admin" on the current cluster, never
# printing it: creates the user and its first key on a fresh database;
# keeps a saved key if it still authenticates; otherwise (revoked, from
# another database, or none saved) issues a new key labelled <key label>.
ensure_admin_key() {
	local keyfile=$1 label=$2 out key
	if out=$(_admin_api create-user --name admin --admin); then
		:
	elif grep -q "already exists" <<<"$out" && [ -s "$keyfile" ] && _admin_key_works "$keyfile"; then
		out="" # same database, saved key still valid
	elif grep -q "already exists" <<<"$out"; then
		out=$(_admin_api create-key --user admin --name "$label") || { echo "$out" >&2; return 1; }
	else
		echo "$out" >&2
		return 1
	fi
	if [ -n "$out" ]; then
		key=$(grep -o 'sk_[A-Za-z0-9_-]*' <<<"$out" | head -1) || true
		[ -n "$key" ] || { echo "no key in the output of /api (see above)" >&2; return 1; }
		mkdir -p "$(dirname "$keyfile")"
		(umask 077 && printf '%s\n' "$key" >"$keyfile")
		echo "    new key saved to $keyfile"
	else
		echo "    existing key in $keyfile"
	fi
}

_admin_api() { kubectl -n sandbox-system exec deploy/sandbox-api -- /api "$@" 2>&1; }

# _admin_key_works <keyfile>: GET /v1/me with the saved key, through a
# port-forward of our own. The key goes to curl in a header file, never on
# a command line.
_admin_key_works() {
	local keyfile=$1 port=18091 pf hdr code log
	log=$(mktemp "$TMPDIR/pf.XXXXXX")
	# 127.0.0.1 only, so a port already in use fails instead of half-working.
	kubectl -n sandbox-system port-forward --address 127.0.0.1 svc/sandbox-api "$port:80" >"$log" 2>&1 &
	pf=$!
	for _ in $(seq 1 40); do
		grep -q "Forwarding from" "$log" && break
		kill -0 "$pf" 2>/dev/null || break
		sleep 0.25
	done
	rm -f "$log"
	if ! kill -0 "$pf" 2>/dev/null; then
		echo "    (couldn't check the saved key: port $port is in use)"
		return 0
	fi
	hdr=$(mktemp "$TMPDIR/auth.XXXXXX")
	{ printf 'Authorization: Bearer '; tr -d '\n' <"$keyfile"; echo; } >"$hdr"
	code=$(curl -s -m 5 -o /dev/null -w '%{http_code}' -H @"$hdr" "http://127.0.0.1:$port/v1/me")
	rm -f "$hdr"
	kill "$pf" 2>/dev/null
	wait "$pf" 2>/dev/null
	[ "$code" = 200 ]
}
