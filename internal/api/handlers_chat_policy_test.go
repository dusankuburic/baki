package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/service"
)

// Phase 4b review T4/C5: the context preview asks the provider policy synchronously (the stream reports it on
// its error event), so its refusal is a coded 400 like PROVIDER_UNKNOWN, not a masked 500.
func TestRenderModelNotAvailable_ProviderNotAllowed(t *testing.T) {
	rr := httptest.NewRecorder()
	if !renderModelNotAvailable(rr, fmt.Errorf("%w: xai is not on your organisation's list", ai.ErrProviderNotAllowed)) {
		t.Fatal("a policy refusal was not rendered")
	}
	checkStatus(t, rr, http.StatusBadRequest)
	if got := azureDecode(t, rr); got["code"] != service.ProviderCodeNotAllowed {
		t.Errorf("code = %v, want %s", got["code"], service.ProviderCodeNotAllowed)
	}
}
