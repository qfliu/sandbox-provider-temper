# Sourced by scripts that SSH to sandboxes through the gateway from this Mac.
#
# gateway_addr prints "<host> <port>" for the SSH gateway:
#   1. SANDBOX_GATEWAY=host:port (or [ipv6]:port), if set;
#   2. the LoadBalancer's address, if the chart's external gateway Service is
#      a LoadBalancer (a cloud install, step 7);
#   3. otherwise minikube's IP and the Service's NodePort.
gateway_addr() {
	if [ -n "${SANDBOX_GATEWAY:-}" ]; then
		local host=${SANDBOX_GATEWAY%:*} port=${SANDBOX_GATEWAY##*:}
		host=${host#\[}
		host=${host%\]}
		case "$port" in
		'' | *[!0-9]*) echo "SANDBOX_GATEWAY must be host:port or [ipv6]:port, got $SANDBOX_GATEWAY" >&2; return 1 ;;
		esac
		case "$SANDBOX_GATEWAY" in \[*\]:*) ;; *:*:*) host="" ;; esac # bare IPv6: ambiguous, needs [ ]
		[ -n "$host" ] && [ "$host" != "$SANDBOX_GATEWAY" ] ||
			{ echo "SANDBOX_GATEWAY must be host:port or [ipv6]:port, got $SANDBOX_GATEWAY" >&2; return 1; }
		echo "$host $port"
		return
	fi
	local svc=(-n sandbox-system get svc sandbox-gateway-nodeport -o) type lb port node ip
	type=$(kubectl "${svc[@]}" jsonpath='{.spec.type}') || return 1
	port=$(kubectl "${svc[@]}" jsonpath='{.spec.ports[0].port}') || return 1
	if [ "$type" = LoadBalancer ]; then
		lb=$(kubectl "${svc[@]}" jsonpath='{.status.loadBalancer.ingress[0].ip}{.status.loadBalancer.ingress[0].hostname}') || return 1
		[ -n "$lb" ] || { echo "the gateway's LoadBalancer has no address yet" >&2; return 1; }
		echo "$lb $port"
	else
		node=$(kubectl "${svc[@]}" jsonpath='{.spec.ports[0].nodePort}') || return 1
		ip=$(minikube ip) || return 1
		echo "$ip $node"
	fi
}
