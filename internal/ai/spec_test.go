package ai

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"pad-analyzer/internal/capability"
)

var updateProviderSpecs = flag.Bool("update-provider-specs", false, "rewrite testdata/providers.golden from the registered provider specs")

// TestProviderSpecs_Complete checks every registered spec against the provider it builds and the catalogue, and
// that each derived list is exactly the spec-derived set — the registry is the one place a provider is
// described, so a spec that disagrees with its constructor would make the listing lie.
func TestProviderSpecs_Complete(t *testing.T) {
	specs := Specs()
	if len(specs) == 0 {
		t.Fatal("no provider specs registered")
	}
	seenOrder := map[int]string{}
	for _, spec := range specs {
		t.Run(spec.ID, func(t *testing.T) {
			if spec.Ctor == nil {
				t.Fatal("Ctor is nil")
			}
			if spec.Name == "" || spec.Order <= 0 {
				t.Errorf("Name %q / Order %d must be set", spec.Name, spec.Order)
			}
			if prev, dup := seenOrder[spec.Order]; dup {
				t.Errorf("Order %d is also %s's", spec.Order, prev)
			}
			seenOrder[spec.Order] = spec.ID

			p := spec.Ctor(Credential{})
			if p.ID() != spec.ID {
				t.Errorf("Ctor(...).ID() = %q, want %q", p.ID(), spec.ID)
			}
			if p.Name() != spec.Name {
				t.Errorf("Ctor(...).Name() = %q, want %q", p.Name(), spec.Name)
			}
			if p.DefaultModel() != spec.DefaultModel && !spec.Caps.Has(CapModelAliases) {
				t.Errorf("Ctor(...).DefaultModel() = %q, spec says %q", p.DefaultModel(), spec.DefaultModel)
			}
			if p.PricePerMillionTokens() != specDefaultPricing(spec) {
				t.Errorf("Ctor(...).PricePerMillionTokens() = %+v, spec derives %+v", p.PricePerMillionTokens(), specDefaultPricing(spec))
			}

			if spec.Catalog != "" {
				if len(modelCatalog[spec.Catalog]) == 0 {
					t.Errorf("Catalog %q has no modelCatalog rows", spec.Catalog)
				}
				if !spec.Caps.Has(CapModelAliases) && !catalogHas(spec.Catalog, spec.DefaultModel) {
					t.Errorf("DefaultModel %q is not a %q catalogue row", spec.DefaultModel, spec.Catalog)
				}
			} else if spec.Pricing == PricingCatalog {
				t.Error("Pricing catalog without a Catalog")
			}
			switch spec.Pricing {
			case PricingCatalog:
				if specDefaultPricing(spec) == (Pricing{}) {
					t.Error("Pricing catalog derives a zero fallback price")
				}
			case PricingFree, PricingUnpriced:
				if specDefaultPricing(spec) != (Pricing{}) {
					t.Errorf("Pricing %s must derive Pricing{}", spec.Pricing)
				}
			default:
				t.Errorf("Pricing %q is not a PricingMode", spec.Pricing)
			}
			if (spec.Pricing == PricingUnpriced) != spec.Caps.Has(CapModelAliases) {
				t.Errorf("Pricing unpriced ⇔ CapModelAliases (pricing %s, aliases %v)", spec.Pricing, spec.Caps.Has(CapModelAliases))
			}

			keyed := spec.Ctor(Credential{Key: "k"})
			_, identifies := keyed.(EmbeddingIdentifier)
			if spec.Caps.Has(CapEmbeddings) && !identifies {
				t.Error("CapEmbeddings but the provider does not implement EmbeddingIdentifier")
			}
			if !spec.Caps.Has(CapEmbeddings) && EmbeddingIdentity(keyed) != "" {
				t.Errorf("no CapEmbeddings but the provider names an embedding identity %q", EmbeddingIdentity(keyed))
			}
			if spec.Caps.Has(CapTools) != p.SupportsTools() {
				t.Errorf("CapTools %v but SupportsTools() %v", spec.Caps.Has(CapTools), p.SupportsTools())
			}
			if !spec.Caps.Has(CapChat | CapStream) {
				t.Error("every provider chats and streams")
			}

			switch spec.AuthType {
			case AuthAPIKey, AuthOAuth:
			default:
				t.Errorf("AuthType %q is not api_key or oauth", spec.AuthType)
			}
			if spec.AuthType == AuthOAuth && spec.Credential.Kind != CredentialOAuthDevice {
				t.Errorf("AuthType oauth needs Credential.Kind oauth_device, got %q", spec.Credential.Kind)
			}
			switch spec.Credential.Kind {
			case CredentialAPIKey, CredentialOAuthDevice, CredentialDocument, CredentialNone:
			default:
				t.Errorf("Credential.Kind %q is not a CredentialKind", spec.Credential.Kind)
			}
			for _, f := range spec.Credential.Fields {
				if f.Name == "" || f.Label == "" {
					t.Errorf("credential field %+v needs a name and a label", f)
				}
				switch f.Type {
				case FieldText, FieldSecret, FieldURL, FieldSelect, FieldNumber, FieldJSON:
				default:
					t.Errorf("credential field %q: type %q is not a FieldType", f.Name, f.Type)
				}
				if (f.Type == FieldSelect) != (len(f.Options) > 0) {
					t.Errorf("credential field %q: options go with type select only", f.Name)
				}
			}
			if spec.Keys.Primary == "" {
				t.Error("Keys.Primary is empty")
			}
			if (spec.Keys.OAuthToken != "") != (spec.Resolver != nil) {
				t.Error("an OAuth token key goes with a Resolver that reads it")
			}
			if len(spec.Modes) == 0 {
				t.Error("Modes is empty")
			}
			for _, m := range spec.Modes {
				switch m {
				case capability.Desktop, capability.LocalDB, capability.Cloud:
				default:
					t.Errorf("Modes names %q, not a capability.Profile", m)
				}
			}
			switch spec.Egress.Kind {
			case EgressFixedHost, EgressUserEndpoint, EgressLoopback:
			default:
				t.Errorf("Egress.Kind %q is not an EgressKind", spec.Egress.Kind)
			}
			if spec.Egress.Kind == EgressLoopback && slices.Contains(spec.Modes, capability.Cloud) {
				t.Error("a loopback provider cannot run in the cloud profile")
			}
			if spec.Egress.Kind != EgressFixedHost && !spec.Egress.DialGuard {
				t.Error("a user-chosen endpoint needs the dial guard")
			}
			switch spec.Wire {
			case WireAnthropic, WireOpenAI, WireGemini, WireResponses:
			default:
				t.Errorf("Wire %q is not a WireFamily", spec.Wire)
			}
		})
	}

	t.Run("derived lists", func(t *testing.T) {
		var ids, names, auths, embedders []string
		for _, spec := range specs {
			ids = append(ids, spec.ID)
			names = append(names, spec.Name)
			auths = append(auths, string(spec.AuthType))
			if spec.Caps.Has(CapEmbeddings) {
				embedders = append(embedders, spec.ID)
			}
			if SupportsEmbeddings(spec.ID) != spec.Caps.Has(CapEmbeddings) {
				t.Errorf("SupportsEmbeddings(%q) = %v, spec says %v", spec.ID, SupportsEmbeddings(spec.ID), spec.Caps.Has(CapEmbeddings))
			}
			if UsesModelAliases(spec.ID) != spec.Caps.Has(CapModelAliases) {
				t.Errorf("UsesModelAliases(%q) = %v, spec says %v", spec.ID, UsesModelAliases(spec.ID), spec.Caps.Has(CapModelAliases))
			}
			if !KnownProvider(spec.ID) {
				t.Errorf("KnownProvider(%q) = false", spec.ID)
			}
			if storageKey(spec.ID) != spec.Keys.Primary {
				t.Errorf("storageKey(%q) = %q, spec says %q", spec.ID, storageKey(spec.ID), spec.Keys.Primary)
			}
			if GetMetadataProvider(spec.ID) == nil {
				t.Errorf("GetMetadataProvider(%q) = nil", spec.ID)
			}
			if got, ok := SpecOf(spec.ID); !ok || got.ID != spec.ID {
				t.Errorf("SpecOf(%q) = %+v, %v", spec.ID, got, ok)
			}
		}
		var gotIDs, gotNames, gotAuths []string
		for _, meta := range AvailableProviders() {
			gotIDs = append(gotIDs, meta.ID)
			gotNames = append(gotNames, meta.Name)
			gotAuths = append(gotAuths, meta.AuthType)
		}
		if !slices.Equal(gotIDs, ids) || !slices.Equal(gotNames, names) || !slices.Equal(gotAuths, auths) {
			t.Errorf("AvailableProviders() = %v / %v / %v, specs say %v / %v / %v", gotIDs, gotNames, gotAuths, ids, names, auths)
		}
		if got := EmbeddingFallbackOrder(); !slices.Equal(got, embedders) {
			t.Errorf("EmbeddingFallbackOrder() = %v, specs say %v", got, embedders)
		}
		for _, id := range []string{"", "nope", "Claude", "claude "} {
			if KnownProvider(id) {
				t.Errorf("KnownProvider(%q) = true", id)
			}
			if SupportsEmbeddings(id) || UsesModelAliases(id) {
				t.Errorf("%q: SupportsEmbeddings/UsesModelAliases must be false for an unknown id", id)
			}
			if _, ok := SpecOf(id); ok {
				t.Errorf("SpecOf(%q) found a spec", id)
			}
			if GetMetadataProvider(id) != nil {
				t.Errorf("GetMetadataProvider(%q) is not nil", id)
			}
		}
	})

	t.Run("copies", func(t *testing.T) {
		// Specs() and SpecOf hand out copies: a caller cannot edit the registry through them.
		first := Specs()[0]
		first.Modes[0] = "edited"
		first.Credential.Fields = append(first.Credential.Fields, CredentialField{Name: "x"})
		if again := Specs()[0]; again.Modes[0] == "edited" || len(again.Credential.Fields) != len(first.Credential.Fields)-1 {
			t.Error("Specs() returned a view onto the registry")
		}
	})
}

// TestProviderSpecs_Golden pins the provider table (id, order, auth, caps, modes, keys, egress, pricing, credential
// kind and field names) in testdata/providers.golden: a change in what a provider is must show in review.
// Refresh with -update-provider-specs.
func TestProviderSpecs_Golden(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("# Provider specs (go test ./internal/ai -run TestProviderSpecs_Golden -update-provider-specs)\n")
	for _, s := range Specs() {
		var fields []string
		for _, f := range s.Credential.Fields {
			fields = append(fields, f.Name)
		}
		var modes []string
		for _, m := range s.Modes {
			modes = append(modes, string(m))
		}
		keys := s.Keys.Primary
		if s.Keys.OAuthToken != "" {
			keys += "," + s.Keys.OAuthToken
		}
		if s.Keys.Family {
			keys += "," + s.Keys.Primary + "/*"
		}
		fmt.Fprintf(&b, "%s order=%d name=%q auth=%s wire=%s caps=%s modes=%s keys=%s egress=%s pricing=%s catalog=%q default=%q credential=%s fields=%s resolver=%v color=%s icon=%s\n",
			s.ID, s.Order, s.Name, s.AuthType, s.Wire, strings.Join(s.Caps.Names(), ","), strings.Join(modes, ","), keys,
			s.Egress.Kind, s.Pricing, s.Catalog, s.DefaultModel, s.Credential.Kind, strings.Join(fields, ","), s.Resolver != nil,
			s.Color, s.Icon)
	}
	path := filepath.Join("testdata", "providers.golden")
	if *updateProviderSpecs {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update-provider-specs)", err)
	}
	if got := b.String(); got != strings.ReplaceAll(string(want), "\r\n", "\n") {
		t.Errorf("provider specs differ from %s (refresh with -update-provider-specs and review the diff):\n--- want\n%s\n--- got\n%s", path, want, got)
	}
}

// TestProviderSpecs_CopilotResolverAndOrgRule: the Resolver and !CapOrgShared derivations keep Copilot's
// dual-auth and never-shared behaviour: the OAuth token (with the exchanger) first, else the PAT; an org
// builder uses the member's own credential and the org-only builder reports not configured.
func TestProviderSpecs_CopilotResolverAndOrgRule(t *testing.T) {
	keys := map[string]string{"u1|copilot-oauth-token": "gho", "u1|copilot": "pat", "u2|copilot": "pat2"}
	f := NewProviderFactory(func(scope, k string) (string, error) { return keys[scope+"|"+k], nil }, NewCopilotAuth(), nil, nil)
	f.SetOrgKeyLookup(func(orgID, k string) (string, error) { return "org-" + k, nil })

	p, err := f.ForOrg(context.Background(), "u1", "org-1", "copilot")
	if err != nil {
		t.Fatalf("ForOrg: %v", err)
	}
	if UsedOrgCredential(p) {
		t.Error("ForOrg(copilot) ran on the org's credential; Copilot is never shared")
	}
	if ap := p.(*auditedProvider); ap.memoScope != "u1" {
		t.Errorf("memoScope = %q, want the member's", ap.memoScope)
	}
	if _, err := f.ForOrgCredential(context.Background(), "u1", "org-1", "copilot"); !errorsIsKeyNotConfigured(err) {
		t.Errorf("ForOrgCredential(copilot) = %v, want ErrKeyNotConfigured", err)
	}
	if _, err := f.ForEmbeddingOrg(context.Background(), "u2", "org-1", "copilot", "ignored"); err != nil {
		t.Errorf("ForEmbeddingOrg(copilot) on a PAT: %v", err)
	}
	if _, err := f.For(context.Background(), "u3", "copilot"); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("For(copilot) with nothing stored = %v, want a not-configured error", err)
	}
}

func errorsIsKeyNotConfigured(err error) bool {
	return err != nil && strings.Contains(err.Error(), ErrKeyNotConfigured.Error())
}

func catalogHas(catalog, model string) bool {
	for _, m := range modelCatalog[catalog] {
		if m.ID == model {
			return true
		}
	}
	return false
}
