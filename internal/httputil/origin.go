package httputil

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// NormalizeOriginHost normalizes a host[:port] authority for Web Origin comparison
// under the given scheme ("http" or "https"), stripping default ports (:80 for http, :443 for https)
// and lowercasing ASCII hostnames while preserving IPv6 brackets.
func NormalizeOriginHost(hostPort string, scheme string) string {
	hp := strings.ToLower(strings.TrimSpace(hostPort))
	if hp == "" {
		return ""
	}
	h, port, err := net.SplitHostPort(hp)
	if err != nil {
		// No port present (or unbracketed IPv6 literal without port); reject malformed trailing colon.
		if strings.HasSuffix(hp, ":") && !strings.HasSuffix(hp, "::") {
			return ""
		}
		return hp
	}
	if h == "" || port == "" {
		return ""
	}
	if (strings.EqualFold(scheme, "https") && port == "443") || (strings.EqualFold(scheme, "http") && port == "80") {
		if strings.Contains(h, ":") {
			return "[" + h + "]"
		}
		return h
	}
	return net.JoinHostPort(h, port)
}

// SameOrigin reports whether two HTTP(S) URLs share the exact Web Origin
// (scheme + hostname + effective port, treating :80 as default for http and :443 for https).
func SameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	sa := strings.ToLower(strings.TrimSpace(a.Scheme))
	sb := strings.ToLower(strings.TrimSpace(b.Scheme))
	if (sa != "http" && sa != "https") || sa != sb {
		return false
	}
	ha := NormalizeOriginHost(a.Host, sa)
	hb := NormalizeOriginHost(b.Host, sb)
	return ha != "" && ha == hb
}

// IsLocalOrPrivateHost reports whether hostPort targets localhost, .local mDNS,
// loopback, RFC1918 private IPs, link-local unicast, or CGNAT/Tailscale (100.64.0.0/10).
func IsLocalOrPrivateHost(hostPort string) bool {
	host := strings.TrimSpace(hostPort)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".local") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
		return true
	}
	return false
}

// ParseBaseURL parses and canonicalizes a remote HTTP(S) base URL:
//   - Defaults scheme-less local/private hosts to http:// and public hosts to https://
//   - Rejects non-http/https schemes
//   - Strips query (? and ForceQuery), fragment (#), RawPath, and trailing slashes
func ParseBaseURL(rawInput string) (*url.URL, error) {
	raw := strings.TrimSpace(rawInput)
	if raw == "" {
		return nil, fmt.Errorf("remote URL is empty")
	}
	if idx := strings.Index(raw, "://"); idx >= 0 {
		scheme := strings.ToLower(strings.TrimSpace(raw[:idx]))
		if scheme != "http" && scheme != "https" {
			return nil, fmt.Errorf("unsupported scheme %q in remote URL %q (expected http or https)", scheme, rawInput)
		}
		raw = scheme + raw[idx:]
	} else {
		hostPart := raw
		if cut := strings.IndexAny(hostPart, "/?#"); cut >= 0 {
			hostPart = hostPart[:cut]
		}
		if IsLocalOrPrivateHost(hostPart) {
			raw = "http://" + raw
		} else {
			raw = "https://" + raw
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || NormalizeOriginHost(u.Host, u.Scheme) == "" {
		return nil, fmt.Errorf("invalid remote URL %q", rawInput)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	u.RawPath = ""
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

// JoinEndpoint constructs a clean request URL from a canonicalized base URL, subpath, and optional query params.
func JoinEndpoint(base *url.URL, subpath string, query url.Values) string {
	if base == nil {
		return ""
	}
	cp := *base
	cp.RawQuery = ""
	cp.ForceQuery = false
	cp.Fragment = ""
	cp.RawFragment = ""
	cp.RawPath = ""
	basePath := strings.TrimRight(cp.Path, "/")
	rel := strings.TrimLeft(strings.TrimSpace(subpath), "/")
	if rel != "" {
		cp.Path = basePath + "/" + rel
	} else {
		cp.Path = basePath
	}
	if len(query) > 0 {
		cp.RawQuery = query.Encode()
	}
	return cp.String()
}
