{{/*
Expand the name of the chart.
*/}}
{{- define "srengine.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name.
*/}}
{{- define "srengine.fullname" -}}
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
Chart label value.
*/}}
{{- define "srengine.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "srengine.labels" -}}
helm.sh/chart: {{ include "srengine.chart" . }}
{{ include "srengine.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "srengine.selectorLabels" -}}
app.kubernetes.io/name: {{ include "srengine.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ServiceAccount name.
*/}}
{{- define "srengine.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "srengine.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Ollama service name — используется в env агента как OLLAMA_URL.
*/}}
{{- define "srengine.ollamaServiceName" -}}
{{- printf "%s-ollama" (include "srengine.fullname" .) }}
{{- end }}

{{/*
Env-переменные агента из values — вынесено в helper чтобы не дублировать
между основным Deployment и возможными Job/initContainer в будущем.
*/}}
{{- define "srengine.agentEnv" -}}
- name: NAMESPACES
  value: {{ .Values.namespaces | join "," | quote }}
- name: AGENT_MAX_STEPS
  value: {{ .Values.agent.maxSteps | quote }}
- name: AGENT_STEP_TIMEOUT
  value: {{ .Values.agent.stepTimeout | quote }}
- name: AGENT_INVESTIG_TIMEOUT
  value: {{ .Values.agent.investigTimeout | quote }}
- name: AGENT_SUMMARIZE_EVERY
  value: {{ .Values.agent.summarizeEvery | quote }}
- name: LLM_PROVIDER
  value: {{ .Values.llm.provider | quote }}
- name: OLLAMA_URL
  value: {{ if .Values.ollama.enabled -}}
    http://{{ include "srengine.ollamaServiceName" . }}:11434
  {{- else -}}
    {{ .Values.llm.ollamaUrl | quote }}
  {{- end }}
- name: OLLAMA_MODEL
  value: {{ .Values.ollama.model | quote }}
- name: LLM_TIMEOUT
  value: {{ .Values.llm.timeout | quote }}
- name: LLM_MAX_RETRIES
  value: {{ .Values.llm.maxRetries | quote }}
- name: LOGS_MAX_LINES
  value: {{ .Values.logs.maxLines | quote }}
- name: LOGS_MAX_PATTERNS
  value: {{ .Values.logs.maxPatterns | quote }}
- name: LOGS_LEVELS
  value: {{ .Values.logs.levels | join "," | quote }}
- name: SERVER_ADDR
  value: {{ .Values.server.addr | quote }}
- name: PROMETHEUS_URL
  value: {{ .Values.metrics.prometheusUrl | quote }}
- name: METRICS_TIMEOUT
  value: {{ .Values.metrics.timeout | quote }}
- name: PROMPT_PATH
  value: "/etc/srengine/prompt.txt"
{{- /* ── Notifiers ── */}}
- name: TELEGRAM_ENABLED
  value: {{ .Values.notifier.telegram.enabled | quote }}
{{- if .Values.notifier.telegram.enabled }}
- name: TELEGRAM_TOKEN
  value: {{ .Values.notifier.telegram.token | quote }}
- name: TELEGRAM_CHAT_ID
  value: {{ .Values.notifier.telegram.chatId | quote }}
{{- end }}
- name: EMAIL_ENABLED
  value: {{ .Values.notifier.email.enabled | quote }}
{{- if .Values.notifier.email.enabled }}
- name: EMAIL_SMTP_HOST
  value: {{ .Values.notifier.email.smtpHost | quote }}
- name: EMAIL_SMTP_PORT
  value: {{ .Values.notifier.email.smtpPort | quote }}
- name: EMAIL_FROM
  value: {{ .Values.notifier.email.from | quote }}
- name: EMAIL_TO
  value: {{ .Values.notifier.email.to | join "," | quote }}
{{- if .Values.notifier.email.password }}
- name: EMAIL_PASSWORD
  value: {{ .Values.notifier.email.password | quote }}
{{- end }}
{{- end }}
- name: WEBHOOK_ENABLED
  value: {{ .Values.notifier.webhook.enabled | quote }}
{{- if .Values.notifier.webhook.enabled }}
- name: WEBHOOK_URL
  value: {{ .Values.notifier.webhook.url | quote }}
- name: WEBHOOK_TIMEOUT
  value: {{ .Values.notifier.webhook.timeout | quote }}
{{- end }}
{{- end }}
