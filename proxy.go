package ehttp

import (
	"net/netip"
	"strings"

	"go.uber.org/zap"
)

// trustedProxies is a list of address ranges. An empty list trusts no peer.
type trustedProxies []netip.Prefix

// proxyKeywords are the names of the Express "trust proxy" setting.
var proxyKeywords = map[string][]string{
	"loopback":    {"127.0.0.1/8", "::1/128"},
	"linklocal":   {"169.254.0.0/16", "fe80::/10"},
	"uniquelocal": {"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"},
}

// parseTrustedProxies accepts a CIDR, a single IP or a keyword.
// It panics for a value that is not valid.
func parseTrustedProxies(values []string) trustedProxies {
	var list trustedProxies

	for _, v := range values {
		v = strings.TrimSpace(v)

		if ranges, ok := proxyKeywords[strings.ToLower(v)]; ok {
			list = append(list, parseTrustedProxies(ranges)...)
			continue
		}

		list = append(list, parseProxyPrefix(v))
	}

	return list
}

func parseProxyPrefix(v string) netip.Prefix {
	if p, err := netip.ParsePrefix(v); err == nil {
		return unmapPrefix(p).Masked()
	}

	addr, err := netip.ParseAddr(v)
	if err != nil {
		bootPanic("WithTrustedProxies", "proxy "+quoteValue(v), "it is not a CIDR, an IP or a preset name",
			"use a CIDR such as \"10.0.0.0/8\", an IP such as \"10.0.0.1\", or loopback, linklocal, uniquelocal")
	}

	addr = addr.Unmap().WithZone("")

	return netip.PrefixFrom(addr, addr.BitLen())
}

// unmapPrefix turns ::ffff:a.b.c.d/N (N >= 96) into a.b.c.d/(N-96),
// so it matches IPv4 peers.
func unmapPrefix(p netip.Prefix) netip.Prefix {
	if !p.Addr().Is4In6() || p.Bits() < 96 {
		return p
	}

	return netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
}

// contains reports whether the host of remoteAddr ("ip:port") is in the list.
func (t trustedProxies) contains(remoteAddr string) bool {
	var addr netip.Addr

	if ap, err := netip.ParseAddrPort(remoteAddr); err == nil {
		addr = ap.Addr()
	} else if a, err := netip.ParseAddr(remoteAddr); err == nil {
		addr = a
	} else {
		return false
	}

	addr = addr.Unmap().WithZone("")

	for _, p := range t {
		if p.Contains(addr) {
			return true
		}
	}

	return false
}

const (
	peerUnknown uint32 = iota
	peerTrusted
	peerUntrusted
)

// trustedPeer reports whether the direct peer is a trusted proxy.
// The result is cached for the request.
func (req *Request) trustedPeer() bool {
	if len(req.proxies) == 0 {
		return false
	}

	if req.peer.Load() == peerUnknown {
		state := peerUntrusted

		if req.proxies.contains(req.Request.RemoteAddr) {
			state = peerTrusted
		}

		req.peer.Store(state)
	}

	return req.peer.Load() == peerTrusted
}

// forwarded returns the first value of the header name when the peer is
// trusted. For an untrusted peer it returns "" and writes a debug log.
func (req *Request) forwarded(name string) string {
	v := req.Header(name)
	if v == "" {
		return ""
	}

	if !req.trustedPeer() {
		req.logIgnoredHeader(name, "untrusted peer")

		return ""
	}

	first, _, _ := strings.Cut(v, ",")

	return strings.TrimSpace(first)
}

// forwardedProto returns "http" or "https" from a trusted proxy.
// Any other value is ignored with a warning.
func (req *Request) forwardedProto() string {
	for _, name := range [...]string{HeaderXForwardedProto, HeaderXForwardedProtocol} {
		v := req.forwarded(name)

		switch {
		case v == "":
			continue
		case strings.EqualFold(v, "https"):
			return "https"
		case strings.EqualFold(v, "http"):
			return "http"
		}

		req.warnInvalidForwarded(name, v, "make the proxy send http or https")
	}

	return ""
}

// forwardedHost returns a valid X-Forwarded-Host from a trusted proxy.
// An invalid value is ignored with a warning.
func (req *Request) forwardedHost() string {
	v := req.forwarded(HeaderXForwardedHost)
	if v == "" {
		return ""
	}

	if !validHost(v) {
		req.warnInvalidForwarded(HeaderXForwardedHost, v, "make the proxy send host or host:port")
		return ""
	}

	return v
}

// warnInvalidForwarded logs one warning for each request: a trusted proxy sent
// a value that is not valid, so the connection value is used. The value is
// client data, so the log keeps it quoted and truncated.
func (req *Request) warnInvalidForwarded(name, value, fix string) {
	if !req.forwardedWarned.CompareAndSwap(false, true) {
		return
	}

	req.newLogger().Warn("ehttp: ignored an invalid forwarded header from a trusted proxy",
		zap.String("header", fieldName(name)), zap.String("value", quoteValue(value)),
		zap.String("peer", req.Request.RemoteAddr), zap.String("fix", fix))
}

func (req *Request) logIgnoredHeader(name, reason string) {
	if !req.baseLogger().Core().Enabled(zap.DebugLevel) {
		return
	}

	req.newLogger().Debug("ehttp: ignored forwarded header",
		zap.String("header", name), zap.String("reason", reason), zap.String("remote_addr", req.Request.RemoteAddr))
}

// validHost reports whether h is "host" or "host:port", with "[ipv6]" for IPv6.
func validHost(h string) bool {
	if strings.HasPrefix(h, "[") {
		end := strings.IndexByte(h, ']')
		if end < 0 {
			return false
		}

		addr, err := netip.ParseAddr(h[1:end])

		return err == nil && addr.Is6() && validPort(h[end+1:])
	}

	name, port, hasPort := strings.Cut(h, ":")
	if hasPort && !validPort(":"+port) {
		return false
	}

	return validName(name)
}

// validPort accepts "" or ":" followed by 1 to 5 digits.
func validPort(s string) bool {
	if s == "" {
		return true
	}

	digits := s[1:]
	if s[0] != ':' || digits == "" || len(digits) > 5 {
		return false
	}

	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return false
		}
	}

	return true
}

func validName(n string) bool {
	if n == "" {
		return false
	}

	for i := 0; i < len(n); i++ {
		c := n[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_'

		if !ok {
			return false
		}
	}

	return true
}

// hostOnly removes the port from a host. It keeps IPv6 literals without brackets.
func hostOnly(h string) string {
	if strings.HasPrefix(h, "[") {
		if end := strings.IndexByte(h, ']'); end > 0 {
			return h[1:end]
		}

		return h
	}

	if i := strings.LastIndexByte(h, ':'); i >= 0 && strings.IndexByte(h, ':') == i {
		return h[:i]
	}

	return h
}
