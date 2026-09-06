// Package netguard decides whether schemaver is allowed to connect somewhere.
//
// It exists because of a specific hazard. Registering a database means asking
// this server to open a TCP connection to an address someone else chose. On a
// deployment where anyone can create an account, that turns the connection test
// into a probe of whatever network this server sits in — and the test reports
// precisely which failure occurred, which makes it an efficient and reliable
// port scanner.
//
// The addresses that matter most are not the obviously internal ones. Cloud
// providers serve instance credentials from 169.254.169.254, so an unguarded
// deployment on such a provider will hand out its own IAM role to anybody who
// asks it to connect there.
//
// # Why this is a policy and not a constant
//
// schemaver's primary deployment (D-005) runs *inside* a private network and
// connects to private databases. Blocking private addresses there would break
// the product entirely. So the restriction is tied to who may create an account:
// a deployment only strangers cannot join has no reason to distrust its own
// operator.
package netguard

import (
	"context"
	"fmt"
	"net"
	"net/netip"
)

// Policy decides which addresses are reachable.
type Policy struct {
	// AllowPrivate permits loopback, private, link-local and carrier-grade NAT
	// addresses. Correct for a self-hosted deployment whose whole purpose is
	// reaching an internal database; unsafe wherever untrusted people can
	// register a target.
	AllowPrivate bool
}

// Blocked reports why an address is refused, or nil if it is permitted.
type Blocked struct {
	Host   string
	IP     netip.Addr
	Reason string
}

func (b *Blocked) Error() string {
	return fmt.Sprintf("refusing to connect to %s (%s): %s", b.Host, b.IP, b.Reason)
}

// Resolver looks up a hostname. Replaceable in tests.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Check resolves host and rejects it if any address it answers to is one this
// policy forbids.
//
// Resolution happens first and the *addresses* are judged, never the name. A
// hostname check is trivially defeated by pointing a public name at an internal
// address, which is the standard way this class of guard is bypassed.
//
// Every resolved address must pass. A name answering with one public and one
// internal address is rejected: which one gets dialled is not ours to control.
//
// # The limitation this does not close
//
// Between this check and the connection, DNS can change its answer — rebinding.
// Closing it fully means dialling the address that was validated rather than
// resolving again, which requires control of the dialler. Callers that have it
// should use Verified to obtain the address and connect to that.
func (p Policy) Check(ctx context.Context, resolver Resolver, host string) error {
	_, err := p.Verified(ctx, resolver, host)
	return err
}

// Verified resolves host, rejects it under this policy, and returns the
// addresses that passed so a caller can connect to one directly.
func (p Policy) Verified(ctx context.Context, resolver Resolver, host string) ([]netip.Addr, error) {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	// A literal address needs no lookup, and passing one through the resolver
	// would be a pointless round trip.
	if addr, err := netip.ParseAddr(host); err == nil {
		if reason := p.reject(addr); reason != "" {
			return nil, &Blocked{Host: host, IP: addr, Reason: reason}
		}
		return []netip.Addr{addr}, nil
	}

	addrs, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("could not resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%s resolved to no addresses", host)
	}
	for _, addr := range addrs {
		if reason := p.reject(addr.Unmap()); reason != "" {
			return nil, &Blocked{Host: host, IP: addr, Reason: reason}
		}
	}
	return addrs, nil
}

// reject names why an address is forbidden, or returns empty if it is fine.
func (p Policy) reject(addr netip.Addr) string {
	if !addr.IsValid() {
		return "not a valid address"
	}
	// Unroutable regardless of policy: these are never a legitimate target and
	// several have surprising behaviour when dialled.
	switch {
	case addr.IsUnspecified():
		return "the unspecified address is not a destination"
	case addr.IsMulticast(), addr.IsInterfaceLocalMulticast(), addr.IsLinkLocalMulticast():
		return "multicast addresses are not database servers"
	}

	if p.AllowPrivate {
		return ""
	}

	switch {
	case addr.IsLoopback():
		return "loopback addresses reach this server itself"
	case addr.IsLinkLocalUnicast():
		// 169.254.169.254 lives here. Cloud instance metadata, and the reason
		// this guard is not optional on a public deployment.
		return "link-local addresses include cloud instance metadata"
	case addr.IsPrivate():
		return "private addresses are inside this deployment's own network"
	case isCarrierGradeNAT(addr):
		return "carrier-grade NAT addresses are inside a provider's network"
	}
	return ""
}

// carrierGradeNAT is 100.64.0.0/10, which net.IP does not classify as private
// but which is just as internal in practice — several providers use it for
// inter-node traffic.
var carrierGradeNAT = netip.MustParsePrefix("100.64.0.0/10")

func isCarrierGradeNAT(addr netip.Addr) bool {
	return addr.Is4() && carrierGradeNAT.Contains(addr)
}
