{{/* The name of the chart. */}}
{{- define "redis.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* <release>-<chart>, or the release alone if it contains the chart name. */}}
{{- define "redis.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* The labels of every object. */}}
{{- define "redis.labels" -}}
app.kubernetes.io/name: {{ include "redis.name" . }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
app.kubernetes.io/instance: {{ .Release.Name | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/* The labels that select the pods. */}}
{{- define "redis.selectorLabels" -}}
app.kubernetes.io/name: {{ include "redis.name" . }}
app.kubernetes.io/instance: {{ .Release.Name | quote }}
{{- end -}}

{{/* registry/repository:tag or @digest */}}
{{- define "redis.image" -}}
{{- $repo := .Values.image.repository -}}
{{- if .Values.image.registry -}}{{- $repo = printf "%s/%s" .Values.image.registry $repo -}}{{- end -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" $repo .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" $repo (.Values.image.tag | default .Chart.AppVersion | toString) -}}
{{- end -}}
{{- end -}}

{{/* The secret with the password. */}}
{{- define "redis.secretName" -}}
{{- .Values.auth.existingSecret | default (include "redis.fullname" .) -}}
{{- end -}}

{{- define "redis.secretPasswordKey" -}}
{{- if .Values.auth.existingSecret -}}{{- .Values.auth.existingSecretPasswordKey -}}{{- else -}}redis-password{{- end -}}
{{- end -}}

{{/* The password: auth.password, else the one of the existing secret (upgrades keep it), else a random one. */}}
{{- define "redis.password" -}}
{{- if .Values.auth.password -}}
{{- .Values.auth.password -}}
{{- else -}}
{{- $secret := lookup "v1" "Secret" .Release.Namespace (include "redis.fullname" .) -}}
{{- if and $secret (index ($secret.data | default dict) "redis-password") -}}
{{- index $secret.data "redis-password" | b64dec -}}
{{- else -}}
{{- randAlphaNum 10 -}}
{{- end -}}
{{- end -}}
{{- end -}}
