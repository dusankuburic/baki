package egress

import (
	"context"
	"errors"
	"net"
	"testing"
)

// PAD_AI_ALLOW_PRIVATE_EGRESS (AllowPrivateNetwork) opens RFC 1918 and
// unique-local addresses only: loopback, link-local (the metadata service),
// unspecified and the other special-purpose ranges stay refused, and an
// RFC 1918 address smuggled inside a NAT64 / 6to4 address is not "private".
func TestPolicyRefuses(t *testing.T) {
	cases := []struct {
		ip                       string
		block, loopback, private bool // want Refuses under each policy
	}{
		{"10.1.2.3", true, true, false},
		{"172.16.0.1", true, true, false},
		{"192.168.1.1", true, true, false},
		{"fd12:3456::1", true, true, false},
		{"::ffff:10.0.0.1", true, true, false},
		{"127.0.0.1", true, true, true},
		{"::1", true, true, true},
		{"169.254.169.254", true, true, true},
		{"fe80::1", true, true, true},
		{"0.0.0.0", true, true, true},
		{"::", true, true, true},
		{"168.63.129.16", true, true, true},
		{"100.64.0.1", true, true, true},
		{"198.18.0.1", true, true, true},
		{"64:ff9b::a00:1", true, true, true},
		{"2002:a00:1::1", true, true, true},
		{"8.8.8.8", false, false, false},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("test bug: %q does not parse", c.ip)
		}
		for _, p := range []struct {
			policy Policy
			want   bool
		}{{BlockInternal, c.block}, {AllowLoopback, c.loopback}, {AllowPrivateNetwork, c.private}} {
			if got := p.policy.Refuses(ip); got != p.want {
				t.Errorf("Policy(%d).Refuses(%s) = %v, want %v", p.policy, c.ip, got, p.want)
			}
		}
	}
}

// The dial guard applies the policy at connect time: under
// AllowPrivateNetwork a private address is dialled, the metadata address and
// loopback — "localhost" included — are not.
func TestGuardDialContext_AllowPrivateNetwork(t *testing.T) {
	for addr, ok := range map[string]bool{
		"10.0.0.5:443":            true,
		"[fd00::5]:443":           true,
		"169.254.169.254:80":      false,
		"127.0.0.1:443":           false,
		"[::1]:443":               false,
		"0.0.0.0:443":             false,
		"168.63.129.16:80":        false,
		"[64:ff9b::a9fe:a9fe]:80": false,
	} {
		if err := checkDialAddress(addr, AllowPrivateNetwork); (err == nil) != ok {
			t.Errorf("checkDialAddress(%q, AllowPrivateNetwork) = %v, want ok=%v", addr, err, ok)
		}
	}
	dial := GuardDialContext(&net.Dialer{}, AllowPrivateNetwork)
	if _, err := dial(context.Background(), "tcp", "localhost:1"); err == nil {
		t.Fatal("AllowPrivateNetwork must not dial localhost")
	} else if !errors.Is(err, ErrInternalAddress) {
		t.Errorf("localhost under AllowPrivateNetwork: %v, want ErrInternalAddress", err)
	}
	if err := RejectPrivateHost("127.0.0.1", AllowPrivateNetwork); err == nil {
		t.Error("RejectPrivateHost must refuse loopback under AllowPrivateNetwork")
	}
	if err := RejectPrivateHost("10.0.0.1", AllowPrivateNetwork); err != nil {
		t.Errorf("RejectPrivateHost(10.0.0.1, AllowPrivateNetwork) = %v, want allowed", err)
	}
}
