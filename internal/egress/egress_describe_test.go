package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"
)

// DescribeFailure turns connection failures into fixed copy that names no
// address — the proxy's and the resolved target's stay in the server log —
// and leaves every other error to the caller.
func TestDescribeFailure(t *testing.T) {
	proxyDial := &url.Error{Op: "Post", URL: "https://res.openai.azure.com/openai/v1/chat/completions",
		Err: &net.OpError{Op: "proxyconnect", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP("10.20.30.40"), Port: 3128}, Err: errors.New("connect: connection refused")}}
	guard := &url.Error{Op: "Post", URL: "https://res.openai.azure.com/x",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: fmt.Errorf("%w %s", ErrInternalAddress, "10.1.2.3")}}
	dns := &url.Error{Op: "Get", URL: "https://nope.openai.azure.com/", Err: &net.OpError{Op: "dial", Net: "tcp",
		Err: &net.DNSError{Err: "no such host", Name: "nope.openai.azure.com", Server: "10.0.0.53:53"}}}
	flattened := errors.New(`ClientSecretCredential: Post "https://login.microsoftonline.com/t/oauth2/v2.0/token": proxyconnect tcp: dial tcp 192.168.7.7:8080: connect: connection refused`)
	timeout := &url.Error{Op: "Post", URL: "https://res.openai.azure.com/x", Err: context.DeadlineExceeded}
	tlsErr := errors.New(`Post "https://res.openai.azure.com/x": tls: failed to verify certificate: x509: certificate signed by unknown authority`)
	// Windows' wording of the same failures (the error chain keeps the syscall text).
	winOp := func(text string) error {
		return &url.Error{Op: "Post", URL: "https://hooks.example.com/x", Err: &net.OpError{Op: "dial", Net: "tcp",
			Addr: &net.TCPAddr{IP: net.ParseIP("10.9.8.7"), Port: 443}, Err: errors.New(text)}}
	}
	winRefused := winOp("connectex: No connection could be made because the target machine actively refused it.")
	winReset := winOp("wsarecv: An existing connection was forcibly closed by the remote host.")
	winTimeout := winOp("connectex: A connection attempt failed because the connected party did not properly respond after a period of time, or established connection failed because connected host has failed to respond.")

	cases := []struct {
		name string
		err  error
		want string // "" = not a connection failure
	}{
		{"proxy refused", proxyDial, "outbound proxy"},
		{"egress guard", guard, "internal network address"},
		{"unverified", fmt.Errorf("x: %w", ErrUnverifiedTarget), "could not be verified"},
		{"dns", dns, "does not resolve"},
		{"flattened proxy failure", flattened, "outbound proxy"},
		{"timeout", timeout, "timed out"},
		{"tls", tlsErr, "TLS handshake"},
		{"windows refused", winRefused, "was refused"},
		{"windows reset", winReset, "closed unexpectedly"},
		{"windows timeout", winTimeout, "timed out"},
		{"windows refused, flattened", errors.New("Post \"https://hooks.example.com/x\": dial tcp 10.9.8.7:443: connectex: No connection could be made because the target machine actively refused it."), "was refused"},
		{"provider answer", errors.New("provider is currently unavailable: Service overloaded (status 503)"), ""},
		{"nil", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg, ok := DescribeFailure(c.err)
			if c.want == "" {
				if ok {
					t.Errorf("DescribeFailure(%v) = %q, want not a connection failure", c.err, msg)
				}
				return
			}
			if !ok || !strings.Contains(msg, c.want) {
				t.Errorf("DescribeFailure = %q, %v; want %q", msg, ok, c.want)
			}
			for _, leak := range []string{"10.20.30.40", "3128", "10.1.2.3", "10.0.0.53", "192.168.7.7", "8080", "login.microsoftonline.com", "10.9.8.7"} {
				if strings.Contains(msg, leak) {
					t.Errorf("copy leaks %q: %q", leak, msg)
				}
			}
		})
	}
}
