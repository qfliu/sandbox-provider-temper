#!/usr/bin/env bash
# Measures what the image pre-pull (D46) saves (step 6.5): how long a pod
# using the ubuntu-dev sandbox image takes to start on a node that doesn't
# have it (it must download ~218 MB from a registry) versus one that does
# (what sandbox-image-prepull guarantees).
#
# minikube builds the images on the node, so this pushes them to minikube's
# registry addon, removes every cached copy from the node, times the cold
# download on the node (a containerd pull, so the kubelet keeps no pull
# record under the registry's name), restores the images under their usual names,
# then times a pod start with the image cached. To make the node really
# cold it also prunes the BuildKit cache. Cold start ≈ download +
# warm start. Finally it restores the pre-pull DaemonSet and the registry
# addon's previous state. Sandboxes can't start while it runs (about a
# minute); it refuses to run if any do.
#
# Image names are only ever removed with `ctr images rm` (by name), never
# `crictl rmi`, which deletes the image with all its names.
#
# Usage (from the project root): source ./env.sh && ./code/scripts/measure-prepull.sh
set -uo pipefail

: "${KUBECONFIG:?source ./env.sh first}"
NS=sandbox-system
REG=localhost:5000
IMAGES="ubuntu-base ubuntu-dev"
node() { minikube ssh -- "$@" </dev/null 2>/dev/null | tr -d '\r'; }

[ "$(kubectl -n sandboxes get pods --no-headers 2>/dev/null | wc -l | tr -d ' ')" = 0 ] ||
	{ echo "sandboxes are running; run this on an idle cluster" >&2; exit 1; }

REG_WAS_ON=$(minikube addons list -o json 2>/dev/null | python3 -c "import json,sys; print(json.load(sys.stdin)['registry']['Status'])")
case "$REG_WAS_ON" in enabled | disabled) ;; *) echo "can't read the registry addon's state" >&2; exit 1 ;; esac
# drop_names <grep pattern> [--sync]: removes every image name (tags and
# the @sha256 names a pull adds) matching the pattern; content goes only
# once no name refers to it.
drop_names() {
	local names
	names=$(node "sudo ctr -n k8s.io images ls -q" | grep -- "$1" | tr '\n' ' ')
	[ -z "$names" ] || node "sudo ctr -n k8s.io images rm ${2:-} $names >/dev/null 2>&1"
}
# put_back: the sandbox images under their usual names (from the registry
# copy if the node lost them), and no registry-named copy left behind.
# ctr pull, not crictl (see the timing step below).
put_back() {
	for img in $IMAGES; do
		node "sudo crictl inspecti docker.io/sandbox/$img:0.3 >/dev/null 2>&1 || { sudo ctr -n k8s.io images pull --plain-http $REG/sandbox/$img:0.3 >/dev/null && sudo ctr -n k8s.io images tag --force $REG/sandbox/$img:0.3 docker.io/sandbox/$img:0.3 >/dev/null; }"
	done
	drop_names "^$REG/sandbox/"
}
restore() {
	echo "== restoring"
	kubectl -n "$NS" delete pod prepull-timing --ignore-not-found --wait=true >/dev/null 2>&1
	put_back
	for img in $IMAGES; do
		if node "sudo crictl inspecti docker.io/sandbox/$img:0.3 >/dev/null 2>&1"; then
			echo "   docker.io/sandbox/$img:0.3 present"
		else
			echo "   docker.io/sandbox/$img:0.3 MISSING: rebuild with cluster-up.sh" >&2
		fi
	done
	# The DaemonSet as it was (Helm's labels and annotations included), so
	# the release still owns it.
	if [ -s "$SAVED_DS" ] && ! kubectl -n "$NS" get daemonset sandbox-image-prepull >/dev/null 2>&1; then
		kubectl create -f "$SAVED_DS" >/dev/null && echo "   pre-pull DaemonSet restored"
	fi
	rm -f "$SAVED_DS"
	[ "$REG_WAS_ON" = enabled ] || { minikube addons disable registry >/dev/null 2>&1 && echo "   registry addon disabled again"; }
}
SAVED_DS=$(mktemp)
kubectl -n "$NS" get daemonset sandbox-image-prepull -o json | python3 -c '
import json, sys
d = json.load(sys.stdin)
for k in ("resourceVersion", "uid", "creationTimestamp", "generation", "managedFields"):
    d["metadata"].pop(k, None)
d.pop("status", None)
json.dump(d, sys.stdout)' >"$SAVED_DS" || { echo "no pre-pull DaemonSet to measure against (run deploy.sh)" >&2; exit 1; }
trap restore EXIT

echo "== registry"
# A registry pod still terminating from the last run would answer the
# readiness check below and then vanish mid-push.
kubectl -n kube-system wait --for=delete pod -l kubernetes.io/minikube-addons=registry --timeout=120s >/dev/null 2>&1
minikube addons enable registry >/dev/null 2>&1
for _ in $(seq 1 30); do node "curl -s -m 2 http://localhost:5000/v2/" | grep '{}' >/dev/null && break; sleep 2; done
for img in $IMAGES; do # retried: a just re-enabled registry can refuse pushes for a few seconds
	for try in 1 2 3 4 5 6; do
		out=$(minikube ssh -- "sudo ctr -n k8s.io images tag --force docker.io/sandbox/$img:0.3 $REG/sandbox/$img:0.3 >/dev/null && sudo ctr -n k8s.io images push --plain-http $REG/sandbox/$img:0.3 >/dev/null && echo pushed-ok" </dev/null 2>&1 | tr -d '\r')
		grep -q pushed-ok <<<"$out" && break
		if [ "$try" = 6 ]; then
			echo "push of $img failed: $out" >&2
			grep -q "content digest .* not found" <<<"$out" && echo "(the node lost the image's compressed layers: rebuild the images with cluster-up.sh)" >&2
			exit 1
		fi
		sleep 5
	done
done
echo "   pushed: $(node "curl -s http://localhost:5000/v2/_catalog")"

echo "== clearing the node (no cached sandbox image at all, like a fresh node)"
kubectl -n "$NS" delete daemonset sandbox-image-prepull --wait=true >/dev/null 2>&1
kubectl -n "$NS" wait --for=delete pod -l app=sandbox-image-prepull --timeout=60s >/dev/null 2>&1
# Its exited init containers outlive the pod for a while (the kubelet
# removes them lazily) and keep the layers' snapshots, which would make the
# pull below skip the download. Wait for them, then remove what's left.
for _ in $(seq 1 15); do
	[ "$(node "sudo ctr -n k8s.io containers ls" | grep -c 'sandbox/ubuntu-')" = 0 ] && break
	sleep 2
done
node "sudo ctr -n k8s.io containers ls" | awk '/sandbox\/ubuntu-/ {print $1}' |
	while read -r c; do node "sudo crictl rm -f $c >/dev/null 2>&1"; done
[ "$(node "sudo ctr -n k8s.io containers ls" | grep -c 'sandbox/ubuntu-')" = 0 ] ||
	{ echo "containers using the sandbox images are still on the node" >&2; exit 1; }
# ubuntu-dev's layer blobs, to check they're really gone afterwards.
MANIFEST=$(node "sudo ctr -n k8s.io images ls" | awk '$1=="docker.io/sandbox/ubuntu-dev:0.3"{print $3}')
LAYERS=$(node "sudo ctr -n k8s.io content get $MANIFEST" |
	python3 -c "import json,sys; print(' '.join(l['digest'] for l in json.load(sys.stdin)['layers']))")
[ -n "$LAYERS" ] || { echo "can't read ubuntu-dev's manifest" >&2; exit 1; }
# minikube image build's BuildKit cache holds leases on the same layers, so
# they'd stay after the names go. Pruning it only makes the next image
# build slower (and frees disk).
node "sudo buildctl prune --all >/dev/null 2>&1"
# Old builds of the sandbox images stay on the node as dangling images
# named by their ID (sha256:…) and share the same layers; drop the ones
# whose manifest uses ubuntu-dev's base layer too.
BASE=${LAYERS%% *}
old=$(node "sudo ctr -n k8s.io images ls" | awk 'NR>1 && $1 ~ /^sha256:/ {print $1, $3}' |
	while read -r name target; do
		node "sudo ctr -n k8s.io content get $target" | grep "$BASE" >/dev/null && printf '%s ' "$name"
	done)
[ -z "$old" ] || node "sudo ctr -n k8s.io images rm $old >/dev/null 2>&1"
drop_names 'sandbox/ubuntu-' --sync # --sync: wait until containerd deletes the unreferenced layers
left=$(node "sudo ctr -n k8s.io images ls -q" | grep -c 'sandbox/ubuntu-')
[ "$left" = 0 ] || { echo "sandbox images still named on the node ($left)" >&2; exit 1; }
kept=$(node "sudo ctr -n k8s.io content ls -q" | grep -cxF "$(tr ' ' '\n' <<<"$LAYERS")")
[ "$kept" = 0 ] || { echo "$kept of ubuntu-dev's layers are still on the node (another image shares them?)" >&2; exit 1; }
echo "   no sandbox image or layer on the node"

echo "== timing (ubuntu-dev, ~218 MB)"
# ctr, not crictl: CRI would drop the compressed layers after unpacking,
# and the restore's pull of ubuntu-base would then skip the shared ones
# (their snapshots exist), leaving images the next run can't push.
# Timed on the node, so ssh round trips don't count.
out=$(minikube ssh -- "s=\$(date +%s.%N); sudo ctr -n k8s.io images pull --plain-http $REG/sandbox/ubuntu-dev:0.3 >/tmp/pull.log 2>&1 && echo took \$s \$(date +%s.%N) || cat /tmp/pull.log" </dev/null 2>&1 | tr -d '\r')
read -r took s e <<<"$(grep '^took ' <<<"$out")"
[ "${took:-}" = took ] || { echo "pull failed: $out" >&2; exit 1; }
PULL=$(python3 -c "print(round($e-$s, 1))")
printf "   cold download + unpack on the node:      %5.1f s\n" "$PULL"
put_back # usual names back before any pod uses them

kubectl -n "$NS" delete pod prepull-timing --ignore-not-found --wait=true >/dev/null 2>&1
T0=$(python3 -c 'import time; print(time.time())')
kubectl -n "$NS" run prepull-timing --image=sandbox/ubuntu-dev:0.3 --image-pull-policy=IfNotPresent \
	--restart=Never --command -- sleep 600 >/dev/null
kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Running pod/prepull-timing --timeout=120s >/dev/null ||
	{ echo "pod didn't start" >&2; exit 1; }
WARM=$(python3 -c "import time; print(round(time.time()-$T0, 1))")
printf "   warm pod start (image cached):           %5.1f s   (%s)\n" "$WARM" \
	"$(kubectl -n "$NS" get events --field-selector involvedObject.name=prepull-timing \
		-o jsonpath='{range .items[*]}{.reason}: {.message}{"\n"}{end}' | grep '^Pulled' | tail -1 | cut -c1-70)"
printf "   => a node without pre-pull: ~%.1f s;  with pre-pull: %.1f s\n" "$(python3 -c "print($PULL+$WARM)")" "$WARM"
