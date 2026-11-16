package httputil

import (
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

func isValidRegName(host string) bool {
	if host == "" || strings.HasPrefix(host, ".") || strings.Contains(host, "..") {
		return false
	}
	for i := 0; i < len(host); i++ {
		c := host[i]
		isAlphaNum := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if !isAlphaNum && c != '-' && c != '.' && c != '_' {
			return false
		}
	}
	return true
}

func parseCanonicalPort(rawPort string) (string, bool) {
	if rawPort == "" {
		return "", false
	}
	for i := 0; i < len(rawPort); i++ {
		if rawPort[i] < '0' || rawPort[i] > '9' {
			return "", false
		}
	}
	n, err := strconv.Atoi(rawPort)
	if err != nil || n < 1 || n > 65535 {
		return "", false
	}
	return strconv.Itoa(n), true
}

// ExtractCanonicalHostname extracts the normalized hostname (without brackets or port) from a valid host[:port] authority.
// Returns ("", false) if hostPort is malformed.
func ExtractCanonicalHostname(hostPort string) (string, bool) {
	norm := NormalizeOriginHost(hostPort, "http")
	if norm == "" {
		return "", false
	}
	if strings.HasPrefix(norm, "[") {
		if strings.HasSuffix(norm, "]") {
			return norm[1 : len(norm)-1], true
		}
		h, _, err := net.SplitHostPort(norm)
		if err != nil {
			return "", false
		}
		return h, true
	}
	if h, _, err := net.SplitHostPort(norm); err == nil {
		return h, true
	}
	return norm, true
}

// NormalizeOriginHost normalizes a host[:port] authority for Web Origin comparison
// under the given scheme ("http" or "https"), stripping default ports (:80 for http, :443 for https),
// canonicalizing IPv4/IPv6 literals and numeric ports, and rejecting malformed authorities.
func NormalizeOriginHost(hostPort string, scheme string) string {
	hp := strings.ToLower(strings.TrimSpace(hostPort))
	if hp == "" || strings.ContainsAny(hp, " \t\r\n/\\?#@%") {
		return ""
	}

	var h, rawPort string
	isBracketedV6 := strings.HasPrefix(hp, "[")
	if isBracketedV6 {
		if strings.HasSuffix(hp, "]") {
			h = hp[1 : len(hp)-1]
		} else {
			var err error
			h, rawPort, err = net.SplitHostPort(hp)
			if err != nil || rawPort == "" {
				return ""
			}
		}
		addr, err := netip.ParseAddr(h)
		if err != nil || !addr.Is6() {
			return ""
		}
		h = addr.String()
	} else {
		colonCount := strings.Count(hp, ":")
		if colonCount > 1 {
			// Unbracketed IPv6 literal in URL authority is invalid per RFC 3986.
			return ""
		}
		if colonCount == 1 {
			var err error
			h, rawPort, err = net.SplitHostPort(hp)
			if err != nil || h == "" || rawPort == "" {
				return ""
			}
		} else {
			h = hp
		}
		if strings.ContainsAny(h, "[]:") {
			return ""
		}
		if addr, err := netip.ParseAddr(h); err == nil {
			if !addr.Is4() {
				return ""
			}
			h = addr.String()
		} else if !isValidRegName(h) {
			return ""
		}
	}

	port := ""
	if rawPort != "" {
		var ok bool
		port, ok = parseCanonicalPort(rawPort)
		if !ok {
			return ""
		}
	}

	isDefaultPort := port == "" ||
		(strings.EqualFold(scheme, "https") && port == "443") ||
		(strings.EqualFold(scheme, "http") && port == "80")
	if isDefaultPort {
		if isBracketedV6 {
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

// IsLoopbackHost reports whether hostPort targets localhost or a loopback IP (127.0.0.0/8 or ::1).
func IsLoopbackHost(hostPort string) bool {
	host, ok := ExtractCanonicalHostname(hostPort)
	if !ok {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return addr.Unmap().IsLoopback()
}

// ParseBaseURL parses and canonicalizes a remote HTTP(S) base URL:
//   - Requires an explicit http:// or https:// scheme
//   - Rejects userinfo credentials and malformed host[:port] authorities
//   - Strips query (? and ForceQuery), fragment (#), RawPath, and trailing slashes
func ParseBaseURL(rawInput string) (*url.URL, error) {
	raw := strings.TrimSpace(rawInput)
	if raw == "" {
		return nil, fmt.Errorf("remote URL is empty")
	}
	idx := strings.Index(raw, "://")
	if idx < 0 {
		return nil, fmt.Errorf("remote URL %q must include an explicit http:// or https:// scheme", rawInput)
	}
	scheme := strings.ToLower(strings.TrimSpace(raw[:idx]))
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme %q in remote URL %q (expected http or https)", scheme, rawInput)
	}
	raw = scheme + raw[idx:]
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid remote URL %q", rawInput)
	}
	normHost := NormalizeOriginHost(u.Host, scheme)
	if normHost == "" {
		return nil, fmt.Errorf("invalid remote URL %q", rawInput)
	}
	u.Scheme = scheme
	u.Host = normHost
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

// LastForwardedHop returns the rightmost comma-separated hop from the LAST header line in vals.
// If vals is empty or the rightmost element of the last header line is blank, it fails closed ("").
func LastForwardedHop(vals []string) string {
	if len(vals) == 0 {
		return ""
	}
	lastLine := vals[len(vals)-1]
	parts := strings.Split(lastLine, ",")
	return strings.TrimSpace(parts[len(parts)-1])
}

// ParseCIDRList parses a comma-separated list of IPv4/IPv6 CIDR prefixes or single IP literals.
func ParseCIDRList(csv string) ([] netip.Prefix, error) {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return nil, nil
	}
	var out []netip.Prefix
	for _, raw := range strings.Split(csv, ",") {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if strings.Contains(s, "/") {
			p, err := netip.ParsePrefix(s)
			if err != nil {
				return nil, fmt.Errorf("invalid trusted proxy CIDR %q: %w", s, err)
			}
			out = append(out, p.Masked())
		} else {
			a, err := netip.ParseAddr(s)
			if err != nil {
				return nil, fmt.Errorf("invalid trusted proxy IP %q: %w", s, err)
			}
			a = a.Unmap()
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
		}
	}
	return out, nil
}

// IsTrustedProxyPeerAddr reports whether remoteAddr belongs to `allowedPrefixes` (when non-empty),
// or to loopback / RFC1918 / RFC4193 private address space by default when `allowedPrefixes` is empty.
func IsTrustedProxyPeerAddr(remoteAddr string, allowedPrefixes []netip.Prefix) bool {
	s := strings.TrimSpace(remoteAddr)
	if s == "" {
		return false
	}
	if h, _, err := net.SplitHostPort(s); err == nil {
		s = h
	}
	if idx := strings.IndexByte(s, '%'); idx >= 0 {
		s = s[:idx]
	}
	addr, err := netip.ParseAddr(strings.Trim(s, "[]"))
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	if len(allowedPrefixes) > 0 {
		for _, p := range allowedPrefixes {
			if p.Contains(addr) {
				return true
			}
		}
		return false
	}
	return addr.IsLoopback() || addr.IsPrivate()
}

// ReadBoundedBody reads all bytes from r up to maxBytes, returning an explicit error if r exceeds maxBytes
// instead of silently truncating the payload.
func ReadBoundedBody(r io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("invalid maxBytes %d", maxBytes)
	}
	b, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxBytes {
		return nil, fmt.Errorf("response body exceeds maximum allowed size (%d bytes)", maxBytes)
	}
	return b, nil
}
