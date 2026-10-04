package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"

	"pad-analyzer/internal/egress"
)

// seamTransport is an in-memory RoundTripper standing in for the network: it
// records every request's host and answers each with a chat completion.
type seamTransport struct {
	mu    sync.Mutex
	hosts []string
}

func (s *seamTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.hosts = append(s.hosts, r.URL.Host)
	s.mu.Unlock()
	body, _ := json.Marshal(chatCompletionJSON("via seam", "gpt-4o"))
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    r,
	}, nil
}

// AzureFoundryOptions.Transport (a test seam) replaces the network under the
// Azure client and the Entra ID credential's pipeline — whatever the egress
// policy — while redirects stay refused.
func TestConfigureAzureFoundry_TransportSeam(t *testing.T) {
	ResetAzureFoundryCaches()
	t.Cleanup(ResetAzureFoundryCaches)
	rt := &seamTransport{}
	saveAzureRuntime(t, AzureFoundryOptions{Egress: egress.BlockInternal, Transport: rt})

	client := azureHTTPClient()
	if client.Transport != http.RoundTripper(rt) {
		t.Fatalf("azure client transport = %T, want the seam", client.Transport)
	}
	if client.CheckRedirect == nil {
		t.Error("the seam client must still refuse redirects")
	}
	if opts := entraCredentialOptions(); opts.Transport != client {
		t.Errorf("the Entra ID credential must use the azure client, got %T", opts.Transport)
	}

	p := NewAzureFoundryProvider(`{"endpoint":"https://my-res.openai.azure.com","auth":{"mode":"api_key","key":"k"},"chatDeployment":"chat1"}`)
	resp, err := p.Chat(context.Background(), Request{Model: "chat1", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("chat through the seam: %v", err)
	}
	if resp.Content != "via seam" {
		t.Errorf("content = %q, want the seam's answer", resp.Content)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.hosts) != 1 || rt.hosts[0] != "my-res.openai.azure.com" {
		t.Errorf("seam saw hosts %v, want one request to my-res.openai.azure.com", rt.hosts)
	}
}
