package scrubber

import (
	"strings"
	"testing"

	"pad-core/models"
)

// Token fixtures are assembled at run time: a contiguous token-shaped string in the source is what credential
// scanners match (see fakeStripeLiveKey), and the values are identical once built.
var (
	fakeOpenAIKey    = "sk-" + "proj-" + strings.Repeat("Ab3dEf6h", 6)
	fakeAnthropicKey = "sk-" + "ant-api03-" + strings.Repeat("Zy9xWv8u", 6)
	fakeGitHubPAT    = "github_" + "pat_" + strings.Repeat("A1b2C3d4E5", 3) + "_" + strings.Repeat("f6G7h8I9j0", 6)
	fakeGitHubOAuth  = "gh" + "o_" + strings.Repeat("Kl1Mn2Op3Q", 3) + "r4St5U"
	fakeGitLabPAT    = "glpat" + "-" + strings.Repeat("Xy7Zw6Vu5T", 2)
	fakeStorageKey   = strings.Repeat("Q2xvdWRTdG9yYWdlS2V5", 4) + "AbCd=="

	fakeGitLabUnderscore  = "glpat" + "_" + strings.Repeat("Rq4Sp3To2U", 2)
	fakeStripePublishable = "pk_" + "live_" + strings.Repeat("ijklmnop", 3)
	fakeStripeRestricted  = "rk_" + "live_" + strings.Repeat("qrstuvwx", 3)
	fakeStripeWebhook     = "whsec" + "_" + strings.Repeat("Wh5Ec6Rt7Y", 3)
)

// fakeGitHubClassic builds a classic GitHub token of the given kind letter (u, s, r, …): gh<kind>_ + 36.
func fakeGitHubClassic(kind string) string {
	return "gh" + kind + "_" + strings.Repeat("Cl4Ss1Ct0K", 3) + "e5N6m7"
}

// TestScrubText_CredentialShapes pins R-security-c11: the free-text pass is the only pass for messages,
// history, tool results and stream output, so every common credential shape must be masked there.
func TestScrubText_CredentialShapes(t *testing.T) {
	cases := []struct {
		name, in, secret string
	}{
		{"json quoted password with a symbol", `{"password": "Winter#2024"}`, "Winter#2024"},
		{"json quoted api key, no spaces", `{"api_key":"q8Wz3kLp0mNs7Rt2"}`, "q8Wz3kLp0mNs7Rt2"},
		{"json quoted password with spaces", `{"password": "correct horse battery"}`, "horse battery"},
		{"yaml quoted password with spaces", `password: "correct horse battery"`, "horse battery"},
		{"yaml single-quoted passphrase", `password: 'supersecret pass'`, "supersecret pass"},
		{"single-quoted dict secret", `{'client_secret': 'Zx9!Yw8@Vu7'}`, "Zx9!Yw8@Vu7"},
		{"yaml password with a symbol", "password: Winter#2024", "Winter#2024"},
		{"basic auth header", "Authorization: Basic dXNlcjpwYXNzd29yZA==", "dXNlcjpwYXNzd29yZA=="},
		{"apim subscription key header", "Ocp-Apim-Subscription-Key: 0123456789abcdef0123456789abcdef", "0123456789abcdef0123456789abcdef"},
		{"functions key header", "x-functions-key: 0123456789abcdef0123456789abcdef", "0123456789abcdef0123456789abcdef"},
		{"storage account key", "DefaultEndpointsProtocol=https;AccountName=acct;AccountKey=" + fakeStorageKey + ";EndpointSuffix=core.windows.net", fakeStorageKey},
		{"openai sk- key", "key " + fakeOpenAIKey + " done", fakeOpenAIKey},
		{"anthropic sk-ant key", "key " + fakeAnthropicKey + " done", fakeAnthropicKey},
		{"github fine-grained pat", "tok " + fakeGitHubPAT + " done", fakeGitHubPAT},
		{"github oauth token", "tok " + fakeGitHubOAuth + " done", fakeGitHubOAuth},
		{"gitlab pat, real dash form", "tok " + fakeGitLabPAT + " done", fakeGitLabPAT},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ScrubText(c.in); strings.Contains(got, c.secret) {
				t.Errorf("secret survived ScrubText:\n  in:  %q\n  out: %q", c.in, got)
			}
		})
	}

	// The visible parts stay: masking the key names or the account would cost the model its analysis.
	exact := map[string]string{
		`{"password": "Winter#2024"}`:                       `{"password": "[REDACTED]"}`,
		`password: "correct horse battery"`:                 `password: "[REDACTED]"`,
		"password: Winter#2024":                             "password: [REDACTED]",
		"Authorization: Basic dXNlcjpwYXNzd29yZA==":         "Authorization: Basic [REDACTED]",
		"x-functions-key: 0123456789abcdef0123456789abcdef": "x-functions-key: [REDACTED]",
	}
	for in, want := range exact {
		if got := ScrubText(in); got != want {
			t.Errorf("ScrubText(%q) = %q, want %q", in, got, want)
		}
	}
	conn := ScrubText("DefaultEndpointsProtocol=https;AccountName=acct;AccountKey=" + fakeStorageKey + ";EndpointSuffix=core.windows.net")
	if !strings.Contains(conn, "AccountName=acct") || !strings.Contains(conn, "EndpointSuffix=core.windows.net") {
		t.Errorf("the connection string's non-secret parts were masked too: %q", conn)
	}
}

// TestScrubText_ProseAfterKeywordIsKept: the assistant explains credentials constantly, so a keyword followed
// by a PAD variable reference, Markdown, a parenthesis, a template, a URL or a word in another language is
// prose, not a secret — masking it puts [REDACTED] into the answer the user reads and the history stores.
func TestScrubText_ProseAfterKeywordIsKept(t *testing.T) {
	for _, in := range []string{
		"Use the variable password: %Password% in the action",
		"Password: `%Password%` is read from the vault",
		"The token: (expired) must be refreshed",
		"Secret: **never** share it",
		"token: ${env:TOKEN} is injected",
		"The password: https://vault.example.com/secrets/db",
		"password: čitljiva lozinka",
		`password: "%Password%"`,
		`password: "${DB_PASSWORD}"`,
		"Authorization: Basic authentication is used",
	} {
		if got := ScrubText(in); got != in {
			t.Errorf("prose was masked:\n  in:  %q\n  out: %q", in, got)
		}
	}
}

// TestScrubText_URLCredentials pins R-security-c12: credentials embedded in a URL — userinfo and credential
// query parameters — are masked, while the scheme, host, path and every other parameter stay readable.
func TestScrubText_URLCredentials(t *testing.T) {
	cases := map[string]string{
		"https://svc:Pr0d!Pass@ftp.example.com/in":                                                                                 "https://svc:[REDACTED]@ftp.example.com/in",
		"ftp://deploy:s3cr3t-Value@files.example.com:21/out/report.csv":                                                            "ftp://deploy:[REDACTED]@files.example.com:21/out/report.csv",
		"https://acct.blob.core.windows.net/c1/data.csv?sv=2022-11-02&sp=r&se=2026-01-01T00%3A00%3A00Z&sig=Ab12Cd34Ef56%2BGh78%3D": "https://acct.blob.core.windows.net/c1/data.csv?sv=2022-11-02&sp=r&se=2026-01-01T00%3A00%3A00Z&sig=[REDACTED]",
		"https://fn.azurewebsites.net/api/Run?code=AbCdEf123456789xyz%3D%3D&name=x":                                                "https://fn.azurewebsites.net/api/Run?code=[REDACTED]&name=x",
		"https://bucket.s3.amazonaws.com/k.csv?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=0a1b2c3d4e5f60718293a4b5c6d7e8f9":  "https://bucket.s3.amazonaws.com/k.csv?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=[REDACTED]",
		"https://api.example.com/v1/items?subscription-key=0123456789abcdef&page=2":                                                "https://api.example.com/v1/items?subscription-key=[REDACTED]&page=2",
		"https://x.example.com/cb?access_token=AbCdEfGh12345678&state=xyz&page=2":                                                  "https://x.example.com/cb?access_token=[REDACTED]&state=xyz&page=2",
		// A SAS inside a storage connection string: the signature ends at the next ';' segment.
		"BlobEndpoint=https://a.blob.core.windows.net/;SharedAccessSignature=sv=2022&sr=c&sig=AbC123XyZ%3D;EndpointSuffix=core.windows.net": "BlobEndpoint=https://a.blob.core.windows.net/;SharedAccessSignature=sv=2022&sr=c&sig=[REDACTED];EndpointSuffix=core.windows.net",
	}
	for in, want := range cases {
		if got := ScrubText(in); got != want {
			t.Errorf("ScrubText(%q)\n  got  %q\n  want %q", in, got, want)
		}
	}

	// Credential-free URLs are untouched, ports included. A '@' in the query or after the URL is not userinfo:
	// RFC 3986 userinfo cannot contain '?' or '#'.
	for _, in := range []string{
		"https://api.example.com/v1",
		"https://example.com/login?next=/home&page=2",
		"ftp://ftp.example.com:21/in",
		"http://localhost:8080/api/health",
		"https://login.example.com:8443?login_hint=user@contoso.com",
		"https://api.example.com:443#contact@example.com",
	} {
		if got := ScrubText(in); got != in {
			t.Errorf("credential-free URL changed: %q -> %q", in, got)
		}
	}
}

// TestScrubDocument_URLPropertyCredentials: a WebAutomation Url property is not a credential-named field and
// is skipped by the entropy fallback (it contains "://"), so only the URL rules stand between its embedded
// credentials and the provider.
func TestScrubDocument_URLPropertyCredentials(t *testing.T) {
	doc := &models.FlowDocument{Subflows: []models.Subflow{{ID: "sf", Name: "Main", Blocks: []models.Block{{
		ID: "b1", RawType: "WebAutomation.LaunchEdge",
		Properties: map[string]string{
			"Url":    "https://svc:Pr0d!Pass@portal.example.com/login",
			"SasUrl": "https://acct.blob.core.windows.net/c1/f.csv?sv=2022-11-02&sig=Ab12Cd34Ef56%2BGh78%3D",
		},
	}}}}}
	doc.RebuildIndexes()

	scrubbed, err := ScrubDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	props := scrubbed.Subflows[0].Blocks[0].Properties
	if got := props["Url"]; got != "https://svc:[REDACTED]@portal.example.com/login" {
		t.Errorf("Url property: got %q", got)
	}
	if got := props["SasUrl"]; strings.Contains(got, "Ab12Cd34Ef56") || !strings.Contains(got, "sv=2022-11-02") {
		t.Errorf("SasUrl property: got %q", got)
	}
}
