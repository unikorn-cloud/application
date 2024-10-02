{{/*
Create the container images
*/}}
{{- define "unikorn.applicationControllerImage" -}}
{{- .Values.controller.image | default (printf "%s/unikorn-application-controller:%s" (include "unikorn.defaultRepositoryPath" .) (.Values.tag | default .Chart.Version)) }}
{{- end }}

{{- define "unikorn.applicationServerImage" -}}
{{- .Values.server.image | default (printf "%s/unikorn-application-server:%s" (include "unikorn.defaultRepositoryPath" .) (.Values.tag | default .Chart.Version)) }}
{{- end }}

{{/*
Create image pull secrets
*/}}
{{- define "unikorn.imagePullSecrets" -}}
{{- if .Values.imagePullSecret -}}
- name: {{ .Values.imagePullSecret }}
{{ end }}
{{- if .Values.dockerConfig -}}
- name: docker-config
{{- end }}
{{- end }}
