{{/* Chart helpers shared by every template. */}}

{{- define "trygalle.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "trygalle.fullname" -}}
{{- if and .Release.Name (contains .Chart.Name .Release.Name) }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{- define "trygalle.labels" -}}
app.kubernetes.io/name: {{ include "trygalle.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end }}

{{- define "trygalle.serviceAccountName" -}}
{{- default (include "trygalle.fullname" .) .Values.rbac.serviceAccount.name }}
{{- end }}
