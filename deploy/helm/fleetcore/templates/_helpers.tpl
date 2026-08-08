{{- define "fleetcore.name" -}}{{ default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}{{- end -}}

{{- define "fleetcore.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}{{ .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else -}}{{ printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" }}{{- end -}}
{{- end -}}

{{- define "fleetcore.labels" -}}
app.kubernetes.io/name: {{ include "fleetcore.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "fleetcore.serverImage" -}}{{ .Values.image.repository }}:{{ .Values.image.tag }}{{- end -}}

{{/* Default agent target: the chart's own agent service, which is also what
     must appear in controlPlane.sans for enrolment to verify. */}}
{{- define "fleetcore.agentServer" -}}
{{- if .Values.agent.server -}}{{ .Values.agent.server }}
{{- else -}}https://{{ include "fleetcore.fullname" . }}:{{ .Values.controlPlane.service.agent.port }}{{- end -}}
{{- end -}}

{{/* SANs the server certificate must carry. The in-cluster service names are
     derived from the release name rather than hardcoded, because a release
     named anything but "fleetcore" would otherwise produce a certificate that
     does not cover the service agents are pointed at. */}}
{{- define "fleetcore.sans" -}}
{{- $full := include "fleetcore.fullname" . -}}
{{- $names := list $full (printf "%s.%s" $full .Release.Namespace) (printf "%s.%s.svc" $full .Release.Namespace) (printf "%s.%s.svc.cluster.local" $full .Release.Namespace) "localhost" "127.0.0.1" -}}
{{- if .Values.controlPlane.extraSans -}}
{{- $names = concat $names (splitList "," .Values.controlPlane.extraSans) -}}
{{- end -}}
{{- join "," $names -}}
{{- end -}}

{{/* Refuse to render a multi-replica control plane. The in-process bus has no
     cross-instance fan-out, the live registry is per-process, and each replica
     would generate its own CA — agents pin the CA at enrolment, so a second
     replica permanently orphans every agent that talks to it. */}}
{{- define "fleetcore.validate" -}}
{{- if gt (int .Values.controlPlane.replicas) 1 -}}
{{- fail "controlPlane.replicas must be 1: the control plane is not HA (in-process bus, in-memory live registry, CA on local disk). See values.yaml." -}}
{{- end -}}
{{- if and .Values.agent.enabled (not .Values.agent.hostPID) -}}
{{- fail "agent.hostPID must be true, or the agent reports only its own PID namespace (one process) instead of the node's." -}}
{{- end -}}
{{- end -}}
