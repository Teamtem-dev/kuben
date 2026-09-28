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

{{/*
A value in the server's environment grammar (figment's, kept by the Go
config): strings quoted TOML-style, lists as `["a", "b"]`, maps as
`{"key" = "value"}`. JSON string quoting is valid TOML basic-string quoting.
*/}}
{{- define "kuben.envString" -}}
{{ . | toString | toJson | quote }}
{{- end }}

{{- define "kuben.envList" -}}
{{ . | default list | toJson | quote }}
{{- end }}

{{- define "kuben.envTable" -}}
{{- $pairs := list }}
{{- range $key, $value := . }}
{{- $pairs = append $pairs (printf "%s = %s" ($key | toJson) ($value | toString | toJson)) }}
{{- end }}
{{- printf "{%s}" (join ", " $pairs) | quote }}
{{- end }}

{{/*
Single sign-on, CI trust, Git sources, builds and integrations: `KUBEN_SSO__*`,
`KUBEN_CI__*`, `KUBEN_GIT__*`, `KUBEN_BUILD__*` and `KUBEN_INTEGRATIONS__*`. Secrets are files mounted
from existing Secrets (see kuben.platformVolumes) or `secretKeyRef`s; no
credential is written into the pod spec.
*/}}
{{- define "kuben.platformEnv" -}}
{{- $sso := .Values.sso }}
{{- if $sso.enabled }}
{{- if not (and $sso.issuer $sso.clientId $sso.existingSecret) }}
{{- fail "sso.enabled needs sso.issuer, sso.clientId and sso.existingSecret (a Secret with the client secret)" }}
{{- end }}
{{- if not .Values.publicUrl }}
{{- fail "sso.enabled needs publicUrl: the provider redirects back to <publicUrl>/api/v1/auth/sso/callback" }}
{{- end }}
- name: KUBEN_SSO__ENABLED
  value: "true"
- name: KUBEN_SSO__ISSUER
  value: {{ include "kuben.envString" $sso.issuer }}
- name: KUBEN_SSO__CLIENT_ID
  value: {{ include "kuben.envString" $sso.clientId }}
- name: KUBEN_SSO__CLIENT_SECRET_FILE
  value: /etc/kuben/sso/{{ $sso.clientSecretKey }}
- name: KUBEN_SSO__DISPLAY_NAME
  value: {{ include "kuben.envString" $sso.displayName }}
- name: KUBEN_SSO__SCOPES
  value: {{ include "kuben.envList" $sso.scopes }}
- name: KUBEN_SSO__GROUP_CLAIM
  value: {{ include "kuben.envString" $sso.groupClaim }}
- name: KUBEN_SSO__GROUPS
  value: {{ include "kuben.envTable" $sso.groups }}
{{- with $sso.defaultRole }}
- name: KUBEN_SSO__DEFAULT_ROLE
  value: {{ include "kuben.envString" . }}
{{- end }}
- name: KUBEN_SSO__ALLOWED_DOMAINS
  value: {{ include "kuben.envList" $sso.allowedDomains }}
- name: KUBEN_SSO__REQUIRE_VERIFIED_EMAIL
  value: {{ $sso.requireVerifiedEmail | toString | quote }}
{{- with $sso.org }}
- name: KUBEN_SSO__ORG
  value: {{ include "kuben.envString" . }}
{{- end }}
- name: KUBEN_SSO__DISABLE_PASSWORD_FOR_LINKED
  value: {{ $sso.disablePasswordForLinked | toString | quote }}
{{- end }}
{{- $gha := .Values.ci.githubActions }}
{{- if $gha.enabled }}
{{- if not (or $gha.audience .Values.publicUrl) }}
{{- fail "ci.githubActions.enabled needs publicUrl or ci.githubActions.audience: workflows request a token for that audience" }}
{{- end }}
- name: KUBEN_CI__GITHUB_ACTIONS
  value: "true"
{{- with $gha.audience }}
- name: KUBEN_CI__GITHUB_OIDC_AUDIENCE
  value: {{ include "kuben.envString" . }}
{{- end }}
{{- end }}
{{- $gh := .Values.git.github }}
{{- if $gh.appId }}
{{- if not $gh.existingSecret }}
{{- fail "git.github.appId needs git.github.existingSecret (a Secret with the App's private key and webhook secret)" }}
{{- end }}
- name: KUBEN_GIT__GITHUB_APP_ID
  value: {{ $gh.appId | toString | quote }}
- name: KUBEN_GIT__GITHUB_PRIVATE_KEY_FILE
  value: /etc/kuben/github/{{ $gh.privateKeyKey }}
- name: KUBEN_GIT__GITHUB_WEBHOOK_SECRET
  valueFrom:
    secretKeyRef:
      name: {{ $gh.existingSecret }}
      key: {{ $gh.webhookSecretKey }}
- name: KUBEN_GIT__GITHUB_API_URL
  value: {{ include "kuben.envString" $gh.apiUrl }}
- name: KUBEN_GIT__GITHUB_CLONE_URL
  value: {{ include "kuben.envString" $gh.cloneUrl }}
{{- end }}
{{- $b := .Values.build }}
{{- if $b.enabled }}
{{- /* Builds read sources through the GitHub App (git.github) and through
  the organizations' Git connections (2.1, runtime data sealed with the
  keyring), so build.enabled no longer needs git.github. */}}
- name: KUBEN_BUILD__ENABLED
  value: "true"
- name: KUBEN_BUILD__NAMESPACE
  value: {{ include "kuben.envString" ($b.namespace | default .Release.Namespace) }}
- name: KUBEN_BUILD__MAX_CONCURRENT
  value: {{ $b.maxConcurrent | toString | quote }}
- name: KUBEN_BUILD__MAX_CONCURRENT_PER_ORG
  value: {{ $b.maxConcurrentPerOrg | toString | quote }}
- name: KUBEN_BUILD__INSECURE_REGISTRY
  value: {{ $b.insecureRegistry | toString | quote }}
{{- range $key, $env := dict "buildkitImage" "BUILDKIT_IMAGE" "fetchImage" "FETCH_IMAGE" "scannerImage" "SCANNER_IMAGE" "railpackFrontend" "RAILPACK_FRONTEND" "railpackImage" "RAILPACK_IMAGE" "pushSecret" "PUSH_SECRET" "nodePool" "NODE_POOL" "cpuRequest" "CPU_REQUEST" "cpuLimit" "CPU_LIMIT" "memory" "MEMORY" "ephemeralStorage" "EPHEMERAL_STORAGE" }}
{{- with index $b $key }}
- name: KUBEN_BUILD__{{ $env }}
  value: {{ include "kuben.envString" . }}
{{- end }}
{{- end }}
{{- with $b.deadlineSeconds }}
- name: KUBEN_BUILD__DEADLINE_SECS
  value: {{ . | toString | quote }}
{{- end }}
{{- with $b.logTailKiB }}
{{- if or (lt (int .) 1) (gt (int .) 1024) }}
{{- fail "build.logTailKiB must be between 1 and 1024 (0 keeps the server's 256)" }}
{{- end }}
- name: KUBEN_BUILD__LOG_TAIL_KIB
  value: {{ . | toString | quote }}
{{- end }}
{{- end }}
{{- if .Values.integrations.allowPrivateHosts }}
- name: KUBEN_INTEGRATIONS__ALLOW_PRIVATE_HOSTS
  value: "true"
{{- end }}
{{- end }}

{{/* Mounts for kuben.platformEnv's files. */}}
{{- define "kuben.platformMounts" -}}
{{- if .Values.sso.enabled }}
- name: sso
  mountPath: /etc/kuben/sso
  readOnly: true
{{- end }}
{{- if .Values.git.github.appId }}
- name: github
  mountPath: /etc/kuben/github
  readOnly: true
{{- end }}
{{- end }}

{{- define "kuben.platformVolumes" -}}
{{- if .Values.sso.enabled }}
- name: sso
  secret:
    secretName: {{ .Values.sso.existingSecret }}
    defaultMode: 0400
    items:
      - key: {{ .Values.sso.clientSecretKey }}
        path: {{ .Values.sso.clientSecretKey }}
{{- end }}
{{- if .Values.git.github.appId }}
- name: github
  secret:
    secretName: {{ .Values.git.github.existingSecret }}
    defaultMode: 0400
    items:
      - key: {{ .Values.git.github.privateKeyKey }}
        path: {{ .Values.git.github.privateKeyKey }}
{{- end }}
{{- end }}
