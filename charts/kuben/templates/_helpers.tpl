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

{{- /* The pod of a backup: `kuben` next to PostgreSQL's client tools.
       Takes a dict: root (the chart context), claim (the backup volume) and
       command. */ -}}
{{- define "kuben.backup.pod" -}}
metadata:
  labels:
    {{- include "kuben.selectorLabels" .root | nindent 4 }}
    app.kubernetes.io/component: backup
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  enableServiceLinks: false
  {{- with .root.Values.imagePullSecrets }}
  imagePullSecrets:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    runAsGroup: 65532
    fsGroup: 65532
    seccompProfile:
      type: RuntimeDefault
  initContainers:
    # The release image has no shell: the binary copies itself.
    - name: kuben
      image: {{ include "kuben.image" .root }}
      imagePullPolicy: {{ .root.Values.image.pullPolicy }}
      args: ["copy-self", "/tools/kuben"]
      resources:
        requests: {cpu: 10m, memory: 16Mi}
        limits: {memory: 64Mi}
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: [ALL]
      volumeMounts:
        - name: tools
          mountPath: /tools
  containers:
    - name: backup
      image: {{ .root.Values.backup.image | default (include "kuben.postgresql.image" .root) }}
      imagePullPolicy: {{ .root.Values.postgresql.image.pullPolicy }}
      command:
        {{- toYaml .command | nindent 8 }}
      env:
        {{- include "kuben.databaseEnv" .root | trim | nindent 8 }}
        - name: KUBEN_SECRETS__KEYRING_FILE
          value: /etc/kuben/keyring/secrets.keyring
        - name: KUBEN_BACKUP__DIR
          value: /backups
        - name: KUBEN_BACKUP__KEEP
          value: {{ .root.Values.backup.keep | quote }}
        - name: KUBEN_SERVER__STATE_DIR
          value: /tmp/kuben
        - name: HOME
          value: /tmp
      resources:
        {{- toYaml .root.Values.backup.resources | nindent 8 }}
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: [ALL]
      volumeMounts:
        - name: tools
          mountPath: /tools
          readOnly: true
        - name: backups
          mountPath: /backups
        - name: keyring
          mountPath: /etc/kuben/keyring
          readOnly: true
        - name: tmp
          mountPath: /tmp
  volumes:
    - name: tools
      emptyDir:
        sizeLimit: 128Mi
    - name: tmp
      emptyDir:
        sizeLimit: 64Mi
    - name: backups
      persistentVolumeClaim:
        claimName: {{ .claim }}
    - name: keyring
      secret:
        secretName: {{ include "kuben.keyring.secret" .root }}
        defaultMode: 0400
{{- end -}}
