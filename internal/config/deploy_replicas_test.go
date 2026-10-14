package config

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// These tests hold the deployment artefacts to the rules for running more than one replica
// (docs/architecture/operations.md, "Running more than one replica"): the repository ships one replica, more than
// one cannot ship without Redis, the Redis the template provisions fits the keys the app writes, each replica names
// itself, and the local overlay and the CI job that prove the mechanisms exist. They parse the files as text, like
// deploy_pool_test.go, so they need no YAML or Bicep toolchain.

// TestDeployReplicas_RepoDefaultsStayAtOne pins Phase 5b's decision 13: the repository ships one replica with no
// Redis, in the template, the example parameters and the deploy workflow. The capability to run more is a repository
// variable away; raising these defaults is a deliberate change made once the two-replica CI job has been green, and
// it changes this test with it.
func TestDeployReplicas_RepoDefaultsStayAtOne(t *testing.T) {
	bicep := readDeployFile(t, "infra/main.bicep")
	for _, want := range []string{
		"param minReplicas int = 1\n",
		"param maxReplicas int = 1\n",
		"param provisionRedis bool = false\n",
		"param scaleConcurrentRequests int = 100\n",
	} {
		if !strings.Contains(bicep, want) {
			t.Errorf("main.bicep lacks %q", strings.TrimSpace(want))
		}
	}

	params := readDeployFile(t, "infra/main.parameters.json")
	for name, want := range map[string]string{"minReplicas": "1", "maxReplicas": "1", "provisionRedis": "false", "scaleConcurrentRequests": "100"} {
		got := regexp.MustCompile(`"` + name + `":\s*\{\s*"value":\s*([^\s}]+)\s*\}`).FindStringSubmatch(params)
		if got == nil || got[1] != want {
			t.Errorf("main.parameters.json %s = %v, want %s", name, got, want)
		}
	}

	deploy := readDeployFile(t, ".github/workflows/deploy.yml")
	for _, want := range []string{
		"minReplicas=${{ vars.PAD_MIN_REPLICAS || 1 }}",
		"maxReplicas=${{ vars.PAD_MAX_REPLICAS || 1 }}",
		"provisionRedis=${{ vars.PAD_PROVISION_REDIS == 'true' }}",
		"scaleConcurrentRequests=${{ vars.PAD_SCALE_CONCURRENT_REQUESTS || 100 }}",
	} {
		if !strings.Contains(deploy, want) {
			t.Errorf("deploy.yml lacks (the fallback is the repository default):\n\t%s", want)
		}
	}
}

// TestDeployReplicas_ManyReplicasCannotShipWithoutRedis: a literal maxReplicas above 1 anywhere in the shipped files
// needs provisionRedis true beside it, and the deploy workflow refuses the variable form before it deploys. The app
// itself refuses to start with PAD_MAX_REPLICAS above 1 and no PAD_REDIS_URL (config.Validate), and the template
// passes the one only with the other (PAD_REDIS_URL follows provisionRedis).
func TestDeployReplicas_ManyReplicasCannotShipWithoutRedis(t *testing.T) {
	params := readDeployFile(t, "infra/main.parameters.json")
	maxN := intAfter(t, params, `"maxReplicas":\s*\{\s*"value":\s*(\d+)`)
	if maxN > 1 && !regexp.MustCompile(`"provisionRedis":\s*\{\s*"value":\s*true`).MatchString(params) {
		t.Errorf("main.parameters.json sets maxReplicas %d without provisionRedis true", maxN)
	}

	deploy := readDeployFile(t, ".github/workflows/deploy.yml")
	// An inline literal.
	if m := regexp.MustCompile(`(?m)^\s*maxReplicas=(\d+)\s*$`).FindStringSubmatch(deploy); m != nil {
		if n, _ := strconv.Atoi(m[1]); n > 1 && !regexp.MustCompile(`(?m)^\s*provisionRedis=true\s*$`).MatchString(deploy) {
			t.Errorf("deploy.yml deploys maxReplicas=%d without provisionRedis=true", n)
		}
	}
	// The variable form is refused by a step that runs before the deploy, with the check spelled the same way the
	// app's own refusal is: above one replica, Redis must be on.
	guard := strings.Index(deploy, "- name: Refuse several replicas without Redis")
	deployStep := strings.Index(deploy, "- name: Deploy infrastructure")
	if guard < 0 || deployStep < 0 || guard > deployStep {
		t.Fatal("deploy.yml must refuse several replicas without Redis in a step before 'Deploy infrastructure'")
	}
	step := deploy[guard:deployStep]
	for _, want := range []string{
		"MAX_REPLICAS: ${{ vars.PAD_MAX_REPLICAS || 1 }}",
		"PROVISION_REDIS: ${{ vars.PAD_PROVISION_REDIS == 'true' }}",
		`[ "$MAX_REPLICAS" -gt 1 ] && [ "$PROVISION_REDIS" != true ]`,
		"exit 1",
	} {
		if !strings.Contains(step, want) {
			t.Errorf("the replica guard step lacks %q", want)
		}
	}

	bicep := readDeployFile(t, "infra/main.bicep")
	for _, want := range []string{
		"provisionRedis ? [ { name: 'PAD_REDIS_URL', secretRef: 'redis-url' } ] : []",
		"{ name: 'PAD_MAX_REPLICAS', value: string(maxReplicas) }",
	} {
		if !strings.Contains(bicep, want) {
			t.Errorf("main.bicep lacks %q", want)
		}
	}
}

// intAfter returns the integer captured by the first match of re in text.
func intAfter(t *testing.T, text, re string) int {
	t.Helper()
	m := regexp.MustCompile(re).FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no match for %s", re)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("%s: %v", re, err)
	}
	return n
}

// TestDeployRedis_VolatileLRUNeedsEveryKeyToExpire: the managed Redis evicts only keys that expire, which is safe
// exactly because every key the app writes has a TTL. The registry golden is where each key's TTL is declared
// (internal/redisx.Declare refuses a key without one); this reads it, so a key declared without a TTL fails here and
// not under memory pressure in production. Channels hold no data and carry none.
func TestDeployRedis_VolatileLRUNeedsEveryKeyToExpire(t *testing.T) {
	bicep := readDeployFile(t, "infra/main.bicep")
	if !strings.Contains(bicep, "'maxmemory-policy': 'volatile-lru'") {
		t.Error("main.bicep must run Redis with maxmemory-policy volatile-lru")
	}
	if strings.Contains(bicep, "allkeys-lru") && !strings.Contains(bicep, "allkeys-lru would") {
		t.Error("main.bicep still configures allkeys-lru")
	}

	golden := readDeployFile(t, "internal/di/testdata/redis_keys.golden")
	keys := 0
	for _, line := range strings.Split(strings.TrimSpace(golden), "\n") {
		parts := strings.Split(line, " | ")
		if len(parts) < 4 {
			t.Errorf("redis_keys.golden: cannot read %q", line)
			continue
		}
		name, kind, ttl := parts[0], parts[1], parts[3]
		if kind == "pubsub" {
			continue
		}
		keys++
		if ttl == "ttl: none" {
			t.Errorf("redis key %s (%s) has no TTL: under volatile-lru it would never be evicted", name, kind)
		}
	}
	if keys == 0 {
		t.Error("redis_keys.golden declares no keys: the check read nothing")
	}

	// The overlay and the template run the same policy.
	overlay := readDeployFile(t, "docker-compose.replicas.yml")
	if !strings.Contains(overlay, `"--maxmemory-policy", "volatile-lru"`) {
		t.Error("docker-compose.replicas.yml must run Redis with --maxmemory-policy volatile-lru")
	}
}

// TestDeployRedis_SkuFollowsTheReplicaCount: with several replicas Redis is a Standard cache (a replica node and an
// SLA), with one it stays Basic so a deployment that only turns Redis on is not repriced; both can be set.
func TestDeployRedis_SkuFollowsTheReplicaCount(t *testing.T) {
	bicep := readDeployFile(t, "infra/main.bicep")
	for _, want := range []string{
		"param redisSku string = ''\n",
		"param redisCapacity int = -1\n",
		"var redisEffectiveSku = !empty(redisSku) ? redisSku : (maxReplicas > 1 ? 'Standard' : 'Basic')",
		"var redisEffectiveCapacity = redisCapacity >= 0 ? redisCapacity : (redisEffectiveSku == 'Basic' ? 0 : 1)",
		"sku: { name: redisEffectiveSku, family: redisFamily, capacity: redisEffectiveCapacity }",
		"output redisHostName string = provisionRedis ? redis!.properties.hostName : ''",
	} {
		if !strings.Contains(bicep, want) {
			t.Errorf("main.bicep lacks %q", strings.TrimSpace(want))
		}
	}
}

// TestDeployScale_ARuleExistsWhereTheCountCanMove: ACA never scales between min and max without a rule, so raising
// maxReplicas alone would change nothing. The rule is emitted exactly when there is room to scale.
func TestDeployScale_ARuleExistsWhereTheCountCanMove(t *testing.T) {
	bicep := readDeployFile(t, "infra/main.bicep")
	for _, want := range []string{
		"rules: maxReplicas > minReplicas ? [",
		"http: { metadata: { concurrentRequests: string(scaleConcurrentRequests) } }",
	} {
		if !strings.Contains(bicep, want) {
			t.Errorf("main.bicep lacks %q", want)
		}
	}
}

// TestDeployInstance_EachReplicaNamesItself: stream commands are routed by the replica's instance id, so two
// replicas must not share one. The template sets none (ACA's replica name is the fallback, see resolveInstanceID's
// test); the overlay sets a distinct one per replica.
func TestDeployInstance_EachReplicaNamesItself(t *testing.T) {
	bicep := readDeployFile(t, "infra/main.bicep")
	if strings.Contains(bicep, "name: 'PAD_INSTANCE_ID'") {
		t.Error("main.bicep sets PAD_INSTANCE_ID: one value would be the name of every replica; leave it to CONTAINER_APP_REPLICA_NAME")
	}

	overlay := readDeployFile(t, "docker-compose.replicas.yml")
	ids := map[string]string{}
	for _, svc := range []string{"app-a", "app-b"} {
		block := composeServiceBlock(overlay, svc)
		m := regexp.MustCompile(`(?m)^\s*-\s*PAD_INSTANCE_ID=(\S+)\s*$`).FindStringSubmatch(block)
		if m == nil {
			t.Errorf("docker-compose.replicas.yml: %s sets no PAD_INSTANCE_ID", svc)
			continue
		}
		ids[svc] = m[1]
	}
	if len(ids) == 2 && ids["app-a"] == ids["app-b"] {
		t.Errorf("both replicas are %q", ids["app-a"])
	}
}

// TestDeployAlerts_RedisAndReplicaAlertsMatchTheirSignals: the alerts a multi-replica deployment adds read log
// lines the code writes, so each message must still be logged (TestDeployAlerts_MatchTheFixedLogMessages does that
// for the lines every deployment alerts on), and exist only where they can fire.
func TestDeployAlerts_RedisAndReplicaAlertsMatchTheirSignals(t *testing.T) {
	bicep := readDeployFile(t, "infra/main.bicep")
	literals := goStringLiterals(t, filepath.Join("..", "..", "internal"))
	for _, msg := range []string{
		"redis degraded: the cross-replica mechanisms run in their on-down mode until it answers",
		"redis recovered",
		"invalidation publish failed; the peers keep their caches until the next resync or their TTL",
		"invalidation subscription lost; resubscribing",
	} {
		if !strings.Contains(bicep, `"msg":"`+msg+`"`) {
			t.Errorf("main.bicep has no alert on %q", msg)
		}
		if !literals[msg] {
			t.Errorf("no Go file under internal/ logs %q any more: the alert on it would never fire", msg)
		}
	}
	for _, want := range []string{
		"var redisAlerts = !provisionRedis ? [] : [",
		"var replicaAlerts = maxReplicas <= 1 ? [] : concat(minReplicas < maxReplicas ? replicaCeilingAlert : [], replicaFloorAlert)",
		"resource redisAlert 'Microsoft.Insights/scheduledQueryRules@2023-03-15-preview' = [for alert in redisAlerts: if (!empty(alertEmails)) {",
		"resource replicaAlert 'Microsoft.Insights/metricAlerts@2018-03-01' = [for alert in replicaAlerts: if (!empty(alertEmails)) {",
		"metricName: 'Replicas'",
	} {
		if !strings.Contains(bicep, want) {
			t.Errorf("main.bicep lacks %q", want)
		}
	}
}

// TestDeployCompose_ReplicasOverlayAndItsCIJob: the overlay the smoke job runs has the shape the scenarios rely on
// (two named replicas on distinct ports with Redis and PAD_MAX_REPLICAS, a balancer, no replica of the base file),
// the script exists, and the CI workflow runs it.
func TestDeployCompose_ReplicasOverlayAndItsCIJob(t *testing.T) {
	overlay := readDeployFile(t, "docker-compose.replicas.yml")
	for _, svc := range []string{"app-a", "app-b"} {
		block := composeServiceBlock(overlay, svc)
		for _, want := range []string{"PAD_MAX_REPLICAS=2", "PAD_REDIS_URL=redis://redis:6379", "service: app"} {
			if !strings.Contains(block, want) {
				t.Errorf("docker-compose.replicas.yml: %s lacks %q", svc, want)
			}
		}
		if !strings.Contains(block, "condition: service_healthy") || !strings.Contains(block, "redis:") {
			t.Errorf("docker-compose.replicas.yml: %s must wait for a healthy db and redis", svc)
		}
	}
	for _, port := range []string{`"8081:8080"`, `"8082:8080"`} {
		if !strings.Contains(overlay, port) {
			t.Errorf("docker-compose.replicas.yml publishes no %s", port)
		}
	}
	if !strings.Contains(composeServiceBlock(overlay, "app"), `profiles: ["single-replica"]`) {
		t.Error("docker-compose.replicas.yml must switch the base file's single app off")
	}
	lb := composeServiceBlock(overlay, "lb")
	if lb == "" || !strings.Contains(overlay, "lb_policy round_robin") || !strings.Contains(overlay, "flush_interval -1") {
		t.Error("docker-compose.replicas.yml needs a round-robin balancer that flushes event streams")
	}

	script := filepath.Join("..", "..", "scripts", "ci", "two-replica-smoke.sh")
	info, err := os.Stat(script)
	if err != nil {
		t.Fatalf("the smoke script: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
		t.Error("scripts/ci/two-replica-smoke.sh is not executable")
	}

	ci := readDeployFile(t, ".github/workflows/ci.yml")
	i := strings.Index(ci, "\n  two-replica:\n")
	if i < 0 {
		t.Fatal("ci.yml has no two-replica job")
	}
	job := ci[i+1:]
	if next := regexp.MustCompile(`(?m)^  [a-z][a-z-]*:\n`).FindStringIndex(job[len("  two-replica:\n"):]); next != nil {
		job = job[:len("  two-replica:\n")+next[0]]
	}
	for _, want := range []string{"docker-compose.replicas.yml", "scripts/ci/two-replica-smoke.sh", "baki-backend:replicas", "down -v"} {
		if !strings.Contains(job, want) {
			t.Errorf("the two-replica job lacks %q", want)
		}
	}
	// The overlay and the job agree on the image's name.
	if !strings.Contains(overlay, "image: baki-backend:replicas") {
		t.Error("docker-compose.replicas.yml must run the image the CI job builds, baki-backend:replicas")
	}
}

// composeServiceBlock returns the lines of one top-level service of a compose file (everything indented below its
// name), "" when there is none. Same hand-formatted, two-space layout composeServiceKey reads.
func composeServiceBlock(text, service string) string {
	var out []string
	inServices, inService := false, false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " "))
		switch {
		case trimmed == "" || strings.HasPrefix(trimmed, "#"):
			if inService {
				out = append(out, line)
			}
		case indent == 0:
			inServices, inService = trimmed == "services:", false
		case inServices && indent == 2:
			inService = trimmed == service+":"
			if inService {
				out = append(out, line)
			}
		case inService:
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
