{{/* Base chart name */}}
{{- define "openstack-mangament-api.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{/* Fully qualified release name */}}
{{- define "openstack-mangament-api.fullname" -}}
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
{{- end }}

{{/* Chart label */}}
{{- define "openstack-mangament-api.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{/* Common labels, optionally scoped to a component */}}
{{- define "openstack-mangament-api.labels" -}}
app.kubernetes.io/name: {{ include "openstack-mangament-api.name" .context }}
helm.sh/chart: {{ include "openstack-mangament-api.chart" .context }}
app.kubernetes.io/instance: {{ .context.Release.Name }}
app.kubernetes.io/managed-by: {{ .context.Release.Service }}
{{- if .context.Chart.AppVersion }}
app.kubernetes.io/version: {{ .context.Chart.AppVersion | quote }}
{{- end }}
{{- if .component }}
app.kubernetes.io/component: {{ .component }}
{{- end }}
{{- end }}

{{/* Selector labels, optionally scoped to a component */}}
{{- define "openstack-mangament-api.selectorLabels" -}}
app.kubernetes.io/name: {{ include "openstack-mangament-api.name" .context }}
app.kubernetes.io/instance: {{ .context.Release.Name }}
{{- if .component }}
app.kubernetes.io/component: {{ .component }}
{{- end }}
{{- end }}

{{/* Component-scoped name */}}
{{- define "openstack-mangament-api.componentName" -}}
{{- $ctx := .context -}}
{{- $component := .component | default "" -}}
{{- $base := include "openstack-mangament-api.fullname" $ctx -}}
{{- if $component -}}
{{- printf "%s-%s" $base $component | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $base -}}
{{- end -}}
{{- end }}

{{/* Component-scoped name with optional suffix for individual resources */}}
{{- define "openstack-mangament-api.resourceName" -}}
{{- $base := include "openstack-mangament-api.componentName" . -}}
{{- $suffix := .name | default "" -}}
{{- if $suffix -}}
{{- printf "%s-%s" $base $suffix | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $base -}}
{{- end -}}
{{- end }}

{{/* Standard rollout annotations to trigger pod restarts on config/template changes */}}
{{- define "openstack-mangament-api.rolloutAnnotations" -}}
# 1. Responds to any change in the Values (becomes a JSON string -> Hash)
checksum/values: {{ .Values | toJson | sha256sum }}
# 2. Responds to the path of the Templates (as a string)
checksum/all-templates: {{ .Template.BasePath | sha256sum }}
{{- end }}
