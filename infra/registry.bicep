// Azure Container Registry for the baki backend image.
//
// Deploy this FIRST, once — before the image is built and before
// infra/main.bicep runs, since the Container App pulls an image that must
// already exist:
//
//   az deployment group create -g rg-baki-prod -f infra/registry.bicep \
//     --parameters registryName=bakiprod
//
// The Container App authenticates to this registry with its user-assigned
// managed identity (AcrPull, granted in infra/main.bicep), so no registry
// username or password is stored in Container App secrets, in GitHub secrets,
// or anywhere else. That is the whole point of moving off GHCR: a GHCR pull
// needs a classic PAT present at every restart and scale event, because
// managed identity cannot authenticate to it.
//
// The deploy pipeline's service principal needs AcrPush on this registry.

@description('Azure region for the registry.')
param location string = resourceGroup().location

@description('Registry name: 5-50 alphanumeric characters, globally unique. The login server becomes <name>.azurecr.io.')
@minLength(5)
@maxLength(50)
param registryName string

@description('Tags applied to the registry.')
param tags object = {}

@allowed([ 'Basic', 'Standard', 'Premium' ])
@description('Registry SKU. Basic is enough for a single app; Premium adds geo-replication, private endpoints and content trust.')
param sku string = 'Basic'

@description('Keep the admin user disabled. It exists only as a shared username/password pair, which is exactly the credential this setup removes.')
param adminUserEnabled bool = false

resource registry 'Microsoft.ContainerRegistry/registries@2023-07-01' = {
  name: registryName
  location: location
  tags: tags
  sku: {
    name: sku
  }
  properties: {
    adminUserEnabled: adminUserEnabled
    // Entra-only data-plane auth. Anonymous pull is off by default and is not
    // settable on this api-version, so there is nothing to declare here.
  }
}

output loginServer string = registry.properties.loginServer
output registryNameOut string = registry.name
output registryResourceId string = registry.id
