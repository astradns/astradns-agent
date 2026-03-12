package coredns

import (
	"bytes"
	"fmt"
	"net/netip"
	"runtime"
	"strings"
	"text/template"

	"github.com/astradns/astradns-types/engine"
)

// RenderConfig renders an EngineConfig to a CoreDNS Corefile string.
func RenderConfig(config engine.EngineConfig) (string, error) {
	normalized := normalizeConfig(config)
	if err := validateCoreDNSCompatibility(normalized); err != nil {
		return "", err
	}
	if err := engine.ValidateTemplateConfig(normalized); err != nil {
		return "", err
	}

	data := engine.NewTemplateData(normalized)

	tmpl, err := template.New("coredns").Parse(engine.CorefileTemplate)
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

func validateCoreDNSCompatibility(config engine.EngineConfig) error {
	if config.DNSSEC.Mode != engine.DNSSECModeOff {
		return fmt.Errorf("dnssec mode %q is not supported by coredns engine", config.DNSSEC.Mode)
	}

	seenTLSServerNames := make(map[string]struct{})
	for i, upstream := range config.Upstreams {
		switch upstream.Transport {
		case engine.UpstreamTransportDNS, engine.UpstreamTransportDoT, engine.UpstreamTransportDoH:
		default:
			return fmt.Errorf("upstreams[%d].transport %q is not supported by coredns engine", i, upstream.Transport)
		}

		if upstream.Transport == engine.UpstreamTransportDNS {
			continue
		}
		if upstream.TLSServerName == "" {
			continue
		}
		seenTLSServerNames[upstream.TLSServerName] = struct{}{}
	}

	if len(seenTLSServerNames) > 1 {
		return fmt.Errorf("coredns forward supports only one tlsServerName across encrypted upstreams")
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
