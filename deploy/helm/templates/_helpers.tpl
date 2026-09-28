{{/*
Expand the name of the chart.
*/}}
{{- define "edgelite.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this.
*/}}
{{- define "edgelite.fullname" -}}
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

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "edgelite.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels
*/}}
{{- define "edgelite.labels" -}}
helm.sh/chart: {{ include "edgelite.chart" . }}
{{ include "edgelite.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Selector labels
*/}}
{{- define "edgelite.selectorLabels" -}}
app.kubernetes.io/name: {{ include "edgelite.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
AI Sidecar labels
*/}}
{{- define "edgelite.aiSidecarLabels" -}}
helm.sh/chart: {{ include "edgelite.chart" . }}
app.kubernetes.io/name: {{ include "edgelite.name" . }}-ai-sidecar
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
{{- end -}}

{{/*
AI Sidecar selector labels
*/}}
{{- define "edgelite.aiSidecarSelectorLabels" -}}
app.kubernetes.io/name: {{ include "edgelite.name" . }}-ai-sidecar
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Generate a random secret key if not provided.
*/}}
{{- define "edgelite.secretKey" -}}
{{- if .Values.gateway.security.secretKey -}}
{{- .Values.gateway.security.secretKey -}}
{{- else -}}
{{- randAlphaNum 48 -}}
{{- end -}}
{{- end -}}

{{/*
Generate a random CSRF secret if not provided.
*/}}
{{- define "edgelite.csrfSecret" -}}
{{- if .Values.gateway.security.csrfSecret -}}
{{- .Values.gateway.security.csrfSecret -}}
{{- else -}}
{{- randAlphaNum 32 -}}
{{- end -}}
{{- end -}}
