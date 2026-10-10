package hmacsig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"testing"
	"time"
)

// The canonical strings are pinned independently of the package: bakicli in the wild signs exactly these.
func TestSign_CanonicalString(t *testing.T) {
	body := []byte(`{"flowId":"f"}`)
	mac := hmac.New(sha256.New, []byte("k"))
	mac.Write([]byte("1700000000." + string(body)))
	if got, want := Sign([]byte("k"), 1700000000, body), "sha256="+hex.EncodeToString(mac.Sum(nil)); got != want {
		t.Errorf("Sign = %s, want %s", got, want)
	}
	mac = hmac.New(sha256.New, []byte("k"))
	mac.Write(body)
	if got, want := SignUntimed([]byte("k"), body), "sha256="+hex.EncodeToString(mac.Sum(nil)); got != want {
		t.Errorf("SignUntimed = %s, want %s", got, want)
	}
}

func TestVerify(t *testing.T) {
	key, body := []byte("secret"), []byte(`{"flowId":"abc"}`)
	ts := time.Now().Unix()
	sig := Sign(key, ts, body)
	if !Verify(key, ts, body, sig) {
		t.Error("a matching signature did not verify")
	}
	for name, ok := range map[string]bool{
		"other key":           Verify([]byte("other"), ts, body, sig),
		"tampered body":       Verify(key, ts, []byte(`{"flowId":"abd"}`), sig),
		"refreshed timestamp": Verify(key, ts+60, body, sig),
		"untimed for timed":   VerifyUntimed(key, body, sig),
		"timed for untimed":   Verify(key, ts, body, SignUntimed(key, body)),
		"empty signature":     Verify(key, ts, body, ""),
		"bare hex":            Verify(key, ts, body, sig[len("sha256="):]),
	} {
		if ok {
			t.Errorf("%s verified", name)
		}
	}
	if !VerifyUntimed(key, body, SignUntimed(key, body)) {
		t.Error("a matching untimed signature did not verify")
	}
}

func TestTimestamp_SkewWindow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	for _, c := range []struct {
		value string
		ok    bool
	}{
		{strconv.FormatInt(now.Unix(), 10), true},
		{strconv.FormatInt(now.Add(-MaxSkew).Unix(), 10), true},
		{strconv.FormatInt(now.Add(MaxSkew).Unix(), 10), true},
		{strconv.FormatInt(now.Add(-MaxSkew-time.Second).Unix(), 10), false},
		{strconv.FormatInt(now.Add(MaxSkew+time.Second).Unix(), 10), false},
		{"", false},
		{"12a", false},
		{"1.5", false},
	} {
		if _, ok := Timestamp(c.value, now); ok != c.ok {
			t.Errorf("Timestamp(%q) ok = %v, want %v", c.value, ok, c.ok)
		}
	}
}
