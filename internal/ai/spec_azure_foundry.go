package ai

// azureFoundrySpec: Azure AI Foundry, a family of the user's own resources described by a JSON document
// (AzureFoundryConfig; the registry adds "azure-foundry/<id>" rows per resource) and reached through the
// model registry's Listing hook once installed. Model ids are deployment aliases, so the provider is unpriced
// and its default is whatever the document (or the registry) designates — "gpt-4.1" only before one exists.
var azureFoundrySpec = ProviderSpec{
	ID:       azureFoundryProviderID,
	Name:     "Azure AI Foundry",
	AuthType: AuthAPIKey,
	Caps:     CapChat | CapStream | CapEmbeddings | CapTools | CapReasoning | CapModelAliases | CapLiveListing | CapOrgShared,
	Modes:    allProfiles,
	Egress:   EgressPolicy{Kind: EgressUserEndpoint, DialGuard: true},
	Credential: CredentialSchema{
		Kind: CredentialDocument,
		Fields: []CredentialField{
			{Name: "endpoint", Label: "Resource endpoint", Type: FieldURL, Required: true, Placeholder: "https://my-resource.services.ai.azure.com"},
			{Name: "authMode", Label: "Authentication", Type: FieldSelect, Required: true, Options: []string{AzureAuthModeAPIKey, AzureAuthModeEntra, AzureAuthModeEntraCLI}},
			{Name: "key", Label: "API key", Type: FieldSecret},
			{Name: "tenantId", Label: "Tenant ID", Type: FieldText},
			{Name: "clientId", Label: "Client ID", Type: FieldText},
			{Name: "clientSecret", Label: "Client secret", Type: FieldSecret},
			{Name: "projectEndpoint", Label: "Project endpoint", Type: FieldURL},
		},
		HelpURL: "https://ai.azure.com/",
	},
	Keys:         KeySchema{Primary: azureFoundryProviderID, Family: true},
	Catalog:      azureFoundryProviderID,
	DefaultModel: "gpt-4.1",
	Pricing:      PricingUnpriced,
	Wire:         WireOpenAI,
	Ctor:         func(c Credential) Provider { return NewAzureFoundryProvider(c.Key) },
	Color:        "#0078d4",
	Icon:         "cloud",
	Order:        80,
}

func init() { mustRegisterSpec(azureFoundrySpec) }
