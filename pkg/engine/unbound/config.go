package unbound

import (
	"bytes"
	"text/template"

	"github.com/astradns/astradns-types/engine"
)

// RenderConfig renders an EngineConfig to an unbound.conf string.
func RenderConfig(config engine.EngineConfig) (string, error) {
	normalized := normalizeConfig(config)
	if err := engine.ValidateTemplateConfig(normalized); err != nil {
		return "", err
	}

	data := engine.NewTemplateData(normalized)

	tmpl, err := template.New("unbound").Parse(engine.UnboundConfigTemplate)
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}

	return buf.String(), nil
}

func normalizeConfig(config engine.EngineConfig) engine.EngineConfig {
	normalized := config
	normalized.Upstreams = make([]engine.UpstreamConfig, len(config.Upstreams))
	copy(normalized.Upstreams, config.Upstreams)

	for i := range normalized.Upstreams {
		if normalized.Upstreams[i].Port == 0 {
			normalized.Upstreams[i].Port = 53
		}
	}

	return normalized
}
