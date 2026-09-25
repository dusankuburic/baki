package api

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"pad-analyzer/internal/service"
)

// Query parameters of GET /api/dashboard/analytics. Parsing is pure (now is a
// parameter) so every branch is table-tested; each failure wraps
// service.ErrInvalidInput, which render.Error turns into a 400 naming the field.

const (
	maxAnalyticsSpanDays = 366
	maxAnalyticsListLen  = 50
	maxAnalyticsCatLen   = 20
)

var (
	tzPattern       = regexp.MustCompile(`^[A-Za-z0-9_+\-/]{1,64}$`)
	idPattern       = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	rulePattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	categoryPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9 _-]{0,31}$`)
	presetDays      = map[string]int{"7d": 7, "30d": 30, "90d": 90}
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", service.ErrInvalidInput, fmt.Sprintf(format, args...))
}

// parseList splits a comma-separated parameter into a trimmed, deduplicated,
// sorted list validated against pattern, capped at max entries.
func parseList(raw, name string, pattern *regexp.Regexp, max int) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return []string{}, nil
	}
	seen := map[string]bool{}
	out := []string{}
	for _, part := range strings.Split(raw, ",") {
		v := strings.TrimSpace(part)
		if v == "" || seen[v] {
			continue
		}
		if !pattern.MatchString(v) {
			return nil, invalid("%s contains an invalid value", name)
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) > max {
		return nil, invalid("%s accepts at most %d values", name, max)
	}
	sort.Strings(out)
	return out, nil
}

func parseAnalyticsQuery(q url.Values, now time.Time) (service.AnalyticsQuery, error) {
	var out service.AnalyticsQuery

	tz := strings.TrimSpace(q.Get("tz"))
	if tz == "" {
		tz = "UTC"
	}
	if !tzPattern.MatchString(tz) {
		return out, invalid("tz must be an IANA time zone name")
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return out, invalid("tz is not a known time zone")
	}
	out.TZ, out.Loc = tz, loc
	local := now.In(loc)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)

	preset := strings.TrimSpace(q.Get("range"))
	if preset == "" {
		preset = "30d"
	}
	switch {
	case presetDays[preset] > 0:
		n := presetDays[preset]
		out.Preset, out.Days = preset, n
		out.From, out.To = today.AddDate(0, 0, -(n-1)), today.AddDate(0, 0, 1)
	case preset == "custom":
		fromRaw, toRaw := strings.TrimSpace(q.Get("from")), strings.TrimSpace(q.Get("to"))
		if fromRaw == "" || toRaw == "" {
			return out, invalid("range=custom needs from and to as YYYY-MM-DD")
		}
		from, err := time.ParseInLocation("2006-01-02", fromRaw, loc)
		if err != nil {
			return out, invalid("from must be YYYY-MM-DD")
		}
		to, err := time.ParseInLocation("2006-01-02", toRaw, loc)
		if err != nil {
			return out, invalid("to must be YYYY-MM-DD")
		}
		if to.Before(from) {
			return out, invalid("from must not be after to")
		}
		if to.After(today) {
			return out, invalid("to must not be in the future")
		}
		days := daysBetween(from, to) + 1
		if days > maxAnalyticsSpanDays {
			return out, invalid("the range must span at most %d days", maxAnalyticsSpanDays)
		}
		out.Preset, out.Days = "custom", days
		out.From, out.To = from, to.AddDate(0, 0, 1)
	default:
		return out, invalid("range must be 7d, 30d, 90d or custom")
	}

	scope := strings.TrimSpace(q.Get("scope"))
	if scope == "" {
		scope = "all"
	}
	orgID := strings.TrimSpace(q.Get("orgId"))
	switch scope {
	case "personal", "all":
		if orgID != "" {
			return out, invalid("orgId only applies to scope=org")
		}
	case "org":
		if orgID == "" {
			return out, invalid("scope=org needs orgId")
		}
		if !idPattern.MatchString(orgID) {
			return out, invalid("orgId is not a valid id")
		}
	default:
		return out, invalid("scope must be personal, org or all")
	}
	out.Scope, out.OrgID = scope, orgID

	if out.Flows, err = parseList(q.Get("flows"), "flows", idPattern, maxAnalyticsListLen); err != nil {
		return out, err
	}
	if out.Severity, err = parseList(q.Get("severity"), "severity", regexp.MustCompile(`^(error|warning|info)$`), 3); err != nil {
		return out, err
	}
	if out.Category, err = parseList(q.Get("category"), "category", categoryPattern, maxAnalyticsCatLen); err != nil {
		return out, err
	}
	if out.Rule, err = parseList(q.Get("rule"), "rule", rulePattern, maxAnalyticsListLen); err != nil {
		return out, err
	}

	switch strings.ToLower(strings.TrimSpace(q.Get("compare"))) {
	case "", "1", "true":
		out.Compare = true
	case "0", "false":
		out.Compare = false
	default:
		return out, invalid("compare must be 1 or 0")
	}
	return out, nil
}

// daysBetween counts calendar days from a to b (both local midnights),
// immune to DST making a "day" 23 or 25 hours long.
func daysBetween(a, b time.Time) int {
	ua := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)
	ub := time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
	return int(ub.Sub(ua).Hours() / 24)
}
