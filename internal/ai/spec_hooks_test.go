package ai

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/egress"
)

// stubListing is a ListingHook that answers constants.
type stubListing struct{ configured bool }

func (s stubListing) Configured(context.Context, string) (bool, error)    { return s.configured, nil }
func (s stubListing) OrgConfigured(context.Context, string) (bool, error) { return false, nil }
func (s stubListing) Picker(context.Context, string, string) (ProviderListing, error) {
	return ProviderListing{Configured: s.configured}, nil
}

// TestInstallListing_SetsTheSpecHook: a family's registry is built after init, so its hook is installed at
// runtime; SpecOf and Specs then carry it, and nil uninstalls. A test override of the same id (RegisterProvider)
// gets the hook too, so swapping a builtin's constructor does not lose the family.
func TestInstallListing_SetsTheSpecHook(t *testing.T) {
	const id = "hook-stub"
	RegisterProvider(id, func(string) Provider { return &auditStub{} })
	if !InstallListing(id, stubListing{configured: true}) {
		t.Fatal("InstallListing on a registered override reported unknown")
	}
	t.Cleanup(func() { InstallListing(id, nil) })
	spec, ok := SpecOf(id)
	if !ok || spec.Listing == nil {
		t.Fatalf("SpecOf(%s).Listing = %v after InstallListing", id, spec.Listing)
	}
	if got, _ := spec.Listing.Configured(context.Background(), "u1"); !got {
		t.Error("the installed hook is not the one SpecOf answers")
	}
	InstallListing(id, nil)
	if spec, _ := SpecOf(id); spec.Listing != nil {
		t.Error("InstallListing(id, nil) did not uninstall")
	}
	if InstallListing("no-such-provider", stubListing{}) {
		t.Error("InstallListing on an unknown id reported ok")
	}
}

// TestUnknownProvider_SentinelKeepsText: the factory's refusal of an id the registry does not know is
// ErrUnknownProvider (chat answers it with PROVIDER_UNKNOWN) and its text is what it always was, so nothing
// matching on the string moves.
func TestUnknownProvider_SentinelKeepsText(t *testing.T) {
	f := NewProviderFactory(func(string, string) (string, error) { return "k", nil }, nil, nil, nil)
	_, err := f.For(context.Background(), "u1", "nope")
	if !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("For(nope): err = %v, want ErrUnknownProvider", err)
	}
	if err.Error() != "unknown provider: nope" {
		t.Errorf("err text = %q, want the historical text", err.Error())
	}
	if _, err := f.ForOrg(context.Background(), "u1", "org-1", "nope"); !errors.Is(err, ErrUnknownProvider) {
		t.Errorf("ForOrg(nope): err = %v", err)
	}
}

// TestConfigureProviders_AppliesEachSpecsEgress: one boot call applies the deployment's egress policy by each
// spec's EgressPolicy — the loopback provider's gate follows it; a fixed-host provider gets nothing to apply.
func TestConfigureProviders_AppliesEachSpecsEgress(t *testing.T) {
	t.Cleanup(func() { ConfigureLocalModels(LocalModelsOptions{}) })
	ConfigureProviders(ProviderOptions{Egress: egress.BlockInternal})
	if LocalModelsAvailable() {
		t.Error("cloud egress (BlockInternal) must leave the loopback provider unavailable")
	}
	ConfigureProviders(ProviderOptions{Egress: egress.AllowLoopback})
	if !LocalModelsAvailable() {
		t.Error("desktop egress (AllowLoopback) must make the loopback provider available")
	}
	for _, spec := range Specs() {
		if spec.Egress.Kind == EgressLoopback && spec.Modes[0] == "cloud" {
			t.Errorf("%s: a loopback provider offered in the cloud profile", spec.ID)
		}
	}
}
