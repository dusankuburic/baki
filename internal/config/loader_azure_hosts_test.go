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
		// Globs may not span hosts other parties can create: public suffixes
		// (ICANN and private PSL sections) and multi-tenant Azure / hosting
		// zones — at, above or inside them. Exact hosts there stay allowed.
		{"rejects two-label public suffix glob", []string{"*.co.uk"}, "public suffix"},
		{"rejects com.au glob", []string{"*.com.au"}, "public suffix"},
		{"rejects apim glob", []string{"*.azure-api.net"}, "multi-tenant zone"},
		{"rejects web apps glob", []string{"*.azurewebsites.net"}, "azurewebsites.net"},
		{"rejects traffic manager glob", []string{"*.trafficmanager.net"}, "trafficmanager.net"},
		{"rejects vm dns label glob", []string{"*.cloudapp.azure.com"}, "cloudapp.azure.com"},
		{"rejects regional vm glob", []string{"*.westeurope.cloudapp.azure.com"}, "cloudapp.azure.com"},
		{"rejects classic cloud service glob", []string{"*.cloudapp.net"}, "cloudapp.net"},
		{"rejects front door glob", []string{"*.azurefd.net"}, "azurefd.net"},
		{"rejects front door endpoint glob", []string{"*.z01.azurefd.net"}, "azurefd.net"},
		{"rejects cdn glob", []string{"*.azureedge.net"}, "azureedge.net"},
		{"rejects blob glob", []string{"*.blob.core.windows.net"}, "blob.core.windows.net"},
		{"rejects parent of a zone", []string{"*.core.windows.net"}, "blob.core.windows.net"},
		{"rejects azure.com glob", []string{"*.azure.com"}, "cloudapp.azure.com"},
		{"rejects apim regional glob", []string{"*.regional.azure-api.net"}, "azure-api.net"},
		{"exact apim host ok", []string{"contoso-ai.azure-api.net"}, ""},
		{"exact web app host ok", []string{"contoso-ai.azurewebsites.net"}, ""},
		{"exact front door host ok", []string{"contoso-ai-abc123.z01.azurefd.net"}, ""},
		{"exact blob host ok", []string{"contoso.blob.core.windows.net"}, ""},
		{"own domain glob ok", []string{"*.ai.contoso.co.uk"}, ""},
		{"microsoft suffix glob ok", []string{"*.openai.azure.com"}, ""},
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
