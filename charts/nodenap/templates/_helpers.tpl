{{- define "pa.name" -}}nodenap{{- end -}}
{{- define "pa.labels" -}}
app.kubernetes.io/name: {{ include "pa.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
