{{- define "kuben.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "kuben.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "kuben.labels" -}}
{{ include "kuben.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/part-of: kuben
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "kuben.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}

{{- define "kuben.postgresql.fullname" -}}
{{- printf "%s-postgresql" (include "kuben.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- /* Non-empty when the chart runs its own PostgreSQL: no external database given. */ -}}
{{- define "kuben.postgresql.bundled" -}}
{{- if not (or .Values.database.url .Values.database.existingSecret) -}}true{{- end -}}
{{- end -}}

{{- define "kuben.postgresql.selectorLabels" -}}
app.kubernetes.io/name: postgresql
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: database
{{- end -}}

{{- define "kuben.postgresql.image" -}}
{{ .Values.postgresql.image.repository }}:{{ .Values.postgresql.image.tag }}
{{- end -}}
