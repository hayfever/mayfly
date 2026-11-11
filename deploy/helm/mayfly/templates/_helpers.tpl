{{- define "mayfly.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "mayfly.fullname" -}}
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

{{- define "mayfly.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "mayfly.labels" -}}
helm.sh/chart: {{ include "mayfly.chart" . }}
app.kubernetes.io/name: {{ include "mayfly.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "mayfly.selectorLabels" -}}
app.kubernetes.io/name: {{ include "mayfly.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "mayfly.controlPlaneImage" -}}
{{- if .Values.image.digest -}}
{{ .Values.image.repository }}@{{ .Values.image.digest }}
{{- else -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}
{{- end -}}

{{- define "mayfly.agentImage" -}}
{{- if .Values.agentImage.digest -}}
{{ .Values.agentImage.repository }}@{{ .Values.agentImage.digest }}
{{- else -}}
{{ .Values.agentImage.repository }}:{{ .Values.agentImage.tag | default .Chart.AppVersion }}
{{- end -}}
{{- end -}}

{{- define "mayfly.controlPlaneServiceAccountName" -}}
{{- default (include "mayfly.fullname" .) .Values.serviceAccount.controlPlaneName -}}
{{- end -}}

{{- define "mayfly.agentServiceAccountName" -}}
{{- default (printf "%s-agent" (include "mayfly.fullname" .)) .Values.serviceAccount.agentName -}}
{{- end -}}

{{- define "mayfly.secretName" -}}
{{- default (printf "%s-secrets" (include "mayfly.fullname" .)) .Values.secret.name -}}
{{- end -}}

{{- define "mayfly.configMapName" -}}
{{- printf "%s-config" (include "mayfly.fullname" .) -}}
{{- end -}}

{{- define "mayfly.skillsConfigMapName" -}}
{{- printf "%s-skills" (include "mayfly.fullname" .) -}}
{{- end -}}

{{- define "mayfly.agentsConfigMapName" -}}
{{- printf "%s-agents" (include "mayfly.fullname" .) -}}
{{- end -}}

{{- define "mayfly.litellmName" -}}
{{- printf "%s-litellm" (include "mayfly.fullname" .) -}}
{{- end -}}
