{{/*
Expand the name of the chart.
*/}}
{{- define "arc-mcp.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name.
*/}}
{{- define "arc-mcp.fullname" -}}
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

{{/*
Chart name and version as used by the chart label.
*/}}
{{- define "arc-mcp.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "arc-mcp.labels" -}}
helm.sh/chart: {{ include "arc-mcp.chart" . }}
{{ include "arc-mcp.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "arc-mcp.selectorLabels" -}}
app.kubernetes.io/name: {{ include "arc-mcp.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Service account name.
*/}}
{{- define "arc-mcp.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "arc-mcp.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Name of the Secret that holds the Arc token.
If the user supplied tokenSecretRef.name, use that directly.
Otherwise, we create a chart-managed Secret with this name.
*/}}
{{- define "arc-mcp.tokenSecretName" -}}
{{- if .Values.arc.tokenSecretRef.name }}
{{- .Values.arc.tokenSecretRef.name }}
{{- else }}
{{- include "arc-mcp.fullname" . }}
{{- end }}
{{- end }}

{{/*
Key within the Secret that holds the Arc token.
*/}}
{{- define "arc-mcp.tokenSecretKey" -}}
{{- default "token" .Values.arc.tokenSecretRef.key }}
{{- end }}
