{{/*
Base name of the chart, overridable with nameOverride
*/}}
{{- define "provino.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified name used for the resources this chart creates
*/}}
{{- define "provino.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "provino.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "provino.selectorLabels" -}}
app.kubernetes.io/name: {{ include "provino.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "provino.labels" -}}
helm.sh/chart: {{ include "provino.chart" . }}
{{ include "provino.selectorLabels" . }}
app.kubernetes.io/version: {{ include "provino.imageTag" . | trunc 63 | trimSuffix "-" | quote }}
app.kubernetes.io/component: server
app.kubernetes.io/part-of: provino
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{- define "provino.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "provino.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Name of the headless Service, which governs the StatefulSet and gives every replica a stable per-pod DNS name
*/}}
{{- define "provino.headlessServiceName" -}}
{{- printf "%s-headless" (include "provino.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Name of the Secret holding the config file, which is the user-provided one when existingConfigSecret is set
*/}}
{{- define "provino.configSecretName" -}}
{{- if .Values.existingConfigSecret }}
{{- .Values.existingConfigSecret }}
{{- else }}
{{- printf "%s-config" (include "provino.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{- define "provino.configSecretKey" -}}
{{- if .Values.existingConfigSecret }}
{{- default "config.yaml" .Values.existingConfigSecretKey }}
{{- else }}
{{- print "config.yaml" }}
{{- end }}
{{- end }}

{{/*
Image tag, which is the appVersion with the "v" prefix the release workflow tags images with
*/}}
{{- define "provino.imageTag" -}}
{{- if .Values.image.tag }}
{{- .Values.image.tag }}
{{- else }}
{{- printf "v%s" (trimPrefix "v" .Chart.AppVersion) }}
{{- end }}
{{- end }}

{{/*
Container image reference, preferring the digest over the tag when both are set
*/}}
{{- define "provino.image" -}}
{{- if .Values.image.digest }}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository (include "provino.imageTag" .) }}
{{- end }}
{{- end }}

{{/*
Values of the user's config the chart reads, each with the default provino applies when it is unset
*/}}
{{- define "provino.config.actorsRuntime" -}}
{{- lower (toString (dig "actors" "runtime" "embedded" (default (dict) .Values.config))) }}
{{- end }}

{{- define "provino.config.actorsDatabase" -}}
{{- toString (dig "actors" "database" "sqlite://./data/actors.db" (default (dict) .Values.config)) }}
{{- end }}

{{/*
Reports whether the embedded actor runtime persists to Postgres, which is selected by the scheme of its connection string
*/}}
{{- define "provino.config.usesPostgres" -}}
{{- $database := lower (trim (include "provino.config.actorsDatabase" .)) }}
{{- if or (hasPrefix "postgres://" $database) (hasPrefix "postgresql://" $database) }}
{{- print "true" }}
{{- end }}
{{- end }}

{{/*
Reports whether a storage DSN is a folder on the local file system, which is the case for every DSN that names neither S3 nor Vercel Blob
Expects the DSN as its argument
*/}}
{{- define "provino.isLocalStorage" -}}
{{- $dsn := lower (trim (toString .)) }}
{{- if not (or (hasPrefix "s3:" $dsn) (hasPrefix "vercelblob:" $dsn)) }}
{{- print "true" }}
{{- end }}
{{- end }}

{{/*
Reports whether the configuration keeps anything in the data volume, which is only known when the chart renders it
*/}}
{{- define "provino.config.usesDataVolume" -}}
{{- $config := default (dict) .Values.config }}
{{- $sqlite := and (eq (include "provino.config.actorsRuntime" .) "embedded") (not (include "provino.config.usesPostgres" .)) }}
{{- $originals := include "provino.isLocalStorage" (dig "storage" "originals" "./data/originals" $config) }}
{{- $variants := include "provino.isLocalStorage" (dig "storage" "variants" "./data/variants" $config) }}
{{- if or $sqlite $originals $variants }}
{{- print "true" }}
{{- end }}
{{- end }}

{{/*
Reports whether the API server serves TLS, which is when the configuration names a certificate
The probes need to know, since they connect to it directly
*/}}
{{- define "provino.config.usesTLS" -}}
{{- if .Values.existingConfigSecret }}
{{- else if or (dig "server" "tls" "certPEM" "" (default (dict) .Values.config)) (dig "server" "tls" "path" "" (default (dict) .Values.config)) }}
{{- print "true" }}
{{- end }}
{{- end }}

{{/*
The configuration file, which is the user's config with the options the chart owns set on top
The actor address isn't one of them: it's different for every pod, so the PROVINO_ACTORS_ADDRESS env var sets it
*/}}
{{- define "provino.configFile" -}}
{{- $config := deepCopy (default (dict) .Values.config) }}
{{- $server := default (dict) (get $config "server") }}
{{- $_ := set $server "bind" "0.0.0.0" }}
{{- $_ := set $server "port" (int .Values.ports.http) }}
{{- $_ := set $config "server" $server -}}
# Generated by the provino Helm chart, do not edit
# actors.address is set through the PROVINO_ACTORS_ADDRESS env var, since every pod advertises its own IP
{{ toYaml $config }}
{{- end }}

{{/*
A container probe that requests a health endpoint on the API port
Expects a dict with "context", "probe", "path", and, optionally, "successThreshold" set to true for probes that accept the field
*/}}
{{- define "provino.probe" -}}
httpGet:
  path: {{ .path }}
  port: http
  scheme: {{ if include "provino.config.usesTLS" .context }}HTTPS{{ else }}HTTP{{ end }}
initialDelaySeconds: {{ int .probe.initialDelaySeconds }}
periodSeconds: {{ int .probe.periodSeconds }}
timeoutSeconds: {{ int .probe.timeoutSeconds }}
failureThreshold: {{ int .probe.failureThreshold }}
{{- if .successThreshold }}
successThreshold: {{ int .probe.successThreshold }}
{{- end }}
{{- end }}

{{/*
The standard OTEL_* variables provino reads, which is the only way its telemetry export is configured
*/}}
{{- define "provino.openTelemetryEnv" -}}
- name: OTEL_EXPORTER_OTLP_ENDPOINT
  value: {{ .Values.openTelemetry.endpoint | quote }}
- name: OTEL_EXPORTER_OTLP_PROTOCOL
  value: {{ .Values.openTelemetry.protocol | quote }}
- name: OTEL_TRACES_EXPORTER
  value: {{ .Values.openTelemetry.traces | quote }}
- name: OTEL_METRICS_EXPORTER
  value: {{ .Values.openTelemetry.metrics | quote }}
- name: OTEL_LOGS_EXPORTER
  value: {{ .Values.openTelemetry.logs | quote }}
{{- with .Values.openTelemetry.sampler }}
- name: OTEL_TRACES_SAMPLER
  value: {{ . | quote }}
{{- end }}
{{- with .Values.openTelemetry.samplerArg }}
- name: OTEL_TRACES_SAMPLER_ARG
  value: {{ . | quote }}
{{- end }}
{{- end }}

{{/*
Fails the render with an actionable message when the values cannot produce a working install
Every template includes this so the error surfaces no matter which resource renders first
*/}}
{{- define "provino.validateValues" -}}
{{- $replicas := int .Values.replicaCount -}}
{{- if lt $replicas 1 -}}
{{- fail (printf "\nprovino: replicaCount must be at least 1, got %d" $replicas) -}}
{{- end -}}

{{- $persistence := .Values.persistence -}}
{{- if and (gt $replicas 1) $persistence.enabled (not $persistence.existingClaim) -}}
{{- fail (printf "\nprovino: replicaCount is %d, but persistence would give every replica a volume of its own, and they can't share what is stored in it.\nWith object storage and Postgres configured nothing is written to the data volume, so set persistence.enabled=false. To keep the originals and the variants in a volume the replicas share instead, set persistence.existingClaim to a ReadWriteMany claim." $replicas) -}}
{{- end -}}

{{- range list "http" "peers" -}}
{{- $port := int (get $.Values.ports .) -}}
{{- if or (lt $port 1) (gt $port 65535) -}}
{{- fail (printf "\nprovino: ports.%s must be between 1 and 65535, got %d" . $port) -}}
{{- end -}}
{{- end -}}

{{/* Everything below checks the config the chart renders, which an existing Secret replaces wholesale */}}
{{- if not .Values.existingConfigSecret -}}
{{- $config := default (dict) .Values.config -}}

{{- range list "server.bind" "server.port" "actors.address" "actors.bindAddress" "actors.bindPort" -}}
{{- $path := splitList "." . -}}
{{- if hasKey (default (dict) (get $config (first $path))) (last $path) -}}
{{- fail (printf "\nprovino: config.%s is set by the chart, so it can't be set in config: use ports.http and ports.peers to change the ports" .) -}}
{{- end -}}
{{- end -}}

{{- $hasJWT := dig "auth" "jwt" "issuer" "" $config -}}
{{- $hasAPIKeys := dig "auth" "apiKeys" list $config -}}
{{- $hasNone := dig "auth" "none" "enabled" false $config -}}
{{- if not (or $hasJWT $hasAPIKeys $hasNone) -}}
{{- fail "\nprovino: config.auth needs at least one way for callers to authenticate: config.auth.jwt.issuer, config.auth.apiKeys, or, for development only, config.auth.none.enabled" -}}
{{- end -}}

{{- $runtime := include "provino.config.actorsRuntime" . -}}
{{- if not (has $runtime (list "embedded" "external")) -}}
{{- fail (printf "\nprovino: config.actors.runtime must be either \"embedded\" or \"external\", got %q" $runtime) -}}
{{- end -}}

{{- if and (gt $replicas 1) (eq $runtime "embedded") -}}
{{- if not (include "provino.config.usesPostgres" .) -}}
{{- fail (printf "\nprovino: replicaCount is %d, but the embedded actor runtime persists to SQLite, which a single replica can use only.\nSet config.actors.database to a Postgres connection string, \"postgres://<user>:<password>@<host>:<port>/<database>\", that every replica shares." $replicas) -}}
{{- end -}}
{{- if not (dig "actors" "runtimePSK" "" $config) -}}
{{- fail (printf "\nprovino: replicaCount is %d, so config.actors.runtimePSK is required: it is what the replicas derive the CA they authenticate each other with from.\nGenerate one with: openssl rand -base64 32" $replicas) -}}
{{- end -}}
{{- end -}}

{{- if and (gt $replicas 1) (not $persistence.existingClaim) -}}
{{- range list "originals" "variants" -}}
{{- if include "provino.isLocalStorage" (dig "storage" . (printf "./data/%s" .) $config) -}}
{{- fail (printf "\nprovino: replicaCount is %d, but config.storage.%s is a folder in the data volume, which the replicas don't share.\nPoint it at an S3 or Vercel Blob store, or set persistence.existingClaim to a ReadWriteMany claim they all mount." $replicas .) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- end -}}

{{- if and .Values.openTelemetry.enabled (not .Values.openTelemetry.endpoint) -}}
{{- fail "\nprovino: openTelemetry.endpoint is required when openTelemetry.enabled is true" -}}
{{- end -}}

{{- if and .Values.httpRoute.enabled (not .Values.httpRoute.parentRefs) -}}
{{- fail "\nprovino: httpRoute.parentRefs is required when httpRoute.enabled is true, and names the Gateways the route attaches to" -}}
{{- end -}}

{{- range .Values.projectedServiceAccountTokens -}}
{{- if not (and .name .audience .mountPath) -}}
{{- fail "\nprovino: every entry of projectedServiceAccountTokens needs a name, an audience, and a mountPath" -}}
{{- end -}}
{{- end -}}
{{- end -}}
