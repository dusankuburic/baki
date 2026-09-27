package egress

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
)

// Every host in this file is a literal IP, "localhost" or empty: net.LookupIP
// short-circuits literals and rejects "" before touching a resolver, so none
// of these tests depend on DNS.

func TestIsInternalIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"10.0.0.1", true},
		{"192.168.1.1", true},
		{"172.16.0.1", true},
		{"169.254.169.254", true}, // cloud metadata
		{"::1", true},
		{"fe80::1", true},
		{"fc00::1", true},
		{"0.0.0.0", true},
		{"::", true},
		{"8.8.8.8", false},
		{"2606:4700::1111", false},
	}
	for _, c := range cases {
		t.Run(c.ip, func(t *testing.T) {
			ip := net.ParseIP(c.ip)
			if ip == nil {
				t.Fatalf("test bug: %q does not parse", c.ip)
			}
			if got := IsInternalIP(ip); got != c.want {
				t.Errorf("IsInternalIP(%s) = %v, want %v", c.ip, got, c.want)
			}
		})
	}
}

func TestIsLoopbackHost(t *testing.T) {
	cases := map[string]bool{
		"localhost":   true,
		"127.0.0.1":   true,
		"127.1.2.3":   true,
		"::1":         true,
		"example.com": false,
		"10.0.0.1":    false, // private but not loopback: the hatch is loopback only
		"":            false,
	}
	for host, want := range cases {
		if got := IsLoopbackHost(host); got != want {
			t.Errorf("IsLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestRejectPrivateHost(t *testing.T) {
	t.Run("private literal refused under BlockInternal", func(t *testing.T) {
		err := RejectPrivateHost("10.0.0.5", BlockInternal)
		if err == nil {
			t.Fatal("expected an error for a private literal IP")
		}
		if !strings.Contains(err.Error(), "internal targets are not allowed") {
			t.Errorf("unexpected error text: %v", err)
		}
		if strings.Contains(err.Error(), "alert URL") {
			t.Errorf("egress must not use notify's wording: %v", err)
		}
	})
	t.Run("loopback literal refused under BlockInternal", func(t *testing.T) {
		for _, host := range []string{"127.0.0.1", "::1", "localhost"} {
			if err := RejectPrivateHost(host, BlockInternal); err == nil {
				t.Errorf("RejectPrivateHost(%q, BlockInternal) = nil, want error", host)
			}
		}
	})
	t.Run("loopback passes under AllowLoopback", func(t *testing.T) {
		for _, host := range []string{"127.0.0.1", "::1", "localhost"} {
			if err := RejectPrivateHost(host, AllowLoopback); err != nil {
				t.Errorf("RejectPrivateHost(%q, AllowLoopback) = %v, want nil", host, err)
			}
		}
	})
	t.Run("hatch is loopback only", func(t *testing.T) {
		for _, host := range []string{"169.254.169.254", "10.0.0.5", "192.168.1.10", "0.0.0.0"} {
			if err := RejectPrivateHost(host, AllowLoopback); err == nil {
				t.Errorf("RejectPrivateHost(%q, AllowLoopback) = nil, want error", host)
			}
		}
	})
	t.Run("empty host is an error", func(t *testing.T) {
		for _, policy := range []Policy{BlockInternal, AllowLoopback} {
			if err := RejectPrivateHost("", policy); err == nil {
				t.Errorf("RejectPrivateHost(\"\", %v) = nil, want error", policy)
			}
		}
	})
	t.Run("public literal passes", func(t *testing.T) {
		for _, host := range []string{"8.8.8.8", "2606:4700::1111"} {
			if err := RejectPrivateHost(host, BlockInternal); err != nil {
				t.Errorf("RejectPrivateHost(%q, BlockInternal) = %v, want nil", host, err)
			}
		}
	})
}

// fakeDialer records whether the wrapped base dialer was reached; it never
// opens a socket.
type fakeDialer struct {
	calls int
}

var errBaseReached = errors.New("base dialer reached")

func (f *fakeDialer) dial(_ context.Context, _, _ string) (net.Conn, error) {
	f.calls++
	return nil, errBaseReached
}

func TestGuardDialContext(t *testing.T) {
	ctx := context.Background()

	t.Run("BlockInternal refuses loopback before dialing", func(t *testing.T) {
		base := &fakeDialer{}
		dial := GuardDialContext(base.dial, BlockInternal)
		conn, err := dial(ctx, "tcp", "127.0.0.1:80")
		if conn != nil {
			t.Error("expected a nil conn")
		}
		if err == nil || !strings.Contains(err.Error(), "internal address") {
			t.Errorf("expected an internal-address refusal, got %v", err)
		}
		if errors.Is(err, errBaseReached) || base.calls != 0 {
			t.Errorf("base dialer must not be called on refusal (calls=%d, err=%v)", base.calls, err)
		}
	})
	t.Run("AllowLoopback lets loopback through to the base dialer", func(t *testing.T) {
		for _, addr := range []string{"127.0.0.1:80", "[::1]:80", "localhost:80"} {
			base := &fakeDialer{}
			dial := GuardDialContext(base.dial, AllowLoopback)
			_, err := dial(ctx, "tcp", addr)
			if !errors.Is(err, errBaseReached) || base.calls != 1 {
				t.Errorf("%s: expected the base dialer to be reached once (calls=%d, err=%v)", addr, base.calls, err)
			}
		}
	})
	t.Run("AllowLoopback still refuses private and link-local", func(t *testing.T) {
		for _, addr := range []string{"10.0.0.5:443", "169.254.169.254:80"} {
			base := &fakeDialer{}
			dial := GuardDialContext(base.dial, AllowLoopback)
			_, err := dial(ctx, "tcp", addr)
			if err == nil || !strings.Contains(err.Error(), "internal address") || base.calls != 0 {
				t.Errorf("%s: expected refusal without dialing (calls=%d, err=%v)", addr, base.calls, err)
			}
		}
	})
	t.Run("public address reaches the base dialer", func(t *testing.T) {
		base := &fakeDialer{}
		dial := GuardDialContext(base.dial, BlockInternal)
		_, err := dial(ctx, "tcp", "8.8.8.8:443")
		if !errors.Is(err, errBaseReached) || base.calls != 1 {
			t.Errorf("expected the base dialer to be reached once (calls=%d, err=%v)", base.calls, err)
		}
	})
	t.Run("address without a port is still guarded", func(t *testing.T) {
		base := &fakeDialer{}
		dial := GuardDialContext(base.dial, BlockInternal)
		_, err := dial(ctx, "tcp", "192.168.1.10")
		if err == nil || !strings.Contains(err.Error(), "internal address") || base.calls != 0 {
			t.Errorf("expected refusal without dialing (calls=%d, err=%v)", base.calls, err)
		}
	})
	t.Run("resolution failure falls through to the base dialer", func(t *testing.T) {
		// An empty host is rejected by net.LookupIP before any resolver runs.
		base := &fakeDialer{}
		dial := GuardDialContext(base.dial, BlockInternal)
		_, err := dial(ctx, "tcp", ":80")
		if !errors.Is(err, errBaseReached) || base.calls != 1 {
			t.Errorf("expected the base dialer to report the failure (calls=%d, err=%v)", base.calls, err)
		}
	})
}

func TestGuardedTransport(t *testing.T) {
	tr := GuardedTransport(BlockInternal)
	if tr == nil || tr == http.DefaultTransport {
		t.Fatal("expected a fresh clone of http.DefaultTransport")
	}
	if tr.DialContext == nil {
		t.Fatal("expected a wrapped DialContext")
	}
	_, err := tr.DialContext(context.Background(), "tcp", "169.254.169.254:80")
	if err == nil || !strings.Contains(err.Error(), "internal address") {
		t.Errorf("expected the dial guard on the transport, got %v", err)
	}
}

func TestNoRedirects(t *testing.T) {
	if err := NoRedirects(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("NoRedirects() = %v, want http.ErrUseLastResponse", err)
	}
}
