package api

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/platform/apperr"
	"pad-analyzer/internal/platform/hmacsig"
	"pad-analyzer/internal/service"
	"pad-core/analyzer"
	"pad-core/export"
	"pad-core/logger"
	"pad-core/models"
)

// maxCIWebhookBodyBytes bounds the CI webhook body: the signature covers the WHOLE raw body, which is read before
// any of it is parsed — without a cap an unauthenticated caller could feed gigabytes straight into memory.
const maxCIWebhookBodyBytes = 1 << 20

// ciWebhookUnconfiguredWarn gates the one WARN a process logs when the CI webhook is called on a deployment that
// cannot authenticate any call (no accounts, no PAD_CI_WEBHOOK_SECRET): the operator learns of it once, repeated
// calls cost nothing in the log.
var ciWebhookUnconfiguredWarn sync.Once

// ciWebhookFormats are the answer shapes of the CI webhook; any other format is a 400 UNKNOWN_FORMAT listing them.
var ciWebhookFormats = []string{"summary", "report", "sarif"}

// codeUnknownFormat answers a format the endpoint does not render; details.formats lists the ones it does.
const codeUnknownFormat = "UNKNOWN_FORMAT"

// ciDeprecationLink is the Link header of an answer to the deprecated deployment-wide secret: the decision record
// that says what replaces it.
const ciDeprecationLink = `<https://github.com/dusankuburic/baki/blob/master/docs/adr/0019-org-ci-credentials.md>; rel="deprecation"`

// ciWebhookRequest is the body a CI runner POSTs: FlowID identifies a stored flow; FailOn (mirrors
// bakicli) sets the severity at which the response reports passed=false, so a pipeline can gate.
type ciWebhookRequest struct {
	FlowID string `json:"flowId"`
	FailOn string `json:"failOn,omitempty"` // "error" (default) | "warning" | "info" | "none"
	Format string `json:"format,omitempty"` // "summary" (default) | "report" | "sarif"
	// DeliveryID is the call's random id, in the body so the signature covers it: a key's id runs once (ADR 0019).
	// Optional while bakicli releases before it are in use; a release before this one ignores the field.
	DeliveryID string `json:"deliveryId,omitempty"`
}

// ciWebhookResponse is the summary shape returned for format="summary". It
// carries enough for a CI step to gate AND to print a compact job log.
// Errors/Warnings/Info count every finding (report.Stats); Passed is decided
// on the gating ones only.
type ciWebhookResponse struct {
	FlowID   string `json:"flowId"`
	FlowName string `json:"flowName,omitempty"`
	Passed   bool   `json:"passed"`
	Gate     string `json:"gate"`
	Errors   int    `json:"errors"`
	Warnings int    `json:"warnings"`
	Info     int    `json:"info"`
	// Preview counts the findings of preview rules: reported, never gating.
	Preview int `json:"preview,omitempty"`
	// Findings is omitted unless format="report".
	Findings []models.Finding `json:"findings,omitempty"`
}

// handleCIWebhook analyzes a stored flow on demand from a CI runner. Auth is an HMAC signature (X-Baki-Signature)
// or a personal API token with the ci scope, NOT a user session: the route authenticates itself
// (route.SelfAuthenticated), and CIService decides who the caller is — an organisation's CI credential, which reaches
// that organisation's flows only; a token, which reaches the flows its user can read; or the deprecated
// deployment-wide PAD_CI_WEBHOOK_SECRET, which reaches any flow by id (ADR 0019). A signed call runs as an
// integration, never as a user; a token's call runs as the token. Each key's delivery id runs once.
// @Summary      CI webhook
// @Description  Analyzes a stored flow for a CI runner. Authenticated by an HMAC-SHA256 of "<X-Baki-Timestamp>." + body, sent as X-Baki-Signature: sha256=<hex>; the timestamp must be within 5 minutes. The key is an organisation CI credential (baki_ci_<keyId>_<secret>, minted under /api/orgs/{id}/ci-credentials; send X-Baki-Key-Id: <keyId>), which reaches that organisation's flows only, or the deprecated deployment-wide PAD_CI_WEBHOOK_SECRET (answered with Deprecation and Link headers where organisations exist). Instead of a signature, Authorization: Bearer <API token> with the ci scope reaches the flows the token's user can read, an organisation's flow only when the user is a member of it; an unscoped token or one without ci is a 401, and another user's flow is a 404 like an unknown one. Body: {flowId, failOn, format, deliveryId}. failOn is error (default), warning, info or none; findings of preview rules never fail the gate and are counted in `preview`. format "summary" (default) returns {flowId, flowName, passed, gate, errors, warnings, info, preview}, "report" adds the findings, "sarif" returns the SARIF 2.1.0 document instead (application/sarif+json); a missing or empty format is "summary", any other is a 400 with code UNKNOWN_FORMAT and details.formats listing the three. deliveryId (16 to 128 characters of A-Z, a-z, 0-9, - and _; bakicli sends 128 random bits) makes the call run once per key: a call whose key sent the same deliveryId before is a 409 CI_REPLAYED and runs nothing. It is optional for now.
// @Tags         analysis
// @Accept       json
// @Produce      json,application/sarif+json
// @Param        X-Baki-Signature header string false "sha256=<hex HMAC-SHA256 of timestamp.body>; required without Authorization"
// @Param        X-Baki-Timestamp header string false "Unix seconds; required with X-Baki-Signature"
// @Param        X-Baki-Key-Id header string false "Key id of the organisation CI credential"
// @Param        Authorization header string false "Bearer <API token with the ci scope>, instead of a signature"
// @Param        request body object true "{flowId, failOn, format, deliveryId}"
// @Success      200 {object} map[string]interface{} "Gate verdict, or the SARIF document"
// @Failure      400 {object} render.ErrorResponse "Bad Request (no flowId, malformed deliveryId; code UNKNOWN_FORMAT with details.formats for an unknown format)"
// @Failure      401 {object} map[string]string "Missing or invalid signature, key, timestamp or API token"
// @Failure      404 {object} map[string]string "Flow not found (or outside what the credential or token reaches)"
// @Failure      409 {object} map[string]string "CI_REPLAYED: this key already sent the deliveryId"
// @Failure      413 {object} map[string]string "Body over 1 MiB"
// @Failure      500 {object} map[string]string "Error"
// @Failure      503 {object} map[string]string "Not configured (no accounts and no PAD_CI_WEBHOOK_SECRET), or no server store"
// @Router       /api/integrations/ci [post]
func (h *AnalysisHandler) handleCIWebhook(w http.ResponseWriter, r *http.Request) {
	if !h.ci.Configured() {
		// The endpoint is public, so render.Error's per-request ERROR log for
		// this 503 let any unauthenticated caller flood the error log. Same
		// status and body; the misconfiguration is reported once per process.
		ciWebhookUnconfiguredWarn.Do(func() {
			logger.Warn("CI webhook called but not configured; POST /api/integrations/ci answers 503 until PAD_CI_WEBHOOK_SECRET is set")
		})
		render.ErrorUnlogged(w, fmt.Errorf("CI webhook not configured: set PAD_CI_WEBHOOK_SECRET"), http.StatusServiceUnavailable)
		return
	}
	// The signature covers the raw body: bound it before the first read (the
	// router's 10 MiB global cap never applied to this self-authenticated route).
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCIWebhookBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			render.Error(w, fmt.Errorf("CI webhook body exceeds %d bytes", maxCIWebhookBodyBytes), http.StatusRequestEntityTooLarge)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	ctx := r.Context()
	var caller *service.CICaller
	if bearer := auth.ExtractToken(r); bearer != "" {
		// A personal API token, verified as jwtAuth verifies one; CIService admits it only with the ci scope. The
		// bearer decides alone: signature headers beside it are not consulted.
		var claims *auth.Claims
		if auth.IsAPIToken(bearer) {
			claims = h.security.verifyAPIToken(ctx, bearer)
		}
		if caller, err = h.ci.AuthenticateToken(auth.ActorFromClaims(claims)); err == nil {
			// Audit records the token's user from the claims, as on every route jwtAuth authenticates.
			ctx = auth.WithClaims(ctx, claims)
			recordActor(r, caller.Actor())
		}
	} else {
		caller, err = h.ci.Authenticate(ctx, service.CIRequest{
			KeyID:     r.Header.Get(hmacsig.KeyIDHeader),
			Timestamp: r.Header.Get(hmacsig.TimestampHeader),
			Signature: r.Header.Get(hmacsig.SignatureHeader),
			Body:      body,
		})
	}
	if err != nil {
		if _, typed := apperr.As(err); !typed {
			// A store failure while looking for the key: the server's, not the caller's.
			render.Error(w, err, http.StatusInternalServerError)
			return
		}
		render.Error(w, err, 0)
		return
	}
	if caller.Deprecated {
		w.Header().Set("Deprecation", "true")
		w.Header().Set("Link", ciDeprecationLink)
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	var req ciWebhookRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.FlowID == "" {
		render.Error(w, fmt.Errorf("flowId is required"), http.StatusBadRequest)
		return
	}
	// The gate is one of four values; anything else gates as error (fail closed) and is recorded as such, so the
	// caller's text never reaches the response or the audit row.
	gate := strings.ToLower(strings.TrimSpace(req.FailOn))
	if !slices.Contains(ciWebhookGates, gate) {
		gate = "error"
	}
	format := strings.ToLower(strings.TrimSpace(req.Format))
	if format == "" {
		format = "summary"
	}
	if !slices.Contains(ciWebhookFormats, format) {
		// A code and the list, not just the message: a pipeline script reads the envelope, and a summary in place of
		// the shape it asked for would be read as a verdict.
		render.Error(w, apperr.Invalid(codeUnknownFormat, fmt.Sprintf("unknown format %q (must be %s)", req.Format, formatChoices(ciWebhookFormats))).
			WithDetails(map[string]any{"formats": ciWebhookFormats}), 0)
		return
	}

	// A signed run acts as the integration: audit records it, and authorization would refuse it everything. A
	// token's run acts as the token.
	ctx = auth.WithActor(ctx, caller.Actor())
	// Claimed before anything runs, once the call has authenticated and is well formed: a replay resolves no flow
	// and analyses nothing.
	if err := h.ci.ClaimDelivery(ctx, caller, req.DeliveryID); err != nil {
		if _, typed := apperr.As(err); !typed {
			render.Error(w, err, http.StatusInternalServerError)
			return
		}
		render.Error(w, err, 0)
		return
	}
	doc, err := h.ci.ResolveFlow(ctx, caller, req.FlowID)
	if err != nil {
		// One static 404 for an unknown flow and for one outside what the credential or token reaches; a wrapped
		// driver error (SQL state, blob details) must not reach an external caller either. The cause stays server-side.
		logger.FromContext(ctx).Warn("ci webhook: flow resolve failed", "flowId", req.FlowID, "method", caller.Method, "err", err)
		render.Error(w, fmt.Errorf("flow not found"), http.StatusNotFound)
		return
	}

	report, fresh, err := h.analysisSvc.AnalyzeFlowFresh(ctx, doc)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	// A report is recorded once, detached and bounded, as the interactive analyze does: a cache hit (a CI poll of
	// an unchanged flow) already recorded it and leaves the dashboard alone.
	if fresh {
		h.recordAnalysis(ctx, doc, report)
	}
	counts := analyzer.CountGating(report.Findings)
	passed := gatePasses(counts, gate)
	h.audit.Record(ctx, audit.Entry{Action: audit.ActionCIWebhookRun, ResourceType: "flow", ResourceID: doc.ID, OrgID: doc.OrganizationID,
		Meta: map[string]string{"method": caller.Method, "format": format, "gate": gate, "passed": strconv.FormatBool(passed)}})

	// SARIF format short-circuits the summary envelope: the same document as
	// analyze-raw and /api/analysis/export/sarif, as rendered.
	if format == "sarif" {
		body, f, err := h.exports.Render("sarif", export.Input{Report: report, Doc: doc}, export.Options{})
		if err != nil {
			render.Error(w, err, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", f.MIME)
		_, _ = w.Write(body)
		return
	}

	resp := ciWebhookResponse{
		FlowID:   doc.ID,
		FlowName: doc.Name,
		Passed:   passed,
		Gate:     gate,
		Errors:   report.Stats.Errors,
		Warnings: report.Stats.Warnings,
		Info:     report.Stats.Info,
		Preview:  counts.Preview,
	}
	if format == "report" {
		resp.Findings = report.Findings
	}
	// The CI gate verdict rides in the body (passed/reason), not the status —
	// CI callers want the structured result even on a failed gate.
	render.JSON(w, resp)
}

// gatePasses reports whether the gating findings (analyzer.CountGating: preview
// findings never gate) clear the requested severity gate. gate="none" always
// passes (report-only); "error" passes with zero errors; "warning" requires
// zero errors AND zero warnings; "info" requires a clean report.
// ciWebhookGates are the failOn values the webhook knows (gatePasses).
var ciWebhookGates = []string{"error", "warning", "info", "none"}

func gatePasses(c analyzer.GateCounts, gate string) bool {
	switch gate {
	case "none":
		return true
	case "warning":
		return c.Errors == 0 && c.Warnings == 0
	case "info":
		return c.Errors == 0 && c.Warnings == 0 && c.Info == 0
	default: // "error", "" and unknown gates
		return c.Errors == 0
	}
}
