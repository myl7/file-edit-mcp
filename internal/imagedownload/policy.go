// Package imagedownload implements the restricted HTTPS image download the
// upload tool performs (ARCHITECTURE §5.9): the URL policy, the SSRF
// address policy, a dedicated transport that binds every dial to a
// validated resolved IP, manual redirect handling, size/encoding limits,
// and image content validation.
//
// Layering: this package classifies failures but never composes
// model-visible message text — errors carry a sentinel Kind plus a
// fixed-vocabulary Detail (stage or reason constant, host name, or status
// number, never a URL, query, header value, or remote body). The tools
// layer maps the kinds onto the errmsg catalog rows.
package imagedownload

import (
	"context"
	"net"
	"net/netip"
	"net/url"
	"strings"
)

// Resolver is the DNS surface the policy dials through. *net.Resolver
// satisfies it; tests inject deterministic answers.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Policy is the URL and address verdict machinery (§5.9). The zero value
// uses the default resolver; it is immutable after first use and safe for
// concurrent callers.
type Policy struct {
	// Resolver resolves host names to IPs. Nil means net.DefaultResolver.
	Resolver Resolver
}

// resolver returns the configured or default resolver.
func (p *Policy) resolver() Resolver {
	if p.Resolver != nil {
		return p.Resolver
	}
	return net.DefaultResolver
}

// URL-shape reasons (fixed vocabulary, model-visible via errmsg.EInvalidURL).
const (
	reasonScheme       = "the scheme must be https"
	reasonUserinfo     = "credentials in the URL are not allowed"
	reasonFragment     = "fragments are not allowed"
	reasonEmptyHost    = "the host is empty"
	reasonPort         = "only port 443 (or the default port) is allowed"
	reasonZone         = "IPv6 zone identifiers are not allowed"
	reasonSingleLabel  = "single-label hostnames are not allowed"
	reasonInternalName = "internal hostname suffixes are not allowed"
	reasonHostChars    = "the host contains characters that are not allowed"
)

// ValidateURL enforces the §5.9 URL shape policy on an already-parsed URL:
// absolute https only, no userinfo, no fragment, non-empty host, default
// or 443 port only, no IPv6 zone, and (for names, not IP literals) no
// single-label or internal-suffixed hosts. The resolved-IP verdict in
// ResolveHost is the real authority; the name checks here are the cheap
// supplementary layer that keeps internal spellings away from the resolver
// at all.
func (p *Policy) ValidateURL(u *url.URL) error {
	if u.Scheme != "https" {
		return &Error{Kind: ErrBadURL, Detail: reasonScheme}
	}
	if u.User != nil {
		return &Error{Kind: ErrBadURL, Detail: reasonUserinfo}
	}
	if u.Fragment != "" || u.RawFragment != "" {
		return &Error{Kind: ErrBadURL, Detail: reasonFragment}
	}
	host := u.Hostname() // strips brackets and port, keeps zone text
	if host == "" {
		return &Error{Kind: ErrBadURL, Detail: reasonEmptyHost}
	}
	if port := u.Port(); port != "" && port != "443" {
		return &Error{Kind: ErrBadURL, Detail: reasonPort}
	}
	if strings.Contains(host, "%") {
		// url.URL keeps a zone as part of Hostname ("fe80::1%eth0").
		return &Error{Kind: ErrBadURL, Detail: reasonZone}
	}
	if len(host) > 253 {
		return &Error{Kind: ErrBadURL, Detail: reasonHostChars}
	}
	// IP literals take the address verdict (in ResolveHost / dial); only
	// names go through the spelling checks here.
	if _, err := netip.ParseAddr(host); err == nil {
		return nil
	}
	if !allowedHostName(host) {
		if !strings.Contains(strings.ToLower(host), ".") {
			return &Error{Kind: ErrBadURL, Detail: reasonSingleLabel}
		}
		return &Error{Kind: ErrBadURL, Detail: reasonInternalName}
	}
	return nil
}

// internalSuffixes are name spellings that never denote a public origin.
var internalSuffixes = []string{
	".localhost",
	".local",
	".localdomain",
	".internal",
	".lan",
	".home",
	".corp",
	".intranet",
}

// allowedHostName is the supplementary name layer: localhost in any case,
// single-label names, and internal-suffixed multi-label names are refused.
func allowedHostName(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "localhost" {
		return false
	}
	if !strings.Contains(h, ".") {
		return false
	}
	for _, sfx := range internalSuffixes {
		if strings.HasSuffix(h, sfx) {
			return false
		}
	}
	return true
}

// ResolveHost resolves host and returns its validated address list (§5.9):
// every answer of the whole A+AAAA set must be a public address — one
// private answer in a mixed set rejects the entire set, so a rebinding
// answer cannot slip the next dial to an internal target. IP literals
// skip DNS and are validated directly.
func (p *Policy) ResolveHost(ctx context.Context, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		if !AllowedIP(ip) {
			return nil, &Error{Kind: ErrBlocked, Detail: host}
		}
		return []netip.Addr{ip}, nil
	}
	addrs, err := p.resolver().LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, &Error{Kind: ErrNet, Detail: StageDNS}
	}
	if len(addrs) == 0 {
		return nil, &Error{Kind: ErrBlocked, Detail: host}
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		a = a.Unmap()
		if !AllowedIP(a) {
			return nil, &Error{Kind: ErrBlocked, Detail: host}
		}
		out = append(out, a)
	}
	return out, nil
}

// special4 lists the IPv4 prefixes refused in addition to the builtin
// verdicts (IANA IPv4 Special-Purpose Registry, conservative first
// version: CGNAT shared space, IETF protocol assignments, documentation
// TEST-NETs, benchmarking, the deprecated 6to4 relay, AMPRNet, reserved
// 240/4, and broadcast). Builtin netip verdicts already cover 10/8,
// 127/8, 169.254/16, 172.16/12, 192.168/16, and 224/4; 0.0.0.0/8 is
// listed because IsUnspecified only matches 0.0.0.0 itself.
var special4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),          // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),      // CGNAT shared address space
	netip.MustParsePrefix("192.0.0.0/24"),       // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),       // TEST-NET-1 (documentation)
	netip.MustParsePrefix("192.52.193.0/24"),    // AMPRNet (not globally reachable)
	netip.MustParsePrefix("192.88.0.0/24"),      // deprecated 6to4 relay anycast
	netip.MustParsePrefix("198.18.0.0/15"),      // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"),    // TEST-NET-2 (documentation)
	netip.MustParsePrefix("203.0.113.0/24"),     // TEST-NET-3 (documentation)
	netip.MustParsePrefix("240.0.0.0/4"),        // reserved for future use
	netip.MustParsePrefix("255.255.255.255/32"), // limited broadcast
}

// special6 lists the IPv6 prefixes refused in addition to the builtin
// verdicts (IANA IPv6 Special-Purpose Registry): the discard-only block,
// NAT64 (well-known and local-use), Teredo, benchmarking, documentation,
// 6to4, the deprecated 6bone space, and SRv6. Builtin verdicts already
// cover ::/128, ::1/128, ::ffff:0:0/96 (via Unmap), fc00::/7 (IsPrivate),
// fe80::/10, and ff00::/8.
var special6 = []netip.Prefix{
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64 well-known prefix
	netip.MustParsePrefix("64:ff9b:1::/48"), // NAT64 local-use
	netip.MustParsePrefix("100::/64"),       // discard-only
	netip.MustParsePrefix("2001::/32"),      // Teredo
	netip.MustParsePrefix("2001:2::/48"),    // benchmarking
	netip.MustParsePrefix("2001:db8::/32"),  // documentation
	netip.MustParsePrefix("2002::/16"),      // 6to4
	netip.MustParsePrefix("3ffe::/20"),      // deprecated 6bone
	netip.MustParsePrefix("5f00::/16"),      // SRv6 (not globally reachable)
}

// AllowedIP is the address verdict (§5.9): a dial target must be a global
// unicast address outside every private/special/transition range, after
// IPv4-mapped forms are unwrapped. It is the single authority — the name
// checks in ValidateURL only supplement it.
func AllowedIP(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() {
		return false
	}
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() ||
		a.IsLinkLocalMulticast() || a.IsMulticast() || a.IsUnspecified() ||
		!a.IsGlobalUnicast() {
		return false
	}
	if a.Is4() {
		for _, pfx := range special4 {
			if pfx.Contains(a) {
				return false
			}
		}
		return true
	}
	for _, pfx := range special6 {
		if pfx.Contains(a) {
			return false
		}
	}
	return true
}
