package notify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/egress"
)

// proxyChildEnv marks the child process TestNotifier_BehindAPrivateProxyFailsClosed
// re-runs itself in.
const proxyChildEnv = "PAD_NOTIFY_PROXY_TEST_CHILD"

// proxyChildDone is the line the child logs when every check ran, so a child
// that silently ran no test cannot pass for one that passed.
const proxyChildDone = "notify-proxy-child: all checks ran"

// Regression (fix round for finding #13): the notify clients are built on
// egress.GuardedTransport, and webhook targets are chosen by org admins, so an
// HTTP(S)_PROXY on a private address must not become a way around the SSRF
// guard. That proxy resolves the target name on its own — a host that passed
// validation can later rebind, or stop resolving here — and it can reach this
// server's private network. Every proxied delivery must therefore fail closed
// (refused at the hop to the proxy) and never reach the proxy; a literal
// internal target is refused before a proxy is even dialled.
//
// http.ProxyFromEnvironment reads the environment once per process, so the
// checks run in a child process that starts with HTTP_PROXY and HTTPS_PROXY
// pointing at a counting proxy on loopback. Targets are literal addresses:
// nothing resolves names, and no dial gets past the guard.
func TestNotifier_BehindAPrivateProxyFailsClosed(t *testing.T) {
	if os.Getenv(proxyChildEnv) != "" {
		runProxiedDeliveryChecks(t)
		return
	}

	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxied.Add(1) // a CONNECT tunnel request or a forwarded POST
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestNotifier_BehindAPrivateProxyFailsClosed$", "-test.count=1", "-test.v", "-test.timeout=2m")
	cmd.Env = append(environWithoutProxy(),
		proxyChildEnv+"=1",
		"HTTP_PROXY="+proxy.URL,
		"HTTPS_PROXY="+proxy.URL,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child process failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), proxyChildDone) {
		t.Fatalf("the child process did not run the checks:\n%s", out)
	}
	if n := proxied.Load(); n != 0 {
		t.Errorf("the private proxy received %d requests; every proxied delivery must be refused before it", n)
	}
}

// environWithoutProxy is the current environment minus every variable
// http.ProxyFromEnvironment reads (names compared case-insensitively, as
// Windows does), so only the child's own proxy settings apply.
func environWithoutProxy() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(name) {
		case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "REQUEST_METHOD":
			continue
		}
		env = append(env, kv)
	}
	return env
}

func runProxiedDeliveryChecks(t *testing.T) {
	const target = "https://203.0.113.10/hook" // public documentation address: passes validation without DNS
	proxyURL, err := url.Parse(os.Getenv("HTTPS_PROXY"))
	if err != nil || proxyURL.Host == "" {
		t.Fatalf("HTTPS_PROXY = %q: %v", os.Getenv("HTTPS_PROXY"), err)
	}
	probe, err := http.NewRequest(http.MethodPost, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := http.ProxyFromEnvironment(probe); err != nil || p == nil || p.Host != proxyURL.Host {
		t.Fatalf("the proxy environment is not in effect for %s: proxy=%v err=%v", target, p, err)
	}

	var notifiers []AlertNotifier
	for _, kind := range ValidChannelKinds {
		n, err := BuildChannelNotifier(kind, target, "s3cret", BlockInternalTargets)
		if err != nil {
			t.Fatalf("BuildChannelNotifier(%s): %v", kind, err)
		}
		notifiers = append(notifiers, n)
	}
	d, err := New(Config{
		WebhookURL: target, TeamsURL: target, SlackURL: target,
		JiraURL: target, JiraEmail: "ops@example.com", JiraToken: "token", JiraProject: "PAD",
		MaxAttempts: 1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	notifiers = append(notifiers, d.notifiers...)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, n := range notifiers {
		setSingleAttempt(n)
		err := n.Notify(ctx, sampleEvent())
		if !errors.Is(err, egress.ErrInternalAddress) || !strings.Contains(err.Error(), "proxyconnect") {
			t.Errorf("%s: want the hop to the private proxy refused, got %v", n.Name(), err)
		}
	}

	// A literal internal target is judged in the proxy hook, before any dial.
	// (Validation never lets one be saved; the client must refuse it anyway.)
	wh, ok := notifiers[0].(*WebhookNotifier)
	if !ok {
		t.Fatalf("notifiers[0] is %T, want *WebhookNotifier", notifiers[0])
	}
	internal := &WebhookNotifier{URL: "https://169.254.169.254/latest/meta-data", Client: wh.Client, MaxAttempts: 1}
	if err := internal.Notify(ctx, sampleEvent()); !errors.Is(err, egress.ErrInternalAddress) || strings.Contains(err.Error(), "proxyconnect") {
		t.Errorf("an internal target must be refused before the proxy is dialled, got %v", err)
	}
	t.Log(proxyChildDone)
}

// setSingleAttempt turns off retries so a refused delivery fails at once.
func setSingleAttempt(n AlertNotifier) {
	switch n := n.(type) {
	case *WebhookNotifier:
		n.MaxAttempts = 1
	case *TeamsNotifier:
		n.MaxAttempts = 1
	case *SlackNotifier:
		n.MaxAttempts = 1
	case *JiraNotifier:
		n.MaxAttempts = 1
	}
}
