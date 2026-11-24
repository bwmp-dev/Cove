package worker

import (
	"errors"
	"net/netip"
	"slices"
)

type IngressBinding struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
}

type IngressPublicBindings struct {
	HTTP  []IngressBinding `json:"http"`
	HTTPS []IngressBinding `json:"https"`
}

type IngressListener struct {
	ID         string           `json:"id"`
	Protocol   string           `json:"protocol"`
	Visibility string           `json:"visibility"`
	Bindings   []IngressBinding `json:"bindings"`
}

type IngressExposureSettings struct {
	Public    *IngressPublicBindings `json:"public,omitempty"`
	HTTP3     bool                   `json:"http3"`
	Listeners []IngressListener      `json:"listeners"`
}

func validateIngressExposures(settings IngressExposureSettings) error {
	if settings.Public == nil && (settings.HTTP3 || len(settings.Listeners) > 0) {
		return errors.New("resolve the proxy's current public bindings before validating listener changes")
	}
	if settings.HTTP3 && len(settings.Public.HTTPS) == 0 {
		return errors.New("HTTP/3 requires an HTTPS publication")
	}
	if settings.HTTP3 {
		for _, binding := range settings.Public.HTTPS {
			if binding.Port != settings.Public.HTTPS[0].Port {
				return errors.New("HTTP/3 requires one advertised HTTPS port across all public addresses")
			}
		}
	}
	type reservation struct {
		address netip.Addr
		port    int
		udp     bool
	}
	reserved := []reservation{}
	reserve := func(bindings []IngressBinding, visibility string, udp bool) error {
		if len(bindings) == 0 || len(bindings) > 8 {
			return errors.New("each listener needs between one and eight explicit address bindings")
		}
		for _, binding := range bindings {
			address, err := netip.ParseAddr(binding.Address)
			if err != nil || address.Zone() != "" || address.Is4In6() || address.IsMulticast() || binding.Port < 1 || binding.Port > 65535 {
				return errors.New("listener bindings require IPv4 or IPv6 literals and ports from 1 to 65535")
			}
			tailscale := netip.MustParsePrefix("100.64.0.0/10").Contains(address) || netip.MustParsePrefix("fd7a:115c:a1e0::/48").Contains(address)
			if visibility == "lan" && (!address.IsPrivate() || tailscale) || visibility == "tailscale" && !tailscale {
				return errors.New("private listeners require an explicit address from their selected LAN or Tailscale range")
			}
			for _, existing := range reserved {
				if existing.port == binding.Port && existing.udp == udp && existing.address.Is4() == address.Is4() && (existing.address == address || existing.address.IsUnspecified() || address.IsUnspecified()) {
					return errors.New("listener bindings overlap on the same address, protocol and port")
				}
			}
			reserved = append(reserved, reservation{address: address, port: binding.Port, udp: udp})
		}
		return nil
	}
	if settings.Public != nil {
		if len(settings.Public.HTTP) > 0 {
			if err := reserve(settings.Public.HTTP, "public", false); err != nil {
				return err
			}
		}
		if len(settings.Public.HTTPS) > 0 {
			if err := reserve(settings.Public.HTTPS, "public", false); err != nil {
				return err
			}
			if settings.HTTP3 {
				if err := reserve(settings.Public.HTTPS, "public", true); err != nil {
					return err
				}
			}
		}
	}
	if len(settings.Listeners) > 32 {
		return errors.New("at most 32 managed listeners are supported")
	}
	seen := map[string]bool{}
	for _, listener := range settings.Listeners {
		if !workloadIdentifier.MatchString(listener.ID) || len(listener.ID) > 32 || seen[listener.ID] {
			return errors.New("listener identifiers must be unique lowercase names of at most 32 characters")
		}
		seen[listener.ID] = true
		if !slices.Contains([]string{"https", "tcp", "udp"}, listener.Protocol) || !slices.Contains([]string{"public", "lan", "tailscale"}, listener.Visibility) || listener.Protocol == "https" && listener.Visibility == "public" {
			return errors.New("listeners require a supported transport and visibility; public HTTPS uses the public bindings")
		}
		if err := reserve(listener.Bindings, listener.Visibility, listener.Protocol == "udp"); err != nil {
			return err
		}
	}
	return nil
}
