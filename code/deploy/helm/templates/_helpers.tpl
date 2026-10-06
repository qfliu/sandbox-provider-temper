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

{{/* Who may reach Postgres (and the sandboxes' sshd): pod labels. */}}
{{- define "sp.platformApps" -}}
[sandbox-api, sandbox-gateway]
{{- end -}}
