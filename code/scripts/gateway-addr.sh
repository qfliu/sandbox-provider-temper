# Sourced by scripts that SSH to sandboxes through the gateway from this Mac.
#
# gateway_addr prints "<host> <port>" for the SSH gateway:
#   1. SANDBOX_GATEWAY=host:port, if set;
#   2. the LoadBalancer's address, if the chart's external gateway Service is
#      a LoadBalancer (a cloud install, step 7);
#   3. otherwise minikube's IP and the Service's NodePort.
gateway_addr() {
	if [ -n "${SANDBOX_GATEWAY:-}" ]; then
		echo "${SANDBOX_GATEWAY%:*} ${SANDBOX_GATEWAY##*:}"
		return
	fi
	local type lb port node
	type=$(kubectl -n sandbox-system get svc sandbox-gateway-nodeport -o jsonpath='{.spec.type}') || return 1
	port=$(kubectl -n sandbox-system get svc sandbox-gateway-nodeport -o jsonpath='{.spec.ports[0].port}')
	if [ "$type" = LoadBalancer ]; then
		lb=$(kubectl -n sandbox-system get svc sandbox-gateway-nodeport \
			-o jsonpath='{.status.loadBalancer.ingress[0].ip}{.status.loadBalancer.ingress[0].hostname}')
		[ -n "$lb" ] || { echo "the gateway's LoadBalancer has no address yet" >&2; return 1; }
		echo "$lb $port"
	else
		node=$(kubectl -n sandbox-system get svc sandbox-gateway-nodeport -o jsonpath='{.spec.ports[0].nodePort}')
		echo "$(minikube ip) $node"
	fi
}
