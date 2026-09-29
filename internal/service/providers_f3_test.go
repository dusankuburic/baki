package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/egress"
	"pad-analyzer/internal/testutil"
)

// The org credential's lender is whoever supplied its secret: another
// admin's edit that keeps the stored secret (a deployment name) changes the
// last writer only. When the lender stops being an admin the credential is
// disabled anyway — it used to survive because the delete matched the last
// writer — and the GET names the lender, not the editor. The last writer's
// departure keeps disabling it too.
func TestOrgAzureFoundryConfig_LenderTracking(t *testing.T) {
	configureAzureForTests(t)
	orgKeys := testutil.NewFakeOrgSecretStore()
	svc := newOrgProviderService(t, testutil.NewFakeSecretStore(), orgKeys)
	withSecret := ai.AzureFoundryConfig{Endpoint: "https://" + azureTestHost, Auth: ai.AzureFoundryAuth{Mode: "api_key", Key: "lender-key"}, ChatDeployment: "chat1"}
	deploymentOnly := ai.AzureFoundryConfig{Endpoint: "https://" + azureTestHost, Auth: ai.AzureFoundryAuth{Mode: "api_key"}, ChatDeployment: "chat2"}

	// A saves the secret; B edits a deployment only.
	at, err := svc.SaveOrgAzureFoundryConfig("org-1", "admin-a", "", withSecret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SaveOrgAzureFoundryConfig("org-1", "admin-b", at, deploymentOnly); err != nil {
		t.Fatalf("deployment-only edit: %v", err)
	}
	pub, err := svc.GetOrgAzureFoundryConfig("org-1")
	if err != nil {
		t.Fatal(err)
	}
	if pub.SecretSetByID != "admin-a" || pub.UpdatedByID != "admin-b" {
		t.Errorf("after B's edit: lender=%q writer=%q, want admin-a / admin-b", pub.SecretSetByID, pub.UpdatedByID)
	}
	raw, _ := orgKeys.GetOrg("org-1", "azure-foundry")
	if stored, _ := ai.ParseAzureFoundryConfig(raw); stored.Auth.Key != "lender-key" || stored.ChatDeployment != "chat2" {
		t.Fatalf("stored = %+v, want A's key with B's deployment", stored)
	}

	// A is demoted: A's secret must not stay live.
	disabled, err := svc.DisableOrgAzureFoundryConfigSavedBy("org-1", "admin-a")
	if err != nil || !disabled {
		t.Fatalf("lender demoted: disabled=%v err=%v, want the credential disabled", disabled, err)
	}
	if has, _ := orgKeys.HasOrg("org-1", "azure-foundry"); has {
		t.Fatal("the departed lender's secret is still stored")
	}

	// The last writer's departure still disables it (org-2), and account
	// erasure finds a credential by its lender (org-3).
	at2, _ := svc.SaveOrgAzureFoundryConfig("org-2", "admin-a", "", withSecret)
	if _, err := svc.SaveOrgAzureFoundryConfig("org-2", "admin-b", at2, deploymentOnly); err != nil {
		t.Fatal(err)
	}
	if disabled, err := svc.DisableOrgAzureFoundryConfigSavedBy("org-2", "admin-b"); err != nil || !disabled {
		t.Errorf("last writer demoted: disabled=%v err=%v", disabled, err)
	}
	at3, _ := svc.SaveOrgAzureFoundryConfig("org-3", "admin-a", "", withSecret)
	if _, err := svc.SaveOrgAzureFoundryConfig("org-3", "admin-c", at3, deploymentOnly); err != nil {
		t.Fatal(err)
	}
	if orgs, err := svc.DisableOrgCredentialsSavedBy("admin-a"); err != nil || strings.Join(orgs, ",") != "org-3" {
		t.Errorf("lender's account erased: disabled %v (%v), want org-3", orgs, err)
	}

	// A new secret makes its supplier the lender: the previous lender's
	// departure no longer touches it.
	at4, _ := svc.SaveOrgAzureFoundryConfig("org-4", "admin-a", "", withSecret)
	rotated := withSecret
	rotated.Auth.Key = "b-key"
	if _, err := svc.SaveOrgAzureFoundryConfig("org-4", "admin-b", at4, rotated); err != nil {
		t.Fatal(err)
	}
	if pub, _ := svc.GetOrgAzureFoundryConfig("org-4"); pub.SecretSetByID != "admin-b" {
		t.Errorf("after a new secret the lender is %q, want admin-b", pub.SecretSetByID)
	}
	if disabled, _ := svc.DisableOrgAzureFoundryConfigSavedBy("org-4", "admin-a"); disabled {
		t.Error("a former lender's departure disabled a credential they no longer lend")
	}
}

// namedProvider gives a scripted provider its own id, so each run's
// failures count against a circuit breaker of its own (the registry is
// process-wide).
var namedProviderSeq atomic.Int64

type namedProvider struct {
	*scriptedProvider
	id string
}

func (p *namedProvider) ID() string { return p.id }

// A connection test never hands the client the raw text of a connection
// failure: it names the operator's proxy and the resolved address. The
// provider's own answers keep their text.
func TestTestProviderConnection_ConnectionFailuresAreFixedCopy(t *testing.T) {
	proxyDown := fmt.Errorf("%w: %w", ai.ErrProviderDown, &url.Error{Op: "Post", URL: "https://res.openai.azure.com/openai/v1/chat/completions",
		Err: &net.OpError{Op: "proxyconnect", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP("10.20.30.40"), Port: 3128}, Err: errors.New("connect: connection refused")}})
	guarded := fmt.Errorf("%w: %w", ai.ErrProviderDown, &url.Error{Op: "Post", URL: "https://res.openai.azure.com/x",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: fmt.Errorf("%w %s", egress.ErrInternalAddress, "169.254.169.254")}})
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"proxy failure": {proxyDown, "outbound proxy"},
		"egress guard":  {guarded, "internal network address"},
	} {
		t.Run(name, func(t *testing.T) {
			sp := &namedProvider{scriptedProvider: &scriptedProvider{FakeProvider: &testutil.FakeProvider{}, def: "chat1",
				chat: func(string) error { return tc.err }}, id: fmt.Sprintf("f3-%s-%d", name, namedProviderSeq.Add(1))}
			swapProvider(t, "azure-foundry", func(string) ai.Provider { return sp }, restoreAzure)
			secrets := testutil.NewFakeSecretStore()
			_ = secrets.Save("u1", "azure-foundry", "k")
			svc := newProviderServiceWithFakeSecrets(t, secrets)
			res, err := svc.TestProviderConnection(context.Background(), "u1", "azure-foundry")
			if err != nil {
				t.Fatal(err)
			}
			if res.Ok || !strings.HasPrefix(res.Error, "could not connect to the endpoint: ") || !strings.Contains(res.Error, tc.want) {
				t.Errorf("result = %+v, want fixed copy containing %q", res, tc.want)
			}
			for _, leak := range []string{"10.20.30.40", "3128", "169.254.169.254", "proxyconnect", "dial"} {
				if strings.Contains(res.Error, leak) {
					t.Errorf("error leaks %q: %q", leak, res.Error)
				}
			}
		})
	}

	// An always-thinking model that spends the test's 16 tokens on hidden
	// reasoning stops at its output limit: the call worked, so the
	// credentials do.
	t.Run("output limit is a working connection", func(t *testing.T) {
		sp := &namedProvider{scriptedProvider: &scriptedProvider{FakeProvider: &testutil.FakeProvider{}, def: "claude:opus",
			chat: func(string) error { return fmt.Errorf("%w (stop_reason max_tokens)", ai.ErrOutputLimit) }},
			id: fmt.Sprintf("f3-output-limit-%d", namedProviderSeq.Add(1))}
		swapProvider(t, "azure-foundry", func(string) ai.Provider { return sp }, restoreAzure)
		secrets := testutil.NewFakeSecretStore()
		_ = secrets.Save("u1", "azure-foundry", "k")
		svc := newProviderServiceWithFakeSecrets(t, secrets)
		res, err := svc.TestProviderConnection(context.Background(), "u1", "azure-foundry")
		if err != nil || !res.Ok || res.Error != "" {
			t.Errorf("result = %+v, %v; want ok", res, err)
		}
		if tried := sp.triedModels(); len(tried) != 1 {
			t.Errorf("tried %v, want the first candidate only", tried)
		}
	})

	// Not a connection failure: the text is the provider's.
	if got := connectionTestError("azure-foundry", "", errors.New("deployment chat1 not found")); got != "deployment chat1 not found" {
		t.Errorf("provider answer rewritten: %q", got)
	}
	if got := connectionTestError("azure-foundry", "the project endpoint", fmt.Errorf("the project deployments listing failed: %w", proxyDown)); !strings.HasPrefix(got, "could not connect to the project endpoint: ") || strings.Contains(got, "10.20.30.40") {
		t.Errorf("project probe failure = %q", got)
	}
}
