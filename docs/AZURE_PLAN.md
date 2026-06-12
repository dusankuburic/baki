# Azure Production Readiness Plan: Baki Backend

This document outlines the architectural and code-level changes required to transition the Baki Backend from a generic containerized application to a production-ready service hosted on Azure (e.g., Azure Container Apps or AKS).

## 1. Security & Secrets Management (Azure Key Vault)

**Current State:** Secrets like `PAD_AUTH_SECRET` and `PAD_DATABASE_URL` are passed as environment variables.
**Target State:** Secrets are stored in Azure Key Vault and fetched at runtime using Managed Identity.

### Implementation Steps:
- **Dependency:** Add `github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets` and `github.com/Azure/azure-sdk-for-go/sdk/azidentity`.
- **Refactor `internal/config`:** 
    - Add `KeyVaultURL` to `ServerConfig`.
    - Modify `loadConfig` to initialize a Key Vault client if `AZURE_KEYVAULT_URL` is set.
    - Implement a "Secret Provider" interface that can resolve secrets from either ENV or Key Vault.
- **Bootstrapping:** The app will use `DefaultAzureCredential()` which works seamlessly in Azure (Managed Identity) and locally (Azure CLI/Environment).

## 2. Passwordless Database (Managed Identity)

**Current State:** Connection string contains hardcoded credentials.
**Target State:** Authentication to Azure Database for PostgreSQL using Entra ID (Managed Identity) tokens.

### Implementation Steps:
- **Dependency Upgrade:** Migrate from `github.com/lib/pq` to `github.com/jackc/pgx/v5`. `pgx` is the modern standard for Go/Postgres and supports dynamic `BeforeConnect` hooks.
- **Logic:**
    - In `internal/storage/database`, implement a token provider that calls `azidentity` to get an Entra ID token for the resource `https://ossrdbms-aad.database.windows.net`.
    - Configure `pgx` to use this token as the password, refreshing it before expiry.
- **Benefit:** No database passwords stored in config or secrets.

## 3. Observability (OpenTelemetry & App Insights)

**Current State:** Basic Prometheus metrics and JSON logs.
**Target State:** Full distributed tracing and integrated logging in Azure Monitor/Application Insights.

### Implementation Steps:
- **Dependency:** Add OpenTelemetry Go SDK (`go.opentelemetry.io/otel`) and the Azure Monitor Exporter (`github.com/Azure/azure-sdk-for-go/sdk/monitor/azquery` or the OTLP exporter).
- **Instrumentation:**
    - **HTTP:** Wrap the router with `otelhttp`.
    - **SQL:** Use `otelpgx` to trace database queries.
    - **AI Providers:** Add manual spans in `internal/ai` to track latency and token usage per provider call.
- **Exporter:** Configure the OpenTelemetry SDK to export data to the Azure Monitor workspace via the Application Insights Connection String.

## 4. Platform Optimization (Azure Container Apps)

**Current State:** Dockerfile optimized for generic Alpine.
**Target State:** Enhanced for Azure-native orchestration.

### Implementation Steps:
- **Health Probes:** 
    - Ensure `/healthz` (liveness) and `/readyz` (readiness) are correctly implemented.
    - `/readyz` should verify database connectivity and Key Vault reachability.
- **Graceful Shutdown:** Verify the `main.go` signal handling (SIGTERM) allows enough time for the Azure Load Balancer to drain connections. Current 10s timeout is good, but may need tuning.
- **Log Aggregation:** Ensure `StdoutOnly` is the default in cloud mode to leverage Azure's automatic log collection into Log Analytics.

## Next Steps

1.  **Dependency Updates:** Run `go get` for Azure and OpenTelemetry SDKs.
2.  **Config Refactor:** Implement the Key Vault secret resolver.
3.  **Storage Refactor:** Implement `pgx` with Managed Identity support.
4.  **Telemetry Setup:** Add the OpenTelemetry initialization logic.
