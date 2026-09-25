package api

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/service"
)

func mustParse(t *testing.T, raw string, now time.Time) service.AnalyticsQuery {
	t.Helper()
	values, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("ParseQuery(%q): %v", raw, err)
	}
	q, err := parseAnalyticsQuery(values, now)
	if err != nil {
		t.Fatalf("parseAnalyticsQuery(%q): %v", raw, err)
	}
	return q
}

func TestParseAnalyticsQuery_Defaults(t *testing.T) {
	now := time.Date(2026, 9, 24, 15, 30, 0, 0, time.UTC)
	q := mustParse(t, "", now)

	if q.Preset != "30d" || q.Days != 30 {
		t.Errorf("preset/days = %q/%d, want 30d/30", q.Preset, q.Days)
	}
	if q.TZ != "UTC" || q.Loc != time.UTC {
		t.Errorf("tz = %q/%v, want UTC", q.TZ, q.Loc)
	}
	wantFrom := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	wantTo := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	if !q.From.Equal(wantFrom) || !q.To.Equal(wantTo) {
		t.Errorf("window = [%v, %v), want [%v, %v)", q.From, q.To, wantFrom, wantTo)
	}
	if got := q.PrevFrom(); !got.Equal(wantFrom.AddDate(0, 0, -30)) {
		t.Errorf("PrevFrom = %v, want %v", got, wantFrom.AddDate(0, 0, -30))
	}
	if q.Scope != "all" || q.OrgID != "" {
		t.Errorf("scope = %q/%q, want all/\"\"", q.Scope, q.OrgID)
	}
	if !q.Compare {
		t.Error("compare must default to true")
	}
	for name, list := range map[string][]string{"flows": q.Flows, "severity": q.Severity, "category": q.Category, "rule": q.Rule} {
		if list == nil || len(list) != 0 {
			t.Errorf("%s = %#v, want an empty, non-nil list", name, list)
		}
	}
	if q.Key() == "" || !strings.Contains(q.Key(), "30d|") {
		t.Errorf("Key() = %q, want the preset in it", q.Key())
	}
}

func TestParseAnalyticsQuery_PresetsAndTimeZone(t *testing.T) {
	// 20:00 UTC on Sep 24 is already Sep 25 in Tokyo: "today" and the whole
	// window follow the reader's calendar, not the server's.
	now := time.Date(2026, 9, 24, 20, 0, 0, 0, time.UTC)
	q := mustParse(t, "range=7d&tz=Asia/Tokyo", now)
	if q.Days != 7 || q.Preset != "7d" {
		t.Errorf("preset/days = %q/%d, want 7d/7", q.Preset, q.Days)
	}
	if q.From.Location().String() != "Asia/Tokyo" {
		t.Errorf("From location = %v, want Asia/Tokyo", q.From.Location())
	}
	if got := q.From.Format("2006-01-02"); got != "2026-09-19" {
		t.Errorf("From = %s, want 2026-09-19", got)
	}
	if got := q.To.Format("2006-01-02"); got != "2026-09-26" {
		t.Errorf("To = %s, want 2026-09-26 (exclusive)", got)
	}
	if q90 := mustParse(t, "range=90d", now); q90.Days != 90 || q90.From.Format("2006-01-02") != "2026-06-27" {
		t.Errorf("90d from = %s (days %d), want 2026-06-27", q90.From.Format("2006-01-02"), q90.Days)
	}
}

func TestParseAnalyticsQuery_CustomRange(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	q := mustParse(t, "range=custom&from=2026-09-01&to=2026-09-10", now)
	if q.Preset != "custom" || q.Days != 10 {
		t.Errorf("preset/days = %q/%d, want custom/10", q.Preset, q.Days)
	}
	if q.From.Format("2006-01-02") != "2026-09-01" || q.To.Format("2006-01-02") != "2026-09-11" {
		t.Errorf("window = [%v, %v), want [Sep 1, Sep 11)", q.From, q.To)
	}
	// A single day is a valid range; today is the latest allowed end.
	if one := mustParse(t, "range=custom&from=2026-09-24&to=2026-09-24", now); one.Days != 1 {
		t.Errorf("single-day range: days = %d, want 1", one.Days)
	}
	// Day counting is calendar-based: the EU spring-forward day is 23 hours
	// long, but the span is still three days.
	dst := mustParse(t, "range=custom&from=2026-03-28&to=2026-03-30&tz=Europe/Berlin", now)
	if dst.Days != 3 {
		t.Errorf("DST span days = %d, want 3", dst.Days)
	}
	if got := daysBetween(dst.From, dst.To); got != 3 {
		t.Errorf("daysBetween across DST = %d, want 3", got)
	}
}

func TestParseAnalyticsQuery_Lists(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	q := mustParse(t, "flows=b,a,b,%20c%20,&severity=warning,error&category=Best%20Practices,Security&rule=x.y-z_1&compare=0", now)
	if strings.Join(q.Flows, ",") != "a,b,c" {
		t.Errorf("flows = %v, want [a b c] (deduped, trimmed, sorted)", q.Flows)
	}
	if strings.Join(q.Severity, ",") != "error,warning" {
		t.Errorf("severity = %v, want [error warning]", q.Severity)
	}
	if strings.Join(q.Category, "|") != "Best Practices|Security" {
		t.Errorf("category = %v", q.Category)
	}
	if strings.Join(q.Rule, ",") != "x.y-z_1" {
		t.Errorf("rule = %v", q.Rule)
	}
	if q.Compare {
		t.Error("compare=0 must disable the comparison")
	}
	if f := mustParse(t, "compare=false", now); f.Compare {
		t.Error("compare=false must disable the comparison")
	}
	org := mustParse(t, "scope=org&orgId=acme-1", now)
	if org.Scope != "org" || org.OrgID != "acme-1" {
		t.Errorf("org scope = %q/%q", org.Scope, org.OrgID)
	}
	if p := mustParse(t, "scope=personal", now); p.Scope != "personal" {
		t.Errorf("personal scope = %q", p.Scope)
	}
}

func TestParseAnalyticsQuery_Rejects(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	many := make([]string, 51)
	for i := range many {
		many[i] = "f" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	cases := []struct{ name, query, want string }{
		{"bad range", "range=1y", "range must be"},
		{"custom missing to", "range=custom&from=2026-09-01", "from and to"},
		{"custom bad from", "range=custom&from=2026-13-01&to=2026-09-10", "from must be"},
		{"custom bad to", "range=custom&from=2026-09-01&to=nope", "to must be"},
		{"custom reversed", "range=custom&from=2026-09-10&to=2026-09-01", "from must not be after"},
		{"custom future", "range=custom&from=2026-09-20&to=2026-09-30", "future"},
		{"custom too long", "range=custom&from=2025-01-01&to=2026-09-01", "at most 366"},
		{"tz pattern", "tz=Not%20A%20Zone!", "IANA"},
		{"tz unknown", "tz=Mars/Phobos", "not a known"},
		{"scope unknown", "scope=team", "scope must be"},
		{"org without id", "scope=org", "needs orgId"},
		{"org bad id", "scope=org&orgId=bad%20id", "not a valid id"},
		{"personal with org", "scope=personal&orgId=acme", "only applies"},
		{"all with org", "orgId=acme", "only applies"},
		{"severity value", "severity=critical", "severity contains"},
		{"rule uppercase", "rule=Hardcoded", "rule contains"},
		{"category symbols", "category=Sec%3Burity", "category contains"},
		{"flows too many", "flows=" + strings.Join(many, ","), "at most 50"},
		{"compare", "compare=maybe", "compare must be"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			values, perr := url.ParseQuery(tc.query)
			if perr != nil {
				t.Fatalf("ParseQuery: %v", perr)
			}
			_, err := parseAnalyticsQuery(values, now)
			if err == nil {
				t.Fatalf("expected an error for %q", tc.query)
			}
			if !errors.Is(err, service.ErrInvalidInput) {
				t.Errorf("error must wrap ErrInvalidInput (→ 400): %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}
