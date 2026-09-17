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
{{ .Values.postgresql.image.repository }}:{{ .Values.postgresql.image.tag }}{{ with .Values.postgresql.image.digest }}@{{ . }}{{ end }}
{{- end -}}

{{- /* The Secret with the managed-secret keyring. */ -}}
{{- define "kuben.keyring.secret" -}}
{{- .Values.secrets.existingKeyringSecret | default (printf "%s-secrets-keyring" (include "kuben.fullname" .)) -}}
{{- end -}}

{{- /* KUBEN_DATABASE__URL (and what it needs) for Kuben's containers. */ -}}
{{- define "kuben.databaseEnv" -}}
{{- if .Values.database.existingSecret }}
- name: KUBEN_DATABASE__URL
  valueFrom:
    secretKeyRef:
      name: {{ .Values.database.existingSecret }}
      key: url
{{- else if .Values.database.url }}
- name: KUBEN_DATABASE__URL
  value: {{ .Values.database.url | quote }}
{{- else }}
# The chart's own PostgreSQL, as the ordinary role `kuben`; the password
# stays in its Secret.
- name: POSTGRES_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ include "kuben.postgresql.fullname" . }}
      key: password
- name: KUBEN_DATABASE__URL
  value: "postgres://kuben:$(POSTGRES_PASSWORD)@{{ include "kuben.postgresql.fullname" . }}:5432/kuben"
{{- end }}
{{- end -}}
