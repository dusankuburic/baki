package config

import (
	"strings"
	"testing"
)

// L-misc-6: PAD_AZURE_FOUNDRY_ALLOWED_HOSTS entries are validated at boot —
// a URL-shaped entry silently never matched (every save failed with
// HOST_NOT_ALLOWED), and "*", "*." or a TLD-only glob silently widened the
// allow-list.
func TestValidate_AzureFoundryAllowedHosts(t *testing.T) {
	cases := []struct {
		name    string
		entries []string
		wantErr string
	}{
		{"empty list ok", nil, ""},
		{"bare host ok", []string{"apim.contoso.com"}, ""},
		{"suffix glob ok", []string{"*.privatelink.openai.azure.com"}, ""},
		{"mixed ok", []string{"apim.contoso.com", "*.privatelink.openai.azure.com"}, ""},
		{"upper case ok", []string{"APIM.Contoso.COM"}, ""},
		{"blank entry skipped", []string{" ", "apim.contoso.com"}, ""},
		{"rejects url", []string{"https://apim.contoso.com"}, "no scheme, port, path"},
		{"rejects port", []string{"apim.contoso.com:443"}, "no scheme, port, path"},
		{"rejects path", []string{"apim.contoso.com/openai"}, "no scheme, port, path"},
		{"rejects bare wildcard", []string{"*"}, "wildcard is only allowed"},
		{"rejects mid wildcard", []string{"apim.*.com"}, "wildcard is only allowed"},
		{"rejects dot-only glob", []string{"*."}, "at least two DNS labels"},
		{"rejects tld glob", []string{"*.com"}, "at least two DNS labels"},
		{"rejects single label", []string{"localhost"}, "at least two DNS labels"},
		{"rejects trailing dot", []string{"apim.contoso.com."}, "at least two DNS labels"},
		{"rejects ip literal", []string{"10.0.0.4"}, "IP literals"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := cloudCfg()
			cfg.Runtime.AzureFoundryAllowedHosts = tc.entries
			err := Validate(cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("entries=%v: unexpected error: %v", tc.entries, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "PAD_AZURE_FOUNDRY_ALLOWED_HOSTS") {
				t.Errorf("entries=%v: error = %v, want one naming the variable and %q", tc.entries, err, tc.wantErr)
			}
		})
	}
}

// L-misc-6: the entries are lower-cased when read from the environment, so
// the consumer's exact/suffix compare does not depend on the operator's
// casing.
func TestApplyEnvVars_AzureFoundryAllowedHostsLowerCased(t *testing.T) {
	t.Setenv("PAD_AZURE_FOUNDRY_ALLOWED_HOSTS", " APIM.Contoso.com , *.PrivateLink.OpenAI.Azure.com ")
	cfg := Default()
	if err := applyEnvVars(cfg); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(cfg.Runtime.AzureFoundryAllowedHosts, ",")
	if got != "apim.contoso.com,*.privatelink.openai.azure.com" {
		t.Errorf("hosts = %q", got)
	}
}
