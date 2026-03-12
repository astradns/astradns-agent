package unbound

import (
	"bytes"
	"fmt"
	"net/netip"
	"runtime"
	"strings"
	"text/template"

	"github.com/astradns/astradns-types/engine"
)

// RenderConfig renders an EngineConfig to an unbound.conf string.
func RenderConfig(config engine.EngineConfig) (string, error) {
	normalized := normalizeConfig(config)
	if err := validateUnboundCompatibility(normalized); err != nil {
		return "", err
	}
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
	normalized.WorkerThreads = normalizeWorkerThreads(normalized.WorkerThreads)
	normalized.DNSSEC.Mode = normalizeDNSSECMode(normalized.DNSSEC.Mode)
	normalized.Upstreams = make([]engine.UpstreamConfig, len(config.Upstreams))
	copy(normalized.Upstreams, config.Upstreams)

	for i := range normalized.Upstreams {
		normalized.Upstreams[i].Transport = normalizeUpstreamTransport(normalized.Upstreams[i].Transport)
		if normalized.Upstreams[i].Port == 0 {
			normalized.Upstreams[i].Port = defaultPortForTransport(normalized.Upstreams[i].Transport)
		}
		normalized.Upstreams[i].TLSServerName = strings.TrimSpace(normalized.Upstreams[i].TLSServerName)
		if normalized.Upstreams[i].Transport != engine.UpstreamTransportDNS && normalized.Upstreams[i].TLSServerName == "" {
			normalized.Upstreams[i].TLSServerName = defaultTLSServerName(normalized.Upstreams[i].Address)
		}
		if normalized.Upstreams[i].Transport == engine.UpstreamTransportDNS {
			normalized.Upstreams[i].TLSServerName = ""
		}
		if normalized.Upstreams[i].Weight <= 0 {
			normalized.Upstreams[i].Weight = 1
		}
		if normalized.Upstreams[i].Preference <= 0 {
			normalized.Upstreams[i].Preference = 100
		}
	}

	return normalized
}

func validateUnboundCompatibility(config engine.EngineConfig) error {
	hasPlainDNS := false
	hasDoT := false

	for i, upstream := range config.Upstreams {
		switch upstream.Transport {
		case engine.UpstreamTransportDoH:
			return fmt.Errorf("upstreams[%d].transport doh is not supported by unbound engine", i)
		case engine.UpstreamTransportDoT:
			hasDoT = true
		default:
			hasPlainDNS = true
		}
	}

	if hasPlainDNS && hasDoT {
		return fmt.Errorf("mixed dns and dot upstream transports are not supported by unbound engine")
	}

	return nil
}

func defaultPortForTransport(transport engine.UpstreamTransport) int32 {
	switch transport {
	case engine.UpstreamTransportDoT:
		return 853
	case engine.UpstreamTransportDoH:
		return 443
	default:
		return 53
	}
}

func normalizeUpstreamTransport(transport engine.UpstreamTransport) engine.UpstreamTransport {
	trimmed := strings.ToLower(strings.TrimSpace(string(transport)))
	switch engine.UpstreamTransport(trimmed) {
	case engine.UpstreamTransportDoT:
		return engine.UpstreamTransportDoT
	case engine.UpstreamTransportDoH:
		return engine.UpstreamTransportDoH
	default:
		return engine.UpstreamTransportDNS
	}
}

func normalizeDNSSECMode(mode engine.DNSSECMode) engine.DNSSECMode {
	trimmed := strings.ToLower(strings.TrimSpace(string(mode)))
	switch engine.DNSSECMode(trimmed) {
	case engine.DNSSECModeProcess:
		return engine.DNSSECModeProcess
	case engine.DNSSECModeValidate:
		return engine.DNSSECModeValidate
	default:
		return engine.DNSSECModeOff
	}
}

func normalizeWorkerThreads(value int32) int32 {
	if value > 256 {
		return 256
	}
	if value > 0 {
		return value
	}

	auto := int32(runtime.NumCPU())
	if auto <= 0 {
		return 2
	}
	if auto > 256 {
		return 256
	}

	return auto
}

func defaultTLSServerName(address string) string {
	trimmed := strings.TrimSuffix(strings.TrimSpace(address), ".")
	if trimmed == "" {
		return ""
	}
	if _, err := netip.ParseAddr(trimmed); err == nil {
		return ""
	}

	return trimmed
}
