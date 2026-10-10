package di

import (
	"testing"

	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/egress"
)

// TestAzureEndpointPolicy pins the egress policy of the user-supplied Azure AI Foundry endpoints by deployment
// profile: only the multi-tenant cloud server sends them through the private-address guard; the desktop and the
// local development profile (a database, accounts or not) reach their owner's own network.
func TestAzureEndpointPolicy(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps capability.Set
		want egress.Policy
	}{
		{"desktop", capability.NewSet(capability.Desktop), egress.AllowLoopback},
		{"local with a database", capability.NewSet(capability.LocalDB, capability.ServerStore), egress.AllowLoopback},
		{"local with a database and accounts", capability.NewSet(capability.LocalDB, capability.ServerStore, capability.Accounts), egress.AllowLoopback},
		{"cloud", capability.NewSet(capability.Cloud, capability.ServerStore, capability.Accounts, capability.RowLevelSecurity), egress.BlockInternal},
	} {
		if got := azureEndpointPolicy(tc.caps); got != tc.want {
			t.Errorf("%s: policy = %v, want %v", tc.name, got, tc.want)
		}
	}
}
