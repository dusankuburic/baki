package audit

import (
	"log/slog"
	"sort"
	"strings"

	"pad-analyzer/internal/metrics"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// fallback emits an event to the structured-log sink when it can't be written to the store, keeping it
// discoverable (pair with pad_audit_dropped_total). PII is REDACTED here: container logs are more broadly
// readable than the audit table, and a DB outage would otherwise dump every event's email/IP/meta into them —
// masked values plus user_id keep the event attributable.
func fallback(reason string, ev *storageif.AuditEvent) {
	metrics.RecordAuditDropped(reason)
	slog.Error("audit event diverted to log fallback sink",
		"reason", reason,
		"audit_id", ev.ID,
		"user_id", ev.UserID,
		"email", RedactEmail(ev.Email),
		"action", ev.Action,
		"resource_type", ev.ResourceType,
		"resource_id", ev.ResourceID,
		"org_id", ev.OrgID,
		"actor_kind", ev.ActorKind,
		"ip", redactIP(ev.IP),
		// Meta values may carry free-form PII; the keys alone say what KIND of change happened.
		"meta_keys", metaKeys(ev.Meta),
		"created_at", ev.CreatedAt,
	)
}

// RedactEmail keeps the first character of the local part and the full domain — enough to correlate log lines
// with a known account without exposing the address itself.
func RedactEmail(email string) string {
	if email == "" {
		return ""
	}
	at := strings.LastIndexByte(email, '@')
	if at <= 0 {
		return "***"
	}
	return email[:1] + "***" + email[at:]
}

// redactIP keeps the network prefix and masks the host part ("203.0.113.7" → "203.0.x.x") — enough to
// distinguish "office vs unknown network" without logging the full address.
func redactIP(ip string) string {
	if ip == "" {
		return ""
	}
	if parts := strings.Split(ip, "."); len(parts) == 4 {
		return parts[0] + "." + parts[1] + ".x.x"
	}
	if i := strings.IndexByte(ip, ':'); i > 0 {
		return ip[:i] + ":…"
	}
	return "***"
}

// metaKeys returns the sorted key set of an audit meta map (values omitted — free-form, may carry PII).
func metaKeys(meta map[string]string) []string {
	if len(meta) == 0 {
		return nil
	}
	keys := make([]string, 0, len(meta))
	for k := range meta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
