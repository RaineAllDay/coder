{{- /* Heavily inspired by the Go toolchain formatting. */ -}}
Usage: {{.FullUsage}}

{{ with .Short }}
{{- wrapTTY . }}
{{"\n"}}
{{- end}}
{{- with .Long}}
{{- formatLong . }}
{{ "\n" }}
{{- end }}
{{- range $index, $group := optionGroups . }}
{{ with $group.Name }} {{- print $group.Name " Options" | prettyHeader }} {{ else -}} {{ prettyHeader "Options"}}{{- end -}}
{{- with $group.Description }}
{{ formatGroupDescription . }}
{{- else }}
{{- end }}
    {{- range $index, $option := $group.Options }}
    {{- with flagName $option }}
    --{{- . -}} {{ end }} {{- with $option.FlagShorthand }}, -{{- . -}} {{ end }}
    {{- with envName $option }}, ${{ . }} {{ end }}
    {{- with $option.Default }} (default: {{ . }}) {{ end }} {{- with typeHelper $option }} {{ . }} {{ end }}
        {{- with $option.Description }}
            {{- $desc := $option.Description }}
{{ indent $desc 2 }}
{{- if isDeprecated $option }} DEPRECATED {{ end }}
        {{- end -}}
    {{- end }}
{{- end }}
{{- range $index, $child := visibleChildren . }}
{{- if eq $index 0 }}
{{ prettyHeader "Subcommands"}}
{{- end }}
    {{ indent $child.Name 1 | trimNewline }}{{"\t"}}{{ indent $child.Short 1 | trimNewline }}
{{- end }}
---
Report bugs and request features at https://github.com/coder/coder/issues/new
