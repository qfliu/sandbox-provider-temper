{{/* Labels Helm adds to everything it manages. */}}
{{- define "sp.labels" -}}
app.kubernetes.io/part-of: sandbox-provider
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{/* Platform image reference; the tag is required. */}}
{{- define "sp.image" -}}
{{ .Values.image.repository }}:{{ required "image.tag is required (the platform image's tag)" .Values.image.tag }}
{{- end -}}

{{/*
A Secret value that is generated once and then kept: the existing value if
the Secret is already in the cluster (an upgrade, or adopting what the dev
scripts created), otherwise the new one. Returns base64.
Args: dict "ns" "name" "key" "new"
*/}}
{{- define "sp.keep" -}}
{{- $s := lookup "v1" "Secret" .ns .name -}}
{{- if and $s $s.data (hasKey $s.data .key) -}}
{{ index $s.data .key }}
{{- else -}}
{{ .new | b64enc }}
{{- end -}}
{{- end -}}

{{/*
The Kubernetes API server's addresses that aren't in a private range, as a
YAML list of /32s: on AKS the `kubernetes` Service forwards to the control
plane's public IP, which the "public internet" egress rule would otherwise
let sandboxes reach. (Clusters whose `kubernetes` endpoints are private,
like minikube, get nothing added. Others with a public API endpoint that
isn't in these endpoints, which may include EKS, need it in
networkPolicies.extraBlockedRanges.) Read at install/upgrade time
(`lookup`, so empty under `helm template`): if the control plane's IP
changes, run the upgrade again; test-network-policy.sh's Kubernetes API
check notices.
*/}}
{{- define "sp.publicAPIServer" -}}
{{- $out := list -}}
{{- $ep := lookup "v1" "Endpoints" "default" "kubernetes" -}}
{{- range ($ep.subsets | default list) -}}
{{- range (.addresses | default list) -}}
{{- $ip := .ip -}}
{{- if not (or (hasPrefix "10." $ip) (hasPrefix "192.168." $ip) (regexMatch "^172\\.(1[6-9]|2[0-9]|3[01])\\." $ip) (regexMatch "^100\\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\\." $ip) (hasPrefix "127." $ip) (hasPrefix "169.254." $ip) (contains ":" $ip)) -}}
{{- $out = append $out (printf "%s/32" $ip) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- toYaml $out -}}
{{- end -}}

{{/* Who may reach Postgres (and the sandboxes' sshd): pod labels. */}}
{{- define "sp.platformApps" -}}
[sandbox-api, sandbox-gateway]
{{- end -}}
