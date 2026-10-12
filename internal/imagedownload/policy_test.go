// Policy tests (§5.9): URL shape verdicts, the address verdict table, and
// the whole-answer-set DNS rule with an injected resolver.

package imagedownload

import (
	"context"
	"net/netip"
	"net/url"
	"testing"
)

func TestValidateURL(t *testing.T) {
	p := &Policy{}
	ok := func(raw string) {
		t.Helper()
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", raw, err)
		}
		if err := p.ValidateURL(u); err != nil {
			t.Errorf("ValidateURL(%s) = %v, want accepted", raw, err)
		}
	}
	bad := func(raw, wantDetail string) {
		t.Helper()
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", raw, err)
		}
		err = p.ValidateURL(u)
		if err == nil {
			t.Errorf("ValidateURL(%s) accepted, want %q", raw, wantDetail)
			return
		}
		e, ok := err.(*Error)
		if !ok || e.Kind != ErrBadURL || e.Detail != wantDetail {
			t.Errorf("ValidateURL(%s) = %v, want ErrBadURL %q", raw, err, wantDetail)
		}
	}

	ok("https://example.com/a.png")
	ok("https://example.com:443/a.png")
	ok("https://sub.example.com/a.png")
	ok("https://1.1.1.1/a.png")
	ok("https://[2606:4700:4700::1111]/a.png")

	bad("http://example.com/a.png", reasonScheme)
	bad("data:image/png;base64,AAAA", reasonScheme)
	bad("file:///etc/hosts", reasonScheme)
	bad("https://user:pw@example.com/a.png", reasonUserinfo)
	bad("https://user@example.com/a.png", reasonUserinfo)
	bad("https://example.com/a.png#frag", reasonFragment)
	bad("https://example.com:8443/a.png", reasonPort)
	bad("https://example.com:0/a.png", reasonPort)
	bad("https://[fe80::1%25eth0]/a.png", reasonZone)
	bad("https://localhost/a.png", reasonSingleLabel)
	bad("https://intranet/a.png", reasonSingleLabel)
	bad("https://nas.local/a.png", reasonInternalName)
	bad("https://corp.example.internal/a.png", reasonInternalName)
	bad("https://printer.home/a.png", reasonInternalName)
}

func TestAllowedIPTable(t *testing.T) {
	allow := []string{
		"1.1.1.1", "8.8.8.8", "93.184.216.34",
		"172.15.255.255", // just below 172.16/12
		"172.32.0.1",     // just above 172.16/12
		"192.169.0.1",    // just above 192.168/16
		"100.63.255.255", // just below CGNAT
		"100.128.0.1",    // just above CGNAT
		"2606:4700:4700::1111",
		"2a00:1450:4001:81b::200e",
	}
	deny := []string{
		"0.0.0.0", "0.1.2.3",
		"10.0.0.1", "10.255.255.255",
		"100.64.0.1", "100.127.255.254",
		"127.0.0.1", "127.8.8.8",
		"169.254.169.254", // metadata
		"172.16.0.1", "172.31.255.255",
		"192.0.0.1",
		"192.0.2.1",
		"192.52.193.1",
		"192.88.0.1",
		"192.168.1.1",
		"198.18.0.1", "198.19.255.255",
		"198.51.100.1",
		"203.0.113.1",
		"224.0.0.1", "239.1.1.1",
		"240.0.0.1", "255.1.2.3",
		"255.255.255.255",
		"::", "::1",
		"::ffff:127.0.0.1", // mapped loopback
		"::ffff:169.254.169.254",
		"64:ff9b::192.0.2.1",      // NAT64 of a doc address
		"64:ff9b:1::1",            // local NAT64
		"100::1",                  // discard
		"2001:0:1234::1",          // Teredo
		"2001:2::1",               // benchmark
		"2001:db8::1",             // documentation
		"2002:cb00:71ac::1",       // 6to4 of 203.0.113.172
		"fc00::1", "fd12:3456::1", // ULA
		"fe80::1",
		"ff02::1",
		"3ffe::1",
		"5f00::1",
	}
	for _, s := range allow {
		if !AllowedIP(netip.MustParseAddr(s)) {
			t.Errorf("AllowedIP(%s) = false, want true", s)
		}
	}
	for _, s := range deny {
		if AllowedIP(netip.MustParseAddr(s)) {
			t.Errorf("AllowedIP(%s) = true, want false", s)
		}
	}
	// 4-in-6 spelling of an allowed address passes after Unmap.
	if !AllowedIP(netip.MustParseAddr("::ffff:1.1.1.1")) {
		t.Error("AllowedIP(::ffff:1.1.1.1) = false, want true")
	}
}

// fakeResolver answers a fixed list per host.
type fakeResolver map[string][]netip.Addr

func (f fakeResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	if addrs, ok := f[host]; ok {
		return addrs, nil
	}
	return nil, &Error{Kind: ErrNet, Detail: StageDNS}
}

func TestResolveHostWholeAnswerSet(t *testing.T) {
	pub := netip.MustParseAddr("93.184.216.34")
	priv := netip.MustParseAddr("10.0.0.7")
	p := &Policy{Resolver: fakeResolver{
		"clean.example.com":  {pub},
		"mixed.example.com":  {pub, priv},
		"rebind.example.com": {netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("169.254.169.254")},
	}}

	if ips, err := p.ResolveHost(context.Background(), "clean.example.com"); err != nil || len(ips) != 1 {
		t.Errorf("clean resolve = (%v, %v), want one public IP", ips, err)
	}
	for _, host := range []string{"mixed.example.com", "rebind.example.com"} {
		if _, err := p.ResolveHost(context.Background(), host); err == nil {
			t.Errorf("ResolveHost(%s) accepted a mixed answer set", host)
		} else if e, ok := err.(*Error); !ok || e.Kind != ErrBlocked {
			t.Errorf("ResolveHost(%s) = %v, want ErrBlocked", host, err)
		}
	}

	// IP literals skip DNS and take the verdict directly.
	if _, err := p.ResolveHost(context.Background(), "1.1.1.1"); err != nil {
		t.Errorf("literal public IP rejected: %v", err)
	}
	if _, err := p.ResolveHost(context.Background(), "127.0.0.1"); err == nil {
		t.Error("literal loopback IP accepted")
	}
	// Empty answer sets are blocked, not dialed.
	empty := &Policy{Resolver: fakeResolver{}}
	_ = empty
	if _, err := (&Policy{Resolver: fakeResolver{"none.example.com": {}}}).ResolveHost(context.Background(), "none.example.com"); err == nil {
		t.Error("empty DNS answer accepted")
	}
}
