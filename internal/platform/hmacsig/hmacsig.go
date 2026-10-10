// Package hmacsig signs and verifies the HMAC-SHA256 signature of a CI webhook request (POST /api/integrations/ci,
// ADR 0019): X-Baki-Signature is "sha256=" + hex(HMAC-SHA256(key, ts + "." + body)), and X-Baki-Timestamp carries
// ts, the sender's clock in unix seconds, which must lie within MaxSkew of the verifier's. Folding the timestamp into
// the signed payload means a captured request cannot be refreshed and replayed after the window. The untimed form
// (the body alone) is the legacy one; only the deprecated deployment-wide secret may still accept it.
//
// The package is pure (stdlib only): the server's CI service and bakicli sign and verify with the same code, and the
// outbound notification webhooks sign X-Baki-Signature (untimed) and X-Baki-Signature-V2 (timed) with it (ADR 0021).
package hmacsig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"
)

// The request headers of a signed CI webhook call. KeyIDHeader names the org CI credential whose key signed it.
const (
	SignatureHeader = "X-Baki-Signature"
	TimestampHeader = "X-Baki-Timestamp"
	KeyIDHeader     = "X-Baki-Key-Id"
)

// MaxSkew is the freshness window: a timestamp further than this from the verifier's clock, either way, is refused.
const MaxSkew = 5 * time.Minute

const prefix = "sha256="

// Sign returns the signature value of body sent at unix second ts.
func Sign(key []byte, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(strconv.FormatInt(ts, 10) + "."))
	mac.Write(body)
	return prefix + hex.EncodeToString(mac.Sum(nil))
}

// SignUntimed returns the legacy signature value of body: no timestamp in the signed payload.
func SignUntimed(key, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	return prefix + hex.EncodeToString(mac.Sum(nil))
}

// Verify reports, in constant time, whether sig is Sign(key, ts, body).
func Verify(key []byte, ts int64, body []byte, sig string) bool {
	return hmac.Equal([]byte(sig), []byte(Sign(key, ts, body)))
}

// VerifyUntimed reports, in constant time, whether sig is SignUntimed(key, body).
func VerifyUntimed(key, body []byte, sig string) bool {
	return hmac.Equal([]byte(sig), []byte(SignUntimed(key, body)))
}

// Timestamp parses an X-Baki-Timestamp value and reports whether it lies within MaxSkew of now.
func Timestamp(value string, now time.Time) (int64, bool) {
	ts, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, false
	}
	skew := now.Sub(time.Unix(ts, 0))
	return ts, skew <= MaxSkew && skew >= -MaxSkew
}
