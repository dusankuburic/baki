package database

import (
	"context"
	"errors"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/storage"
	"pad-analyzer/internal/storage/interfaces"
)

// Key rotation (ADR 0005): values sealed with a key PAD_ENCRYPTION_KEY replaced stay readable while that key is
// listed in PAD_ENCRYPTION_KEY_PREVIOUS, writes seal with the current key, and the re-seal pass moves every value
// to the current key, after which the previous key can go. Postgres-backed tests are DATABASE_URL-gated.

const (
	rotOldKey     = "rotation-test-old-key-0123456789abcdefghij"
	rotNewKey     = "rotation-test-new-key-0123456789abcdefghij"
	rotUnknownKey = "rotation-test-unknown-key-0123456789abcdef"
)

// TestSealKeys_OpenTriesTheCurrentKeyThenThePreviousOnes: a value opens under the current key or any previous one,
// reports which, and opens under no other key; sealing always uses the current key.
func TestSealKeys_OpenTriesTheCurrentKeyThenThePreviousOnes(t *testing.T) {
	keys := func(current string, previous ...string) *sealKeys {
		t.Helper()
		var prev [][]byte
		for _, p := range previous {
			prev = append(prev, []byte(p))
		}
		k, err := newSealKeys([]byte(current), prev)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	aad := fieldAAD("org_channels", "url", "ch-1")
	underOld, err := keys(rotOldKey).seal([]byte("https://hooks.example.com/x"), aad)
	if err != nil {
		t.Fatal(err)
	}

	rotated := keys(rotNewKey, rotUnknownKey, rotOldKey)
	plain, current, err := rotated.open(underOld, aad)
	if err != nil || current || string(plain) != "https://hooks.example.com/x" {
		t.Fatalf("open under a previous key = %q, current %v, %v", plain, current, err)
	}
	if _, _, err := keys(rotNewKey).open(underOld, aad); err == nil {
		t.Error("the current key alone opened a value a replaced key sealed")
	}
	if _, _, err := rotated.open(underOld, fieldAAD("org_channels", "url", "ch-2")); err == nil {
		t.Error("a previous key opened a value moved to another row")
	}

	underNew, err := rotated.seal([]byte("v"), aad)
	if err != nil {
		t.Fatal(err)
	}
	if _, current, err := keys(rotNewKey).open(underNew, aad); err != nil || !current {
		t.Errorf("a write did not seal with the current key: current %v, %v", current, err)
	}
	if _, _, err := keys(rotOldKey).open(underNew, aad); err == nil {
		t.Error("a write sealed with a previous key")
	}
	if _, err := newSealKeys([]byte(rotNewKey), [][]byte{nil}); err == nil {
		t.Error("an empty previous key was accepted")
	}
}

// TestReseal_CoversEverySealedColumn: every column sealField writes and every keystore table holding a ciphertext
// is one the re-seal pass walks, so a new secret column cannot be forgotten by the rotation.
func TestReseal_CoversEverySealedColumn(t *testing.T) {
	covered := map[string]bool{}
	for _, c := range append(append([]sealedColumn{}, fieldSealedColumns...), keystoreSealedColumns...) {
		covered[c.table+"."+c.column] = true
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	sealCall := regexp.MustCompile(`sealField\(\s*("([a-z_]+)"|idempotencyTable)\s*,\s*"([a-z_]+)"`)
	keystoreWrite := regexp.MustCompile(`(?:INSERT INTO|UPDATE) ([a-z_]+)[^;]*?ciphertext`)
	found := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range sealCall.FindAllStringSubmatch(string(src), -1) {
			table := m[2]
			if m[1] == "idempotencyTable" {
				table = idempotencyTable
			}
			found++
			if !covered[table+"."+m[3]] {
				t.Errorf("%s seals %s.%s, which the re-seal pass does not walk (fieldSealedColumns)", name, table, m[3])
			}
		}
		if name == "keystore.go" {
			for _, m := range keystoreWrite.FindAllStringSubmatch(string(src), -1) {
				found++
				if !covered[m[1]+".ciphertext"] {
					t.Errorf("keystore.go writes %s.ciphertext, which the re-seal pass does not walk (keystoreSealedColumns)", m[1])
				}
			}
		}
	}
	if found < 6 {
		t.Fatalf("found %d sealed writes; did the patterns break?", found)
	}
}

// rotationReplica is another process on the same database with the given current and previous keys.
func rotationReplica(t *testing.T, dsn string, current string, previous ...string) *PostgresStorageBackend {
	t.Helper()
	b, err := New(context.Background(), DefaultConfig(dsn))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	var prev [][]byte
	for _, p := range previous {
		prev = append(prev, []byte(p))
	}
	if err := b.EnableFieldEncryption([]byte(current), prev...); err != nil {
		t.Fatal(err)
	}
	return b
}

// seedSealedUnderOldKey writes, with a process that has only the old key, a value into every column field_crypto.go
// seals: two channels (url and secret each), a CI credential and an idempotency body. It returns how many sealed
// values that is.
func seedSealedUnderOldKey(t *testing.T, b *PostgresStorageBackend) int {
	t.Helper()
	ctx := context.Background()
	if err := b.EnableFieldEncryption([]byte(rotOldKey)); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users (id, email, password, role) VALUES ('rot-user', 'rot@example.com', 'x', 'user')`,
		`INSERT INTO organisations (id, name, owner_id) VALUES ('rot-org', 'Rotation', 'rot-user'), ('rot-org-2', 'Other', 'rot-user')`,
	} {
		if _, err := b.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"rot-ch-1", "rot-ch-2"} {
		if err := b.SaveOrgChannel(ctx, &interfaces.OrgChannel{ID: id, OrgID: "rot-org", Name: id, Kind: "webhook",
			URL: "https://hooks.example.com/" + id, Secret: "hmac-" + id, Enabled: true, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.CreateCICredential(ctx, &interfaces.CICredential{ID: "rot-ci", OrgID: "rot-org", Name: "ci",
		Secret: "baki_ci_rot-ci_s3cr3t", SecretHint: "cr3t", CreatedBy: "rot-user"}, 10); err != nil {
		t.Fatal(err)
	}
	// Sealed as CompleteIdempotencyKey seals it; inserted directly, as there is no request transaction here.
	body, err := b.sealField(idempotencyTable, "body", idempotencyRowID("rot-user", "rot-key"), `{"token":"baki_pat_once"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.db.ExecContext(ctx, `INSERT INTO idempotency_keys (user_id, idem_key, fingerprint, status, content_type, body, expires_at)
		VALUES ('rot-user', 'rot-key', 'fp', 201, 'application/json', $1, NOW() + INTERVAL '1 hour')`, []byte(body)); err != nil {
		t.Fatal(err)
	}
	return 2*2 + 1 + 1
}

// readRotationValues opens every value seedSealedUnderOldKey wrote, through the readers the app uses.
func readRotationValues(ctx context.Context, b *PostgresStorageBackend) (map[string]string, error) {
	out := map[string]string{}
	chs, err := b.ListOrgChannels(ctx, "rot-org", false)
	if err != nil {
		return nil, err
	}
	for _, ch := range chs {
		if ch.Unreadable {
			// Listed without its values (ListOrgChannels no longer fails the list): no configured key opens them.
			return nil, errors.New("channel " + ch.ID + " is unreadable")
		}
		out[ch.ID+".url"], out[ch.ID+".secret"] = ch.URL, ch.Secret
	}
	c, err := b.GetCICredential(ctx, "rot-ci")
	if err != nil {
		return nil, err
	}
	out["rot-ci.secret"] = c.Secret
	var raw []byte
	if err := b.db.QueryRowContext(ctx, `SELECT body FROM idempotency_keys WHERE user_id = 'rot-user' AND idem_key = 'rot-key'`).Scan(&raw); err != nil {
		return nil, err
	}
	body, err := b.openField(idempotencyTable, "body", idempotencyRowID("rot-user", "rot-key"), string(raw))
	if err != nil {
		return nil, err
	}
	out["rot-key.body"] = body
	return out, nil
}

// rawRotationValues reads the stored (sealed) form of the same values.
func rawRotationValues(t *testing.T, b *PostgresStorageBackend) map[string]string {
	t.Helper()
	ctx := context.Background()
	out := map[string]string{}
	rows, err := b.db.QueryContext(ctx, `SELECT id, url, secret FROM org_channels WHERE org_id = 'rot-org'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, url, secret string
		if err := rows.Scan(&id, &url, &secret); err != nil {
			t.Fatal(err)
		}
		out[id+".url"], out[id+".secret"] = url, secret
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var ci string
	var body []byte
	if err := b.db.QueryRowContext(ctx, `SELECT secret FROM org_ci_credentials WHERE id = 'rot-ci'`).Scan(&ci); err != nil {
		t.Fatal(err)
	}
	if err := b.db.QueryRowContext(ctx, `SELECT body FROM idempotency_keys WHERE user_id = 'rot-user' AND idem_key = 'rot-key'`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	out["rot-ci.secret"], out["rot-key.body"] = ci, string(body)
	return out
}

// TestKeyRotation_PreviousKeyOpensAndTheResealConverges: after a rotation, a process with (new, previous old) reads
// every value the old key sealed; the re-seal pass moves each to the new key once (a second pass changes nothing),
// after which a process with the new key alone reads everything and one with the old key alone reads nothing. New
// writes seal with the new key.
func TestKeyRotation_PreviousKeyOpensAndTheResealConverges(t *testing.T) {
	b, dsn := openIsolatedMigrationDB(t)
	ctx := context.Background()
	n := seedSealedUnderOldKey(t, b)
	want, err := readRotationValues(ctx, b)
	if err != nil || len(want) != n {
		t.Fatalf("read under the old key: %d values, %v", len(want), err)
	}

	newOnly := rotationReplica(t, dsn, rotNewKey)
	if _, err := readRotationValues(ctx, newOnly); err == nil {
		t.Fatal("the new key alone opened values the old key sealed")
	}
	rotated := rotationReplica(t, dsn, rotNewKey, rotOldKey)
	if got, err := readRotationValues(ctx, rotated); err != nil || !equalMaps(got, want) {
		t.Fatalf("read with (new, previous old) = %v, %v; want %v", got, err, want)
	}

	before := rawRotationValues(t, b)
	// Pages of one value walk every keyset step, the two-column keys of the idempotency bodies included.
	rep, err := resealColumns(ctx, rotated.db, rotated.sealer.Load().keys, resealOptions{pageSize: 1}, fieldSealedColumns)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if rep.Checked != n || rep.Resealed != n || rep.Remaining != 0 || rep.Unreadable != 0 {
		t.Fatalf("first pass = %+v, want %d checked and resealed, nothing remaining or unreadable", rep, n)
	}
	after := rawRotationValues(t, b)
	for k, v := range after {
		if v == before[k] || !strings.HasPrefix(v, sealedPrefix) {
			t.Errorf("%s: stored %q after the pass (before %q), want a new enc:v1: value", k, v, before[k])
		}
	}
	if got, err := readRotationValues(ctx, newOnly); err != nil || !equalMaps(got, want) {
		t.Fatalf("after the pass, the new key alone read %v, %v; want %v", got, err, want)
	}
	if _, err := readRotationValues(ctx, b); err == nil {
		t.Error("the old key alone still opened values after the pass")
	}

	rep, err = rotated.ResealFields(ctx)
	if err != nil || rep.Checked != n || rep.Resealed != 0 || rep.Remaining != 0 || rep.Unreadable != 0 {
		t.Fatalf("second pass = %+v, %v; want %d checked and nothing else", rep, err, n)
	}
	if again := rawRotationValues(t, b); !equalMaps(again, after) {
		t.Error("a second pass rewrote values the first had moved")
	}

	if err := rotated.SaveOrgChannel(ctx, &interfaces.OrgChannel{ID: "rot-ch-new", OrgID: "rot-org-2", Name: "n", Kind: "webhook",
		URL: "https://hooks.example.com/new", Enabled: true, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if chs, err := newOnly.ListOrgChannels(ctx, "rot-org-2", false); err != nil || len(chs) != 1 || chs[0].URL != "https://hooks.example.com/new" {
		t.Errorf("a write with (new, previous old) is not readable with the new key alone: %+v, %v", chs, err)
	}
}

// TestKeyRotation_ResealResumesAfterAnInterruption: a pass stopped part-way (shutdown) leaves every value readable,
// some under each key, and the next pass moves the rest.
func TestKeyRotation_ResealResumesAfterAnInterruption(t *testing.T) {
	b, dsn := openIsolatedMigrationDB(t)
	n := seedSealedUnderOldKey(t, b)
	rotated := rotationReplica(t, dsn, rotNewKey, rotOldKey)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pages := 0
	rep, err := resealColumns(ctx, rotated.db, rotated.sealer.Load().keys, resealOptions{pageSize: 1, pageDone: func() {
		if pages++; pages == 2 {
			cancel()
		}
	}}, fieldSealedColumns)
	if !errors.Is(err, context.Canceled) || rep.Resealed != 2 {
		t.Fatalf("interrupted pass = %+v, %v; want 2 resealed and context.Canceled", rep, err)
	}
	if _, err := readRotationValues(context.Background(), rotated); err != nil {
		t.Fatalf("values unreadable after an interrupted pass: %v", err)
	}
	rep, err = rotated.ResealFields(context.Background())
	if err != nil || rep.Resealed != n-2 || rep.Checked != n {
		t.Fatalf("resumed pass = %+v, %v; want %d resealed of %d", rep, err, n-2, n)
	}
	if _, err := readRotationValues(context.Background(), rotationReplica(t, dsn, rotNewKey)); err != nil {
		t.Fatalf("after the resumed pass the new key alone does not read everything: %v", err)
	}
}

// TestKeyRotation_UnknownKeyIsCountedAndLeftAlone: a value no configured key opens is reported by table, column and
// row id, never by content, and left as stored; the pass moves everything else and does not fail.
func TestKeyRotation_UnknownKeyIsCountedAndLeftAlone(t *testing.T) {
	b, dsn := openIsolatedMigrationDB(t)
	ctx := context.Background()
	n := seedSealedUnderOldKey(t, b)
	stray := rotationReplica(t, dsn, rotUnknownKey)
	if err := stray.SaveOrgChannel(ctx, &interfaces.OrgChannel{ID: "rot-ch-stray", OrgID: "rot-org-2", Name: "s", Kind: "webhook",
		URL: "https://hooks.example.com/stray-credential", Secret: "stray-hmac", Enabled: true, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var strayURL, straySecret string
	if err := b.db.QueryRowContext(ctx, `SELECT url, secret FROM org_channels WHERE id = 'rot-ch-stray'`).Scan(&strayURL, &straySecret); err != nil {
		t.Fatal(err)
	}

	rotated := rotationReplica(t, dsn, rotNewKey, rotOldKey)
	rep, err := rotated.ResealFields(ctx)
	if err != nil {
		t.Fatalf("an unreadable value failed the pass: %v", err)
	}
	if rep.Resealed != n || rep.Unreadable != 2 || rep.Checked != n+2 {
		t.Fatalf("pass = %+v, want %d resealed and 2 unreadable of %d", rep, n, n+2)
	}
	wantRefs := []SealedValueRef{{"org_channels", "secret", "rot-ch-stray"}, {"org_channels", "url", "rot-ch-stray"}}
	got := append([]SealedValueRef(nil), rep.UnreadableRefs...)
	sort.Slice(got, func(i, j int) bool { return got[i].Column < got[j].Column })
	if len(got) != 2 || got[0] != wantRefs[0] || got[1] != wantRefs[1] {
		t.Errorf("unreadable refs = %+v, want %+v", got, wantRefs)
	}
	for _, r := range rep.UnreadableRefs {
		if s := r.Table + r.Column + r.RowID; strings.Contains(s, "stray-credential") || strings.Contains(s, "stray-hmac") || strings.Contains(s, sealedPrefix) {
			t.Errorf("a ref carries the value: %+v", r)
		}
	}
	var url, secret string
	if err := b.db.QueryRowContext(ctx, `SELECT url, secret FROM org_channels WHERE id = 'rot-ch-stray'`).Scan(&url, &secret); err != nil {
		t.Fatal(err)
	}
	if url != strayURL || secret != straySecret {
		t.Error("the pass rewrote a value no configured key opens")
	}
	if _, err := readRotationValues(ctx, rotationReplica(t, dsn, rotNewKey)); err != nil {
		t.Errorf("the readable values were not all moved: %v", err)
	}
}

// TestKeyRotation_ConcurrentPassesDoNotDoubleWrite: two replicas running the pass at once (the advisory lock
// unavailable) move each value exactly once between them, and a replica that read a value before the other moved it
// does not write over the moved value.
func TestKeyRotation_ConcurrentPassesDoNotDoubleWrite(t *testing.T) {
	b, dsn := openIsolatedMigrationDB(t)
	ctx := context.Background()
	n := seedSealedUnderOldKey(t, b)
	stale := rawRotationValues(t, b)["rot-ch-1.url"]
	r1 := rotationReplica(t, dsn, rotNewKey, rotOldKey)
	r2 := rotationReplica(t, dsn, rotNewKey, rotOldKey)

	var wg sync.WaitGroup
	reps := make([]ResealReport, 2)
	errs := make([]error, 2)
	start := make(chan struct{})
	for i, r := range []*PostgresStorageBackend{r1, r2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			reps[i], errs[i] = r.ResealFields(ctx)
		}()
	}
	close(start)
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("passes failed: %v, %v", errs[0], errs[1])
	}
	if got := reps[0].Resealed + reps[1].Resealed; got != n || reps[0].Remaining+reps[1].Remaining != 0 {
		t.Fatalf("two passes resealed %d values between them (%+v, %+v), want each of %d once", got, reps[0], reps[1], n)
	}
	moved := rawRotationValues(t, b)

	// The second replica's view from before the first one moved the value: its compare-and-set finds the value
	// changed, reads it again and leaves it.
	p := &resealPass{db: r2.db, keys: r2.sealer.Load().keys}
	if err := p.resealValue(ctx, &fieldSealedColumns[0], []string{"rot-ch-1"}, []byte(stale)); err != nil {
		t.Fatal(err)
	}
	if p.report.Resealed != 0 || p.report.Checked != 1 {
		t.Errorf("a stale read was rewritten: %+v", p.report)
	}
	if now := rawRotationValues(t, b); now["rot-ch-1.url"] != moved["rot-ch-1.url"] {
		t.Error("a stale read overwrote the moved value")
	}
	if _, err := readRotationValues(ctx, rotationReplica(t, dsn, rotNewKey)); err != nil {
		t.Errorf("after both passes the new key alone does not read everything: %v", err)
	}
}

// TestKeyRotation_Keystore: provider keys and organisation credentials sealed with the old key open with (new,
// previous old); the pass moves them without touching updated_at (the compare-and-swap token of org edits and the
// orphan sweep's clock), reports a row no key opens, and afterwards the new key alone reads them.
func TestKeyRotation_Keystore(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	if _, err := b.db.ExecContext(ctx, `INSERT INTO organisations (id, name, owner_id) VALUES ('rot-org', 'Rotation', 'rot-user')`); err != nil {
		t.Fatal(err)
	}
	keystore := func(current string, previous ...string) *EncryptedKeyStore {
		t.Helper()
		var prev [][]byte
		for _, p := range previous {
			prev = append(prev, []byte(p))
		}
		ks, err := b.NewEncryptedKeyStore([]byte(current), prev...)
		if err != nil {
			t.Fatal(err)
		}
		return ks
	}
	old := keystore(rotOldKey)
	if err := old.Save("rot-user", "openai", "sk-old-key"); err != nil {
		t.Fatal(err)
	}
	if err := old.SaveOrg("rot-org", "azure-foundry", `{"apiKey":"org-doc"}`, "rot-user"); err != nil {
		t.Fatal(err)
	}
	if err := keystore(rotUnknownKey).Save("rot-user", "anthropic", "sk-stray"); err != nil {
		t.Fatal(err)
	}
	stamps := func() (user, org time.Time) {
		t.Helper()
		if err := b.db.QueryRowContext(ctx, `SELECT updated_at FROM provider_keys WHERE user_id = 'rot-user' AND provider = 'openai'`).Scan(&user); err != nil {
			t.Fatal(err)
		}
		if err := b.db.QueryRowContext(ctx, `SELECT updated_at FROM org_provider_keys WHERE org_id = 'rot-org'`).Scan(&org); err != nil {
			t.Fatal(err)
		}
		return user, org
	}
	userAt, orgAt := stamps()

	newOnly := keystore(rotNewKey)
	if _, err := newOnly.Get("rot-user", "openai"); !errors.Is(err, storage.ErrSecretDecryptFailed) {
		t.Fatalf("new key alone: Get = %v, want ErrSecretDecryptFailed", err)
	}
	rotated := keystore(rotNewKey, rotOldKey)
	if got, err := rotated.Get("rot-user", "openai"); err != nil || got != "sk-old-key" {
		t.Fatalf("Get with (new, previous old) = %q, %v", got, err)
	}
	if got, err := rotated.GetOrg("rot-org", "azure-foundry"); err != nil || got != `{"apiKey":"org-doc"}` {
		t.Fatalf("GetOrg with (new, previous old) = %q, %v", got, err)
	}

	// Pages of one row walk the two-column keyset (provider_keys holds two rows).
	rep, err := resealColumns(ctx, b.db, rotated.keys, resealOptions{pageSize: 1}, keystoreSealedColumns)
	if err != nil {
		t.Fatalf("first keystore pass: %v", err)
	}
	if rep.Checked != 3 || rep.Resealed != 2 || rep.Unreadable != 1 || len(rep.UnreadableRefs) != 1 ||
		rep.UnreadableRefs[0] != (SealedValueRef{"provider_keys", "ciphertext", "rot-user/anthropic"}) {
		t.Fatalf("keystore pass = %+v, want 2 of 3 resealed and rot-user/anthropic unreadable", rep)
	}
	if u, o := stamps(); !u.Equal(userAt) || !o.Equal(orgAt) {
		t.Errorf("the pass moved updated_at: user %v → %v, org %v → %v", userAt, u, orgAt, o)
	}
	if got, err := newOnly.Get("rot-user", "openai"); err != nil || got != "sk-old-key" {
		t.Errorf("after the pass, the new key alone: Get = %q, %v", got, err)
	}
	if got, err := newOnly.GetOrg("rot-org", "azure-foundry"); err != nil || got != `{"apiKey":"org-doc"}` {
		t.Errorf("after the pass, the new key alone: GetOrg = %q, %v", got, err)
	}
	if rep, err := rotated.ResealKeys(ctx); err != nil || rep.Resealed != 0 || rep.Unreadable != 1 || rep.Checked != 3 {
		t.Errorf("second keystore pass = %+v, %v; want nothing moved", rep, err)
	}

	if err := rotated.Save("rot-user", "openai", "sk-new-key"); err != nil {
		t.Fatal(err)
	}
	if got, err := newOnly.Get("rot-user", "openai"); err != nil || got != "sk-new-key" {
		t.Errorf("a keystore write with (new, previous old) is not readable with the new key alone: %q, %v", got, err)
	}
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// TestKeyRotation_RestoredSnapshotGoesBackUnderTheCurrentKey: an org credential snapshot taken before the pass moved
// its row, put back after it (a credential change whose registry write failed), is written under the current key,
// so the restore does not leave a value only the previous key opens.
func TestKeyRotation_RestoredSnapshotGoesBackUnderTheCurrentKey(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	if _, err := b.db.ExecContext(ctx, `INSERT INTO organisations (id, name, owner_id) VALUES ('rot-org', 'Rotation', 'rot-user')`); err != nil {
		t.Fatal(err)
	}
	old, err := b.NewEncryptedKeyStore([]byte(rotOldKey))
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := b.NewEncryptedKeyStore([]byte(rotNewKey), []byte(rotOldKey))
	if err != nil {
		t.Fatal(err)
	}
	newOnly, err := b.NewEncryptedKeyStore([]byte(rotNewKey))
	if err != nil {
		t.Fatal(err)
	}
	const provider = "azure-foundry/abcdefghij"
	if err := old.SaveOrg("rot-org", provider, `{"doc":"before"}`, "rot-user"); err != nil {
		t.Fatal(err)
	}
	snap, ok, err := rotated.SnapshotOrg("rot-org", provider)
	if err != nil || !ok {
		t.Fatalf("SnapshotOrg: %v %v", ok, err)
	}
	if _, err := rotated.ResealKeys(ctx); err != nil {
		t.Fatal(err)
	}
	savedAt, ok, err := rotated.SaveOrgIfUnchanged("rot-org", provider, `{"doc":"after"}`, "rot-user", false, snap.UpdatedAt)
	if err != nil || !ok {
		t.Fatalf("SaveOrgIfUnchanged: %v %v", ok, err)
	}
	if restored, err := rotated.RestoreOrgIfUnchanged("rot-org", provider, savedAt, &snap); err != nil || !restored {
		t.Fatalf("RestoreOrgIfUnchanged: %v %v", restored, err)
	}
	if got, err := newOnly.GetOrg("rot-org", provider); err != nil || got != `{"doc":"before"}` {
		t.Errorf("after the restore, the new key alone reads %q, %v; want the restored document", got, err)
	}
	if meta, ok, err := rotated.OrgKeyMeta("rot-org", provider); err != nil || !ok || !meta.UpdatedAt.Equal(snap.UpdatedAt) {
		t.Errorf("the restore did not put the snapshot's stamp back: %+v %v %v", meta, ok, err)
	}
}
