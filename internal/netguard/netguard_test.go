package netguard

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
)

// fakeResolver answers with whatever a test says a name points at.
type fakeResolver map[string][]string

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	raw, ok := f[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	out := make([]netip.Addr, 0, len(raw))
	for _, s := range raw {
		out = append(out, netip.MustParseAddr(s))
	}
	return out, nil
}

func strict() Policy { return Policy{} }

// TestBlocksInternalAddresses is the guard's reason for existing. Each of these
// is reachable from a server and none is a legitimate target for a stranger.
func TestBlocksInternalAddresses(t *testing.T) {
	for _, tc := range []struct{ addr, why string }{
		{"169.254.169.254", "cloud instance metadata — hands out IAM credentials"},
		{"127.0.0.1", "loopback reaches this server"},
		{"::1", "IPv6 loopback"},
		{"10.0.0.5", "private"},
		{"172.16.4.1", "private"},
		{"192.168.1.1", "private"},
		{"fd00::1", "IPv6 unique-local"},
		{"fe80::1", "IPv6 link-local"},
		{"100.64.0.1", "carrier-grade NAT, internal at several providers"},
		{"0.0.0.0", "unspecified"},
		{"224.0.0.1", "multicast"},
	} {
		if err := strict().Check(context.Background(), fakeResolver{}, tc.addr); err == nil {
			t.Errorf("%s was allowed (%s)", tc.addr, tc.why)
		}
	}
}

func TestAllowsPublicAddresses(t *testing.T) {
	for _, addr := range []string{"93.184.216.34", "8.8.8.8", "2606:2800:220:1:248:1893:25c8:1946"} {
		if err := strict().Check(context.Background(), fakeResolver{}, addr); err != nil {
			t.Errorf("%s was blocked: %v", addr, err)
		}
	}
}

// TestJudgesResolvedAddressesNotNames covers the standard bypass: a public
// hostname pointed at an internal address.
func TestJudgesResolvedAddressesNotNames(t *testing.T) {
	dns := fakeResolver{
		"metadata.example.com": {"169.254.169.254"},
		"db.example.com":       {"93.184.216.34"},
	}
	if err := strict().Check(context.Background(), dns, "metadata.example.com"); err == nil {
		t.Error("a public-looking name pointing at instance metadata was allowed")
	}
	if err := strict().Check(context.Background(), dns, "db.example.com"); err != nil {
		t.Errorf("a name pointing at a public address was blocked: %v", err)
	}
}

// TestRejectsMixedResolution checks a name answering with both a public and an
// internal address is refused. Which one gets dialled is not ours to choose.
func TestRejectsMixedResolution(t *testing.T) {
	dns := fakeResolver{"split.example.com": {"93.184.216.34", "10.1.2.3"}}
	err := strict().Check(context.Background(), dns, "split.example.com")
	if err == nil {
		t.Fatal("a name resolving to both public and private addresses was allowed")
	}
	var blocked *Blocked
	if !errors.As(err, &blocked) {
		t.Fatalf("want a Blocked error, got %T", err)
	}
	if blocked.IP.String() != "10.1.2.3" {
		t.Errorf("blamed %s, want the internal address", blocked.IP)
	}
}

// TestAllowPrivateRestoresSelfHosting covers D-005's actual deployment: inside a
// private network, reaching private databases is the entire point.
func TestAllowPrivateRestoresSelfHosting(t *testing.T) {
	p := Policy{AllowPrivate: true}
	for _, addr := range []string{"10.0.0.5", "192.168.1.10", "127.0.0.1", "169.254.169.254"} {
		if err := p.Check(context.Background(), fakeResolver{}, addr); err != nil {
			t.Errorf("%s blocked with AllowPrivate: %v", addr, err)
		}
	}
	// Still never routable, whatever the policy.
	for _, addr := range []string{"0.0.0.0", "224.0.0.1"} {
		if err := p.Check(context.Background(), fakeResolver{}, addr); err == nil {
			t.Errorf("%s allowed even though it is not a destination", addr)
		}
	}
}

// TestBlockedErrorNamesTheAddress checks a refusal is explicable. An operator
// hitting this on a legitimate internal host needs to know which address failed
// and why, or the fix is guesswork.
func TestBlockedErrorNamesTheAddress(t *testing.T) {
	dns := fakeResolver{"internal.example.com": {"10.9.8.7"}}
	err := strict().Check(context.Background(), dns, "internal.example.com")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, want := range []string{"internal.example.com", "10.9.8.7", "private"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal omits %q: %v", want, err)
		}
	}
}

// TestVerifiedReturnsAddresses supports connecting to the address that was
// checked, which is what closes the DNS-rebinding window.
func TestVerifiedReturnsAddresses(t *testing.T) {
	dns := fakeResolver{"db.example.com": {"93.184.216.34"}}
	addrs, err := strict().Verified(context.Background(), dns, "db.example.com")
	if err != nil {
		t.Fatalf("Verified: %v", err)
	}
	if len(addrs) != 1 || addrs[0].String() != "93.184.216.34" {
		t.Errorf("got %v, want the resolved public address", addrs)
	}
}

func TestUnresolvableHostIsAnError(t *testing.T) {
	if err := strict().Check(context.Background(), fakeResolver{}, "nowhere.invalid"); err == nil {
		t.Error("an unresolvable host was allowed")
	}
}
