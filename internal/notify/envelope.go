package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"pad-analyzer/internal/platform/hmacsig"
	"pad-core/logger"
)

// The webhook envelope, v1 additive (ADR 0021, docs/webhooks.md). Every HTTP delivery carries X-Baki-Event,
// X-Baki-Delivery (one id per event and channel, the same across retries), X-Baki-Timestamp (per attempt) and a
// User-Agent of baki/<version>. A signed kind with a secret adds X-Baki-Signature-V2, the HMAC of
// "<timestamp>." + body (the inbound CI webhook's canonical string), and keeps X-Baki-Signature, the HMAC of the body
// alone, for receivers that verify it today (deprecated, no removal date). The generic webhook's body adds
// schemaVersion, id, deliveryId and, on an organisation's channel, orgId to the event's flat fields.

// SchemaVersion is the generic webhook body's version: 1 is the flat event plus the additive envelope fields.
const SchemaVersion = 1

// Envelope headers.
const (
	HeaderEvent       = "X-Baki-Event"
	HeaderDelivery    = "X-Baki-Delivery"
	HeaderTimestamp   = "X-Baki-Timestamp"
	HeaderSignature   = "X-Baki-Signature"
	HeaderSignatureV2 = "X-Baki-Signature-V2"
)

// SignatureTolerance is how far a V2 signature's timestamp may be from the receiver's clock (VerifySignature).
const SignatureTolerance = 5 * time.Minute

// Envelope is what stamps a delivery: its clock, its delivery ids and its User-Agent. The zero value is the real
// clock, random UUIDs and "baki"; tests inject fixed ones.
type Envelope struct {
	Now       func() time.Time
	NewID     func() string
	UserAgent string
}

func (e Envelope) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e Envelope) deliveryID() string {
	if e.NewID != nil {
		return e.NewID()
	}
	return uuid.NewString()
}

func (e Envelope) userAgent() string {
	if e.UserAgent != "" {
		return e.UserAgent
	}
	return "baki"
}

// UserAgent is the User-Agent of a build: baki/<version>.
func UserAgent(version string) string {
	if version == "" {
		return "baki"
	}
	return "baki/" + version
}

// EventID is the id of the event a stored row raised (an inbox alert's id, a notice's id): "evt_" and 24 hex
// characters of its SHA-256, so every channel, retry and replica sends the same id for it.
func EventID(rowID string) string {
	sum := sha256.Sum256([]byte(rowID))
	return "evt_" + hex.EncodeToString(sum[:12])
}

// NewEventID is the id of an event no row raised (an analysis_complete, a channel test): "evt_" and 24 random hex
// characters.
func NewEventID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "evt_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]
	}
	return "evt_" + hex.EncodeToString(b[:])
}

// signBody is X-Baki-Signature's value: the HMAC-SHA256 of the body alone.
func signBody(secret string, body []byte) string {
	return hmacsig.SignUntimed([]byte(secret), body)
}

// signV2 is X-Baki-Signature-V2's value: the HMAC-SHA256 of "<ts>." + body, the canonical string the inbound CI
// webhook verifies (one implementation for both directions, internal/platform/hmacsig).
func signV2(secret string, ts int64, body []byte) string {
	return hmacsig.Sign([]byte(secret), ts, body)
}

// VerifySignature checks a delivery the way a receiver should: signature is the X-Baki-Signature-V2 header, timestamp
// the X-Baki-Timestamp header (Unix seconds), body the raw request body. It refuses a timestamp more than
// SignatureTolerance from now, and compares in constant time. docs/webhooks.md shows the same check in a few lines.
func VerifySignature(secret, timestamp, signature string, body []byte, now time.Time) error {
	if secret == "" {
		return errors.New("notify: no signing secret")
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return errors.New("notify: invalid timestamp")
	}
	if d := now.Sub(time.Unix(ts, 0)); d > SignatureTolerance || d < -SignatureTolerance {
		return errors.New("notify: timestamp outside the tolerance")
	}
	if !hmacsig.Verify([]byte(secret), ts, body, signature) {
		return errors.New("notify: signature mismatch")
	}
	return nil
}

// StatusError is a delivery the endpoint answered with a non-2xx status.
type StatusError struct {
	Status    int
	Retryable bool
}

func (e *StatusError) Error() string {
	if e.Retryable {
		return fmt.Sprintf("status %d (retryable)", e.Status)
	}
	return fmt.Sprintf("unexpected status %d", e.Status)
}

// RedactionMarker stands for the part of a URL that is never shown (RedactURL, ListedURL).
const RedactionMarker = "…"

// RedactURL keeps a URL's scheme and host: for Slack and Teams the webhook URL is the credential (anyone holding it
// can post into the channel), and generic webhooks often carry a token in the path or query. It is the only form of
// a channel's URL in an error or a log line; the API shows ListedURL.
func RedactURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return RedactionMarker
	}
	return u.Scheme + "://" + u.Host + "/" + RedactionMarker
}

// ListedURL is what the API shows of a channel's URL: its host and "/…", without the scheme. Unlike RedactURL's
// form it is never a valid channel URL itself, so a stale tab, a pasted listing or a rolled-back release that
// re-sends it cannot store it as the real URL.
func ListedURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return RedactionMarker
	}
	return u.Host + "/" + RedactionMarker
}

// HasRedactionMarker reports whether a submitted URL carries the redaction marker, as typed or percent-encoded: a
// redacted URL sent back as a new one.
func HasRedactionMarker(raw string) bool {
	return strings.Contains(raw, RedactionMarker) || strings.Contains(strings.ToUpper(raw), "%E2%80%A6")
}

// RedactError strips the URL's path and query from the *url.Error in err's chain (the client's transport errors name
// the full URL), so a log line or an error report never carries the credential (B4). Errors notify builds itself
// never name more than RedactURL.
func RedactError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		ue.URL = RedactURL(ue.URL)
	}
	return err
}

// httpDelivery is one event's POST to one channel: the body, built once, and the headers every attempt carries.
type httpDelivery struct {
	client      *http.Client
	url         string
	body        []byte
	header      http.Header
	secret      string // signs the attempt (V2) and the body (legacy) when not ""
	env         Envelope
	maxAttempts int
	backoff     time.Duration
}

// newHTTPDelivery stamps the envelope headers of ev's delivery to a channel.
func newHTTPDelivery(client *http.Client, target string, body []byte, ev Event, deliveryID, secret string, env Envelope, maxAttempts int, backoff time.Duration) *httpDelivery {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", env.userAgent())
	h.Set(HeaderEvent, string(ev.Type))
	h.Set(HeaderDelivery, deliveryID)
	if secret != "" {
		h.Set(HeaderSignature, signBody(secret, body))
	}
	return &httpDelivery{client: client, url: target, body: body, header: h, secret: secret, env: env, maxAttempts: maxAttempts, backoff: backoff}
}

// post sends the delivery: any non-2xx is an error, and transient failures (a network error, 429, 5xx) are retried
// with exponential backoff, within ctx. Each attempt carries its own timestamp and V2 signature.
func (d *httpDelivery) post(ctx context.Context) error {
	maxAttempts, backoff := d.maxAttempts, d.backoff
	if maxAttempts <= 0 {
		maxAttempts = defaultRetries
	}
	if backoff <= 0 {
		backoff = defaultBackoff
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff << (attempt - 1)):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(d.body))
		if err != nil {
			// The parse error quotes the whole URL.
			return fmt.Errorf("invalid alert URL %s", RedactURL(d.url))
		}
		req.Header = d.header.Clone()
		ts := d.env.now().Unix()
		req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
		if d.secret != "" {
			req.Header.Set(HeaderSignatureV2, signV2(d.secret, ts, d.body))
		}

		resp, err := d.client.Do(req)
		if err != nil {
			lastErr = RedactError(err)
			logger.Warn("notify: HTTP error, will retry", "attempt", attempt+1, "err", lastErr)
			continue
		}
		// Drain a little so the connection can be reused; the body itself is not read.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		// 429 (rate-limited) and 5xx (server error) are retryable; any other 4xx is permanent.
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = &StatusError{Status: resp.StatusCode, Retryable: true}
			logger.Warn("notify: retryable status, will retry", "attempt", attempt+1, "status", resp.StatusCode)
			continue
		}
		return &StatusError{Status: resp.StatusCode}
	}
	return fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}
