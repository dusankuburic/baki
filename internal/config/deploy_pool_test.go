package config

import (
	"bytes"
	"go/scanner"
	"go/token"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These tests hold the deployment artefacts to the operations rules in docs/architecture/operations.md: the
// per-replica pool derived from the server's connection cap, a stop budget the platform does not cut short, and
// alerts that match the log lines they are written for.

// readDeployFile reads a deployment artefact (relative to the repository root) with LF line endings, so the
// checks hold on a CRLF checkout too.
func readDeployFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, strings.Split(rel, "/")...)...))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// TestDeployPool_DerivedFromTheServerCap evaluates main.bicep's pool derivation: with the server's
// max_connections known, each replica gets its share of what the reserve leaves, counting the rolling deploy's
// surge (the new revision's minReplicas, at least one, beside the old revision's maxReplicas), at least 1 (the app
// reads 0 as "unset" and opens its default) and at most the app default.
func TestDeployPool_DerivedFromTheServerCap(t *testing.T) {
	bicep := readDeployFile(t, "infra/main.bicep")
	if !regexp.MustCompile(`(?m)^param dbServerMaxConnections int = 0$`).MatchString(bicep) {
		t.Fatal("main.bicep must declare `param dbServerMaxConnections int = 0` (0 = unknown, the pool stays at the app default)")
	}
	ceiling := bicepInts{t: t, text: bicep}.value("dbAppDefaultMaxOpenConns")

	for _, tc := range []struct {
		name                                       string
		server, reserved, minReplicas, maxReplicas int
		want                                       int
	}{
		{"B1ms, one replica", 50, 20, 1, 1, 15},
		{"B1ms, up to three replicas", 50, 20, 1, 3, 7},
		{"B2s, one replica", 429, 20, 1, 1, ceiling},
		{"D2ds_v5 share above the app default", 859, 20, 1, 1, ceiling},
		{"three replicas always: three more mid-deploy", 100, 20, 3, 3, 13},
		{"scale to zero still starts one", 50, 20, 0, 1, 15},
		{"nothing left after the reserve", 20, 20, 1, 1, 1},
		{"cap below the reserve", 10, 20, 1, 2, 1},
		{"no reserve", 40, 0, 1, 1, 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := bicepInts{t: t, text: bicep, params: map[string]int{
				"dbServerMaxConnections": tc.server,
				"dbReservedConnections":  tc.reserved,
				"minReplicas":            tc.minReplicas,
				"maxReplicas":            tc.maxReplicas,
			}}.value("dbDerivedMaxOpenConns")
			if got != tc.want {
				t.Errorf("derived pool = %d, want %d", got, tc.want)
			}
			// The property the formula exists for: the old revision's replicas and the new one's, each with a
			// full pool, stay inside the server's cap less the reserve (whenever that leaves one connection each).
			pools := tc.maxReplicas + max(tc.minReplicas, 1)
			if room := tc.server - tc.reserved; room >= pools && got*pools > room {
				t.Errorf("%d pools mid-deploy × %d connections = %d, over the %d the server leaves", pools, got, got*pools, room)
			}
		})
	}
}

// TestDeployPool_ExplicitSizeWinsAndEveryKnobIsWired: an explicit dbMaxOpenConns still wins over the derivation,
// nothing is emitted while the server's cap is unknown, the container learns the replica count and the cap, and
// the CI deploy can set every knob.
func TestDeployPool_ExplicitSizeWinsAndEveryKnobIsWired(t *testing.T) {
	bicep := readDeployFile(t, "infra/main.bicep")
	for _, want := range []string{
		"var dbEffectiveMaxOpenConns = !empty(dbMaxOpenConns) ? dbMaxOpenConns : (dbServerMaxConnections > 0 ? string(dbDerivedMaxOpenConns) : '')",
		"empty(dbEffectiveMaxOpenConns) ? [] : [ { name: 'PAD_DB_MAX_OPEN_CONNS', value: dbEffectiveMaxOpenConns } ]",
		"dbServerMaxConnections > 0 ? [ { name: 'PAD_DB_SERVER_MAX_CONNS', value: string(dbServerMaxConnections) } ] : []",
		"{ name: 'PAD_MAX_REPLICAS', value: string(maxReplicas) }",
	} {
		if !strings.Contains(bicep, want) {
			t.Errorf("main.bicep lacks:\n\t%s", want)
		}
	}
	if strings.Contains(bicep, "value: dbMaxOpenConns }") {
		t.Error("main.bicep still emits dbMaxOpenConns directly; emit dbEffectiveMaxOpenConns so the derivation applies")
	}

	// deploy.yml: the int parameters must render a number even when the repository variable is unset (az refuses
	// an empty int), so they fall back to the template's own default.
	reserved := bicepInts{t: t, text: bicep}.value("dbReservedConnections")
	deploy := readDeployFile(t, ".github/workflows/deploy.yml")
	for _, want := range []string{
		`dbMaxOpenConns="${{ vars.PAD_DB_MAX_OPEN_CONNS }}"`,
		`dbServerMaxConnections=${{ vars.PAD_DB_SERVER_MAX_CONNS || 0 }}`,
		`dbReservedConnections=${{ vars.DB_RESERVED_CONNECTIONS || ` + strconv.Itoa(reserved) + ` }}`,
	} {
		if !strings.Contains(deploy, want) {
			t.Errorf("deploy.yml lacks (the fallback must equal main.bicep's default):\n\t%s", want)
		}
	}
}

// decidedStopTimeout is the app's fx.StopTimeout as decided in Phase 5a (ADR 0024's budget table). The test also
// reads the value from the wiring, so a later change there is checked as well.
const decidedStopTimeout = 28 * time.Second

// TestDeployStop_GraceOutlastsStopTimeout: the platform's SIGKILL must come after the app's own stop budget, on
// ACA (terminationGracePeriodSeconds) and in both compose files (stop_grace_period; Docker's default is 10 s).
func TestDeployStop_GraceOutlastsStopTimeout(t *testing.T) {
	stop := decidedStopTimeout
	if d, ok := appStopTimeout(t); ok && d > stop {
		stop = d
	}

	bicep := readDeployFile(t, "infra/main.bicep")
	if !strings.Contains(bicep, "terminationGracePeriodSeconds: stopGraceSeconds") {
		t.Error("main.bicep must set terminationGracePeriodSeconds from var stopGraceSeconds (the readiness alert uses it too)")
	}
	if grace := time.Duration(bicepInts{t: t, text: bicep}.value("stopGraceSeconds")) * time.Second; grace <= stop {
		t.Errorf("ACA grace period %v must be more than the app's StopTimeout %v", grace, stop)
	}

	for _, f := range []string{"docker-compose.yml", "docker-compose.prod.yml"} {
		raw := composeServiceKey(readDeployFile(t, f), "app", "stop_grace_period")
		if raw == "" {
			t.Errorf("%s: the app service sets no stop_grace_period (Docker kills it after 10 s)", f)
			continue
		}
		grace, err := time.ParseDuration(raw)
		if err != nil {
			t.Errorf("%s: stop_grace_period %q: %v", f, raw, err)
			continue
		}
		if grace < 30*time.Second || grace <= stop {
			t.Errorf("%s: stop_grace_period %v must be at least 30s and more than the app's StopTimeout %v", f, grace, stop)
		}
	}
}

// TestDeployAlerts_ReadinessToleratesAStoppingReplica: a stopping replica answers /readyz with 503 from the start
// of its drain and then stops listening, so it logs readiness failures until it exits. The readiness alert counts
// per replica and needs more failures than a stop can produce, and at least as many as take a replica out of
// rotation, for any probe settings.
func TestDeployAlerts_ReadinessToleratesAStoppingReplica(t *testing.T) {
	bicep := readDeployFile(t, "infra/main.bicep")
	for _, want := range []string{
		`| summarize failures = count() by replica = tostring(column_ifexists("ReplicaName_s", ""))`,
		`| where failures >= ${readinessAlertMinFailures}`,
	} {
		if !strings.Contains(bicep, want) {
			t.Errorf("the readiness alert query lacks:\n\t%s", want)
		}
	}
	grace := bicepInts{t: t, text: bicep}.value("stopGraceSeconds")
	for _, tc := range []struct{ threshold, period int }{{6, 10}, {1, 10}, {3, 5}, {10, 30}, {6, 1}, {1, 240}} {
		got := bicepInts{t: t, text: bicep, params: map[string]int{
			"readinessFailureThreshold": tc.threshold,
			"probePeriodSeconds":        tc.period,
		}}.value("readinessAlertMinFailures")
		// Probes at t, t+period, ... within the grace period: at most grace/period + 1 of them fail.
		if stopping := grace/tc.period + 1; got <= stopping {
			t.Errorf("threshold %d, period %ds: alert at %d failures, but a stopping replica can log %d", tc.threshold, tc.period, got, stopping)
		}
		if got < tc.threshold {
			t.Errorf("threshold %d, period %ds: alert at %d failures, below the %d that take a replica out of rotation", tc.threshold, tc.period, got, tc.threshold)
		}
	}
}

// alertedLogMessages are the fixed slog messages main.bicep alerts on (ADR 0023's table, ADR 0024's chat
// shutdown line). quietLogMessages are fixed messages of normal operation that must not page anyone.
var (
	alertedLogMessages = []string{
		"job overdue",
		"job lease lost",
		"advisory unlock reported not held; discarding connection",
		"advisory lease lost; discarding its session",
		"chat workers still running at shutdown",
	}
	quietLogMessages = []string{
		"job skipped: lock unavailable",
		"server draining",
	}
)

// TestDeployAlerts_MatchTheFixedLogMessages: every alerted message is still logged by the code, the KQL pattern
// main.bicep builds for it matches the JSON line slog writes, and the normal-operation messages have no alert.
func TestDeployAlerts_MatchTheFixedLogMessages(t *testing.T) {
	bicep := readDeployFile(t, "infra/main.bicep")
	// The pattern the alert loop renders for each message m (Bicep escapes the single quotes).
	const kqlPattern = `Log_s contains \'"msg":"${m}"\'`
	if !strings.Contains(bicep, kqlPattern) {
		t.Fatalf("main.bicep's log alerts must match the message with:\n\t%s", kqlPattern)
	}
	literals := goStringLiterals(t, filepath.Join("..", "..", "internal"))
	for _, msg := range alertedLogMessages {
		if !strings.Contains(bicep, "'"+msg+"'") {
			t.Errorf("main.bicep has no alert on %q", msg)
		}
		if !literals[msg] {
			t.Errorf("no Go file under internal/ logs %q any more: the alert on it would never fire", msg)
		}
		var line bytes.Buffer
		slog.New(slog.NewJSONHandler(&line, nil)).Warn(msg, "job", "scanner")
		// KQL's contains is case-insensitive; the JSON line keeps the message's case, so this is the same test.
		if pattern := `"msg":"` + msg + `"`; !strings.Contains(line.String(), pattern) {
			t.Errorf("slog writes %s, which the KQL pattern %s does not match", strings.TrimSpace(line.String()), pattern)
		}
	}
	for _, msg := range quietLogMessages {
		if strings.Contains(bicep, "'"+msg+"'") || strings.Contains(bicep, `"msg":"`+msg+`"`) {
			t.Errorf("main.bicep alerts on %q, a line of normal operation", msg)
		}
	}
}

// bicepInts evaluates integer parameters and variables of a Bicep template: the subset its derivations use
// (integer literals, other parameters and variables, + - * / with ARM's truncating division, parentheses, min and
// max). params override the template's values.
type bicepInts struct {
	t      *testing.T
	text   string
	params map[string]int
}

func (b bicepInts) value(name string) int {
	b.t.Helper()
	if v, ok := b.params[name]; ok {
		return v
	}
	decl := regexp.MustCompile(`(?m)^(?:var|param)\s+` + regexp.QuoteMeta(name) + `\s+(?:int\s+)?=\s*([^\n]+?)\s*(?://[^\n]*)?$`)
	m := decl.FindStringSubmatch(b.text)
	if m == nil {
		b.t.Fatalf("main.bicep declares no integer %s", name)
	}
	p := &intExprParser{b: b, src: m[1]}
	v := p.sum()
	if p.peek() != 0 {
		b.t.Fatalf("%s = %s: cannot evaluate from %q", name, m[1], p.src[p.pos:])
	}
	return v
}

type intExprParser struct {
	b   bicepInts
	src string
	pos int
}

func (p *intExprParser) peek() byte {
	for p.pos < len(p.src) && p.src[p.pos] == ' ' {
		p.pos++
	}
	if p.pos < len(p.src) {
		return p.src[p.pos]
	}
	return 0
}

func (p *intExprParser) expect(c byte) {
	if p.peek() != c {
		p.b.t.Fatalf("cannot evaluate %q: want %q at %d", p.src, c, p.pos)
	}
	p.pos++
}

func (p *intExprParser) sum() int {
	v := p.product()
	for {
		switch p.peek() {
		case '+':
			p.pos++
			v += p.product()
		case '-':
			p.pos++
			v -= p.product()
		default:
			return v
		}
	}
}

func (p *intExprParser) product() int {
	v := p.operand()
	for {
		switch p.peek() {
		case '*':
			p.pos++
			v *= p.operand()
		case '/':
			p.pos++
			d := p.operand()
			if d == 0 {
				p.b.t.Fatalf("cannot evaluate %q: division by zero", p.src)
			}
			v /= d // truncates toward zero, as ARM's div
		default:
			return v
		}
	}
}

func (p *intExprParser) operand() int {
	isIdent := func(c byte) bool {
		return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
	}
	switch c := p.peek(); {
	case c == '(':
		p.pos++
		v := p.sum()
		p.expect(')')
		return v
	case c == '-':
		p.pos++
		return -p.operand()
	case c >= '0' && c <= '9':
		start := p.pos
		for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
			p.pos++
		}
		n, _ := strconv.Atoi(p.src[start:p.pos])
		return n
	case isIdent(c):
		start := p.pos
		for p.pos < len(p.src) && isIdent(p.src[p.pos]) {
			p.pos++
		}
		name := p.src[start:p.pos]
		if p.peek() != '(' {
			return p.b.value(name)
		}
		p.pos++
		args := []int{p.sum()}
		for p.peek() == ',' {
			p.pos++
			args = append(args, p.sum())
		}
		p.expect(')')
		switch name {
		case "min":
			return slices.Min(args)
		case "max":
			return slices.Max(args)
		}
		p.b.t.Fatalf("cannot evaluate %q: function %s is not supported", p.src, name)
	}
	p.b.t.Fatalf("cannot evaluate %q at %d", p.src, p.pos)
	return 0
}

// composeServiceKey returns the value of key in a service of a compose file, "" when it is not set. The files are
// hand-formatted with two-space indentation, which this reads without a YAML dependency.
func composeServiceKey(text, service, key string) string {
	inServices, inService := false, false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		switch indent := len(line) - len(strings.TrimLeft(line, " ")); {
		case indent == 0:
			inServices, inService = trimmed == "services:", false
		case inServices && indent == 2:
			inService = trimmed == service+":"
		case inService && indent == 4 && strings.HasPrefix(trimmed, key+":"):
			return strings.TrimSpace(strings.TrimPrefix(trimmed, key+":"))
		}
	}
	return ""
}

// appStopTimeout reads fx.StopTimeout from the app's wiring (main.go, or internal/app after the split): a literal
// "N * time.Second", or a constant or variable defined that way. ok is false when there is no StopTimeout; a
// StopTimeout in another form fails the test, so this check cannot quietly stop applying.
func appStopTimeout(t *testing.T) (time.Duration, bool) {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join("..", "app", "*.go"))
	files = append(files, filepath.Join("..", "..", "main.go"))
	var src strings.Builder
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		if b, err := os.ReadFile(f); err == nil {
			src.Write(b)
			src.WriteByte('\n')
		}
	}
	call := regexp.MustCompile(`fx\.StopTimeout\(\s*([^)]*?)\s*\)`).FindStringSubmatch(src.String())
	if call == nil {
		return 0, false
	}
	seconds := regexp.MustCompile(`^(\d+)\s*\*\s*time\.Second$`)
	arg := call[1]
	if !seconds.MatchString(arg) {
		def := regexp.MustCompile(`\b` + regexp.QuoteMeta(arg) + `\s*=\s*(\d+\s*\*\s*time\.Second)\b`).FindStringSubmatch(src.String())
		if def == nil {
			t.Fatalf("fx.StopTimeout(%s): cannot read its value; extend appStopTimeout", arg)
		}
		arg = def[1]
	}
	n, _ := strconv.Atoi(seconds.FindStringSubmatch(arg)[1])
	return time.Duration(n) * time.Second, true
}

// goStringLiterals collects the string literals of the non-test Go files under root.
func goStringLiterals(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var s scanner.Scanner
		s.Init(token.NewFileSet().AddFile(path, -1, len(src)), src, nil, 0)
		for {
			_, tok, lit := s.Scan()
			if tok == token.EOF {
				break
			}
			if tok != token.STRING {
				continue
			}
			if v, err := strconv.Unquote(lit); err == nil {
				out[v] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}
