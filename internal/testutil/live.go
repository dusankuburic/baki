package testutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/egress"
)

// The live-provider harness (R-testing-11, R-testing-12): the tests built with -tags live call real vendors, each
// provider gated on its own BAKI_LIVE_<PROVIDER>_KEY (the provider id upper-cased, "-" as "_": BAKI_LIVE_CLAUDE_KEY,
// BAKI_LIVE_GITHUB_MODELS_KEY, BAKI_LIVE_AZURE_FOUNDRY_KEY). The value is what the keystore would hold: the API key,
// the PAT or OAuth token (GitHub Models, Copilot), or the JSON credential document (Azure AI Foundry, local). An unset
// variable skips that provider's tests (the per-test t.Skip of internal/storage/database/idle_tx_timeout_test.go), so
// `go test -tags live` without keys passes having called nothing. Every call goes through a LiveBudget: a per-call
// output cap and a per-run token and call ceiling that aborts the run (budgets and pricing are gone from the product,
// ADR 0037, so the cap is in tokens, not dollars). This file reads the environment itself: internal/testutil is
// exempt from the internal/config ratchet (internal/archtest TestArch_EnvReadsOnlyInConfig).

// LiveEnvVar is the variable of a provider's live setting: LiveEnvVar("azure-foundry", "KEY") is
// BAKI_LIVE_AZURE_FOUNDRY_KEY.
func LiveEnvVar(providerID, suffix string) string {
	return "BAKI_LIVE_" + strings.ToUpper(strings.ReplaceAll(providerID, "-", "_")) + "_" + suffix
}

// liveSmokeModels are the models the live tests run on when BAKI_LIVE_<PROVIDER>_MODEL is unset: the cheapest
// catalogue model of each provider that takes tools, so a run stays small. A provider missing here (Azure AI Foundry,
// GLM, local) runs on its own default — for Azure, the credential document's chat deployment.
var liveSmokeModels = map[string]string{
	"claude":        "claude-haiku-4-5",
	"openai":        "gpt-4o-mini",
	"gemini":        "gemini-2.5-flash",
	"xai":           "grok-3-mini",
	"github-models": "gpt-4o-mini",
	"copilot":       "gpt-4o-mini",
}

// LiveProvider is one provider the live tests may call: its spec, the credential from the environment and the model
// they run on.
type LiveProvider struct {
	Spec  ai.ProviderSpec
	Key   string
	Model string
}

// LookupLiveProvider reads a provider's live credential; false when its BAKI_LIVE_<PROVIDER>_KEY is unset or the id
// names no provider.
func LookupLiveProvider(providerID string) (LiveProvider, bool) {
	spec, ok := ai.SpecOf(providerID)
	if !ok {
		return LiveProvider{}, false
	}
	key := strings.TrimSpace(os.Getenv(LiveEnvVar(providerID, "KEY")))
	if key == "" {
		return LiveProvider{}, false
	}
	model := strings.TrimSpace(os.Getenv(LiveEnvVar(providerID, "MODEL")))
	if model == "" {
		model = liveSmokeModels[providerID]
	}
	configureLiveProviders.Do(func() {
		// The desktop profile's outbound policy: the vendors' hosts, a user-named endpoint, and the loopback server a
		// BAKI_LIVE_LOCAL_KEY names (the cloud profile would refuse it).
		ai.ConfigureProviders(ai.ProviderOptions{Egress: egress.AllowLoopback})
	})
	lp := LiveProvider{Spec: spec, Key: key, Model: model}
	if lp.Model == "" {
		lp.Model = lp.defaultModel()
	}
	return lp, true
}

var configureLiveProviders sync.Once

// defaultModel is the provider's own default, else — a local server, whose models are whatever it serves — the first
// chat model it lists.
func (lp LiveProvider) defaultModel() string {
	p := lp.Raw()
	if m := p.DefaultModel(); m != "" {
		return m
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	list, err := p.Models(ctx)
	if err != nil {
		return ""
	}
	for _, m := range list {
		if !m.Embedding && m.ID != "" {
			return m.ID
		}
	}
	return ""
}

// RequireLiveProvider is LookupLiveProvider for a test: it skips the test when the provider's key is unset.
func RequireLiveProvider(t testing.TB, providerID string) LiveProvider {
	t.Helper()
	lp, ok := LookupLiveProvider(providerID)
	if !ok {
		t.Skipf("%s not set — skipping the live %s test", LiveEnvVar(providerID, "KEY"), providerID)
	}
	return lp
}

// Raw builds the provider from the credential with the spec's constructor: no audited wrapper, so nothing records a
// usage row, and no retry or breaker.
func (lp LiveProvider) Raw() ai.Provider {
	return lp.Spec.Ctor(ai.Credential{Key: lp.Key})
}

// KeyLookup is a ProviderFactory key function that answers the live credential for the provider's own keystore row
// and nothing else — no other provider, no device-flow token, no organisation key.
func (lp LiveProvider) KeyLookup() func(scope, key string) (string, error) {
	return func(_, key string) (string, error) {
		if key == lp.Spec.Keys.Primary {
			return lp.Key, nil
		}
		return "", nil
	}
}

// LiveIntEnv reads a positive integer setting of the live run (a cap), def when unset or not a positive integer.
func LiveIntEnv(name string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil && v > 0 {
		return v
	}
	return def
}

// LiveSummaryf appends one line to the run's Markdown summary, the file BAKI_LIVE_SUMMARY names (the workflow copies
// it into the job summary); nothing when it is unset.
func LiveSummaryf(format string, args ...any) {
	path := os.Getenv("BAKI_LIVE_SUMMARY")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- the workflow's own summary file
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintf(f, format+"\n", args...)
}

// ErrLiveBudgetExhausted is a call the LiveBudget refused: the run reached its token or call ceiling.
var ErrLiveBudgetExhausted = errors.New("live budget exhausted")

// LiveBudget is the hard cap of a live run: at most MaxCalls provider calls (Chat, Stream, Embed) and, once the calls
// so far have used MaxTokens (input + output as the providers reported them), no further call at all. Every call's
// MaxTokens is clamped to the per-call cap of the wrapper it goes through.
type LiveBudget struct {
	Name      string
	MaxTokens int
	MaxCalls  int

	mu     sync.Mutex
	tokens int
	calls  int
}

// NewLiveBudget is a budget of maxTokens tokens and maxCalls calls.
func NewLiveBudget(name string, maxTokens, maxCalls int) *LiveBudget {
	return &LiveBudget{Name: name, MaxTokens: maxTokens, MaxCalls: maxCalls}
}

// Used reports the tokens and calls spent so far.
func (b *LiveBudget) Used() (tokens, calls int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tokens, b.calls
}

// admit takes a call slot, or refuses the call once either ceiling is reached.
func (b *LiveBudget) admit() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.calls >= b.MaxCalls || b.tokens >= b.MaxTokens {
		return fmt.Errorf("%w: %s used %d/%d tokens in %d/%d calls", ErrLiveBudgetExhausted, b.Name, b.tokens, b.MaxTokens, b.calls, b.MaxCalls)
	}
	b.calls++
	return nil
}

func (b *LiveBudget) spend(tokens int) {
	if tokens <= 0 {
		return
	}
	b.mu.Lock()
	b.tokens += tokens
	b.mu.Unlock()
}

// Wrap puts p behind the budget, every request's MaxTokens clamped to maxTokensPerCall.
func (b *LiveBudget) Wrap(p ai.Provider, maxTokensPerCall int) ai.Provider {
	return &cappedProvider{Provider: p, budget: b, perCall: maxTokensPerCall}
}

// cappedProvider is a provider behind a LiveBudget.
type cappedProvider struct {
	ai.Provider
	budget  *LiveBudget
	perCall int
}

// clamp caps the request's output at the per-call cap — plus LiveReasoningAllowance, with the effort pinned to "low",
// for a model that thinks (LiveReasoning): its hidden reasoning counts against the same cap as the answer, so a bare
// 64-token cap ends with thoughts only (Gemini's MAX_TOKENS, an ErrOutputLimit with no usage) and the service's own
// 16 384-token reasoning floor (ai.ReasoningOutputFloor) would make the run's ceiling meaningless.
func (c *cappedProvider) clamp(req ai.Request) ai.Request {
	limit := c.perCall
	if LiveReasoning(req.Model) {
		limit += LiveReasoningAllowance
		req.ReasoningEffort = "low"
	}
	if req.MaxTokens <= 0 || req.MaxTokens > limit {
		req.MaxTokens = limit
	}
	return req
}

// LiveReasoningAllowance is what a live call on a reasoning model may spend thinking on top of its per-call cap,
// at "low" effort (Gemini 2.5 then gets its minimum thinking budget; OpenAI and Claude reason briefly).
const LiveReasoningAllowance = 1024

// liveExtraReasoningRe are the thinking models the service's reasoning floor does not list (their reasoning is
// billed as output all the same): xAI's grok-3-mini family takes reasoning_effort and thinks by default.
var liveExtraReasoningRe = regexp.MustCompile(`(^|[^a-z0-9])grok-3-mini`)

// LiveReasoning reports whether model thinks by default, counting its reasoning against the output cap: the
// service's reasoning floor (ai.ReasoningOutputFloor) plus liveExtraReasoningRe.
func LiveReasoning(model string) bool {
	return ai.ReasoningOutputFloor(model) > 0 || liveExtraReasoningRe.MatchString(strings.ToLower(model))
}

// LiveTruncated reports whether a finish reason means the call stopped at its output cap — each wire's own word:
// "length" (OpenAI-compatible, Responses), "max_tokens" and "model_context_window_exceeded" (Claude), "MAX_TOKENS"
// (Gemini), "max_output_tokens" (a Responses incomplete reason).
func LiveTruncated(finish string) bool {
	switch strings.ToLower(strings.TrimSpace(finish)) {
	case "length", "max_tokens", "max_output_tokens", "model_context_window_exceeded":
		return true
	}
	return false
}

// liveURLRe is any URL: a failure message never carries an endpoint (a *url.Error prints the request URL).
var liveURLRe = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>]+`)

// Redact removes credential material and endpoints from text a live test prints (a failure message, a log line):
// every URL, the credential itself and — for a credential document — the values of its secret, id, endpoint and URL
// fields and the hosts of its endpoints. GitHub masks a secret in the console only as a whole, never a substring of
// a JSON document, and never in an artifact.
func (lp LiveProvider) Redact(s string) string {
	secrets := []string{strings.TrimSpace(lp.Key)}
	var doc any
	if json.Unmarshal([]byte(lp.Key), &doc) == nil {
		secrets = append(secrets, credentialValues(doc, "")...)
	}
	s = liveURLRe.ReplaceAllString(s, "<url>")
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, v := range secrets {
		if len(v) >= 4 {
			s = strings.ReplaceAll(s, v, "<redacted>")
		}
	}
	return s
}

// credentialValues lists the values of a credential document's sensitive fields (by name: key, secret, token,
// password, endpoint, url, tenant, client) and the host of each URL among them.
func credentialValues(v any, name string) []string {
	var out []string
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			out = append(out, credentialValues(e, k)...)
		}
	case []any:
		for _, e := range x {
			out = append(out, credentialValues(e, name)...)
		}
	case string:
		n := strings.ToLower(name)
		for _, w := range []string{"key", "secret", "token", "password", "endpoint", "url", "tenant", "client"} {
			if strings.Contains(n, w) {
				out = append(out, x)
				if u, err := url.Parse(x); err == nil && u.Host != "" {
					out = append(out, u.Host, u.Hostname())
				}
				break
			}
		}
	}
	return out
}

func (c *cappedProvider) Chat(ctx context.Context, req ai.Request) (*ai.Response, error) {
	if err := c.budget.admit(); err != nil {
		return nil, err
	}
	resp, err := c.Provider.Chat(ctx, c.clamp(req))
	if resp != nil {
		c.budget.spend(resp.TokensIn + resp.TokensOut)
	}
	return resp, err
}

func (c *cappedProvider) Stream(ctx context.Context, req ai.Request, onChunk func(ai.Chunk)) error {
	if err := c.budget.admit(); err != nil {
		return err
	}
	// Usage arrives on the terminal chunk (or the error chunk of a failed turn); the largest counts seen are spent.
	var in, out int
	err := c.Provider.Stream(ctx, c.clamp(req), func(ch ai.Chunk) {
		in, out = max(in, ch.TokensIn), max(out, ch.TokensOut)
		onChunk(ch)
	})
	c.budget.spend(in + out)
	return err
}

func (c *cappedProvider) Embed(ctx context.Context, text []string) ([][]float32, error) {
	if err := c.budget.admit(); err != nil {
		return nil, err
	}
	n := 0
	for _, s := range text {
		n += c.EstimateTokens(s)
	}
	c.budget.spend(n)
	return c.Provider.Embed(ctx, text)
}
