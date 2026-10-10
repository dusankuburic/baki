package database

import (
	"context"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Key rotation (ADR 0005). Every sealed value, the secret columns of field_crypto.go and the keystore's ciphertexts,
// is AES-256-GCM under a key derived from PAD_ENCRYPTION_KEY. A rotation lists the replaced keys in
// PAD_ENCRYPTION_KEY_PREVIOUS: reads try the current key and then each previous one, writes seal with the current
// key, and the re-seal pass moves what a previous key sealed to the current one, after which the previous keys can
// go. No key identifier is stored. GCM authentication tells which key fits, and the stored formats ("enc:v1:", the
// keystore's bare base64) stay the ones the release before reads, so a rollback reads every value its key sealed.

// sealKeys are the current key, which seals and opens, and the keys it replaced, newest first, which only open.
type sealKeys struct {
	current  cipher.AEAD
	previous []cipher.AEAD
}

var errCiphertextTooShort = errors.New("ciphertext too short")

// newSealKeys derives the AEADs of the current key and the previous ones, each as the keystore derives its key.
func newSealKeys(current []byte, previous [][]byte) (*sealKeys, error) {
	aead, err := newKeystoreAEAD(current)
	if err != nil {
		return nil, err
	}
	k := &sealKeys{current: aead}
	for i, p := range previous {
		prev, err := newKeystoreAEAD(p)
		if err != nil {
			return nil, fmt.Errorf("previous key %d: %w", i+1, err)
		}
		k.previous = append(k.previous, prev)
	}
	return k, nil
}

// seal encrypts plaintext with the current key and a fresh random nonce, bound to aad: nonce || ciphertext.
func (k *sealKeys) seal(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, k.current.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	return k.current.Seal(nonce, nonce, plaintext, aad), nil
}

// open reverses seal with the first key that authenticates raw, the current key first, and reports whether it was
// the current key. A wrong key fails GCM's tag check after one pass over the value, which costs what a successful
// open does (well under a microsecond for a channel URL, about 0.15 ms for the largest stored response, 256 KiB), so
// during a rotation a read costs at most one failed open per previous key ahead of the right one, and none once the
// re-seal pass has run.
func (k *sealKeys) open(raw, aad []byte) (plain []byte, current bool, err error) {
	ns := k.current.NonceSize()
	if len(raw) < ns {
		return nil, false, errCiphertextTooShort
	}
	nonce, ct := raw[:ns], raw[ns:]
	if plain, err = k.current.Open(nil, nonce, ct, aad); err == nil {
		return plain, true, nil
	}
	for _, prev := range k.previous {
		if p, perr := prev.Open(nil, nonce, ct, aad); perr == nil {
			return p, false, nil
		}
	}
	return nil, false, err
}

// ResealReport counts what a re-seal pass found, per value: a channel's url and secret are two.
type ResealReport struct {
	// Checked is every sealed value the pass read.
	Checked int
	// Resealed were sealed with a previous key and now are with the current one.
	Resealed int
	// Remaining are still sealed with a previous key: written again under it while the pass ran, by a replica still
	// on the configuration before the rotation. The next pass moves them.
	Remaining int
	// Unreadable opened with no configured key and were left as stored. UnreadableRefs names the first of them.
	Unreadable     int
	UnreadableRefs []SealedValueRef
}

// SealedValueRef names a sealed value by where it is stored, never by its content.
type SealedValueRef struct{ Table, Column, RowID string }

// maxUnreadableRefs bounds the refs a report carries (and the log line prints); Unreadable counts them all.
const maxUnreadableRefs = 20

// Add folds o into r.
func (r *ResealReport) Add(o ResealReport) {
	r.Checked += o.Checked
	r.Resealed += o.Resealed
	r.Remaining += o.Remaining
	r.Unreadable += o.Unreadable
	for _, ref := range o.UnreadableRefs {
		if len(r.UnreadableRefs) < maxUnreadableRefs {
			r.UnreadableRefs = append(r.UnreadableRefs, ref)
		}
	}
}

// sealedColumn is one column of sealed values the re-seal pass walks, keyset-paged by the table's primary key.
type sealedColumn struct {
	table, column string
	keys          []string // the primary key, in order
	// prefixed values carry sealedPrefix (field_crypto.go); a value without it is plaintext the at-rest backfill
	// seals. The keystore stores bare base64.
	prefixed bool
	bytea    bool
	// live narrows the walk to rows still read: an expired idempotency record never answers again.
	live string
	// pageSize bounds the values held at once; 0 is defaultResealPageSize.
	pageSize int
	aad      func(key []string) []byte
}

const defaultResealPageSize = 500

// fieldSealedColumns are the columns sealField writes (TestReseal_CoversEverySealedColumn keeps the list whole).
var fieldSealedColumns = []sealedColumn{
	{table: "org_channels", column: "url", keys: []string{"id"}, prefixed: true,
		aad: func(k []string) []byte { return fieldAAD("org_channels", "url", k[0]) }},
	{table: "org_channels", column: "secret", keys: []string{"id"}, prefixed: true,
		aad: func(k []string) []byte { return fieldAAD("org_channels", "secret", k[0]) }},
	{table: "org_channels", column: "secrets", keys: []string{"id"}, prefixed: true,
		aad: func(k []string) []byte { return fieldAAD("org_channels", "secrets", k[0]) }},
	{table: "org_ci_credentials", column: "secret", keys: []string{"id"}, prefixed: true,
		aad: func(k []string) []byte { return fieldAAD("org_ci_credentials", "secret", k[0]) }},
	// A stored response is at most 256 KiB, so a page holds at most 16 MiB.
	{table: idempotencyTable, column: "body", keys: []string{"user_id", "idem_key"}, prefixed: true, bytea: true,
		live: "expires_at > NOW()", pageSize: 64,
		aad: func(k []string) []byte { return fieldAAD(idempotencyTable, "body", idempotencyRowID(k[0], k[1])) }},
}

// keystoreSealedColumns are the keystore's ciphertexts: provider keys and organisation credentials (the Azure
// registry's credential documents among them).
var keystoreSealedColumns = []sealedColumn{
	{table: "provider_keys", column: "ciphertext", keys: []string{"user_id", "provider"},
		aad: func(k []string) []byte { return keyAAD(k[0], k[1]) }},
	{table: "org_provider_keys", column: "ciphertext", keys: []string{"org_id", "provider"},
		aad: func(k []string) []byte { return orgKeyAAD(k[0], k[1]) }},
}

// ResealFields moves the secret column values a previous key sealed to the current key and reports what it found;
// without field encryption there is nothing to move. Idempotent and resumable: every rewrite is a compare-and-set,
// so a concurrent save or a second replica's pass is never overwritten, and a pass stopped part-way leaves every
// value readable. Runs on the pool, outside any request's RLS scope, as the maintenance job it is.
func (b *PostgresStorageBackend) ResealFields(ctx context.Context) (ResealReport, error) {
	s := b.sealer.Load()
	if s == nil {
		return ResealReport{}, nil
	}
	return resealColumns(ctx, b.db, s.keys, resealOptions{}, fieldSealedColumns)
}

// ResealKeys moves the provider keys and organisation credentials a previous key sealed to the current key, like
// ResealFields. A row keeps its updated_at: its value is unchanged, and that stamp is the compare-and-swap token of
// in-flight edits and the orphan sweep's clock.
func (s *EncryptedKeyStore) ResealKeys(ctx context.Context) (ResealReport, error) {
	return resealColumns(ctx, s.db, s.keys, resealOptions{}, keystoreSealedColumns)
}

// resealOptions let a test shrink the pages and act between them.
type resealOptions struct {
	pageSize int
	pageDone func()
}

type resealPass struct {
	db     *sql.DB
	keys   *sealKeys
	opts   resealOptions
	report ResealReport
}

// resealColumns walks cols in order and returns what it found, also when it stops on an error.
func resealColumns(ctx context.Context, db *sql.DB, keys *sealKeys, opts resealOptions, cols []sealedColumn) (ResealReport, error) {
	p := &resealPass{db: db, keys: keys, opts: opts}
	for i := range cols {
		if err := p.column(ctx, &cols[i]); err != nil {
			return p.report, fmt.Errorf("re-seal %s.%s: %w", cols[i].table, cols[i].column, err)
		}
	}
	return p.report, nil
}

// resealRow is one stored value and its row's primary key.
type resealRow struct {
	key    []string
	stored []byte
}

func (p *resealPass) column(ctx context.Context, c *sealedColumn) error {
	size := c.pageSize
	if p.opts.pageSize > 0 {
		size = p.opts.pageSize
	}
	if size <= 0 {
		size = defaultResealPageSize
	}
	var after []string
	for {
		rows, err := p.page(ctx, c, after, size)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if err := p.resealValue(ctx, c, r.key, r.stored); err != nil {
				return err
			}
		}
		if p.opts.pageDone != nil {
			p.opts.pageDone()
		}
		if len(rows) < size {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		after = rows[len(rows)-1].key
	}
}

// page reads the next size rows after the key after (nil: from the start). The result set is closed before any of
// its rows is rewritten.
func (p *resealPass) page(ctx context.Context, c *sealedColumn, after []string, size int) ([]resealRow, error) {
	keyList := strings.Join(c.keys, ", ")
	where := []string{c.column + " IS NOT NULL"}
	if c.live != "" {
		where = append(where, c.live)
	}
	args := make([]any, 0, len(c.keys)+1)
	if after != nil {
		ph := make([]string, len(after))
		for i, k := range after {
			args = append(args, k)
			ph[i] = "$" + strconv.Itoa(i+1)
		}
		where = append(where, "("+keyList+") > ("+strings.Join(ph, ", ")+")")
	}
	args = append(args, size)
	// #nosec G202 -- table and column names come from the sealedColumn lists in this file; values are parameters.
	q := "SELECT " + keyList + ", " + c.column + " FROM " + c.table + " WHERE " + strings.Join(where, " AND ") +
		" ORDER BY " + keyList + " LIMIT $" + strconv.Itoa(len(args))
	rows, err := p.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []resealRow
	for rows.Next() {
		r := resealRow{key: make([]string, len(c.keys))}
		dest := make([]any, 0, len(c.keys)+1)
		for i := range r.key {
			dest = append(dest, &r.key[i])
		}
		dest = append(dest, &r.stored)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// resealAttempts bounds the rewrites of one value that keeps changing under the pass.
const resealAttempts = 3

// resealValue moves one stored value to the current key. The rewrite is a compare-and-set on the value as read, so
// a concurrent save, delete or second pass is never overwritten; when it loses, the value is read again and judged
// afresh.
func (p *resealPass) resealValue(ctx context.Context, c *sealedColumn, key []string, stored []byte) error {
	aad := c.aad(key)
	checked := false
	for attempt := 1; ; attempt++ {
		raw, sealed, decodeErr := c.decode(stored)
		if !sealed {
			return nil // plaintext: the at-rest backfill's to seal
		}
		if !checked {
			p.report.Checked++
			checked = true
		}
		var plain []byte
		current := false
		err := decodeErr
		if err == nil {
			plain, current, err = p.keys.open(raw, aad)
		}
		switch {
		case err != nil:
			p.unreadable(c, key)
			return nil
		case current:
			return nil
		case attempt > resealAttempts:
			p.report.Remaining++
			return nil
		}
		resealed, err := p.keys.seal(plain, aad)
		if err != nil {
			return err
		}
		moved, err := p.compareAndSet(ctx, c, key, stored, c.encode(resealed))
		if err != nil {
			return err
		}
		if moved {
			p.report.Resealed++
			return nil
		}
		var found bool
		if stored, found, err = p.reread(ctx, c, key); err != nil || !found {
			return err // gone (deleted, or an expired record taken over) or the read failed
		}
	}
}

func (p *resealPass) unreadable(c *sealedColumn, key []string) {
	p.report.Unreadable++
	if len(p.report.UnreadableRefs) < maxUnreadableRefs {
		p.report.UnreadableRefs = append(p.report.UnreadableRefs, SealedValueRef{Table: c.table, Column: c.column, RowID: strings.Join(key, "/")})
	}
}

// decode splits a stored value into nonce || ciphertext; sealed is false for a plaintext value.
func (c *sealedColumn) decode(stored []byte) (raw []byte, sealed bool, err error) {
	enc := stored
	if c.prefixed {
		rest, ok := strings.CutPrefix(string(stored), sealedPrefix)
		if !ok {
			return nil, false, nil
		}
		enc = []byte(rest)
	}
	raw = make([]byte, base64.StdEncoding.DecodedLen(len(enc)))
	n, err := base64.StdEncoding.Decode(raw, enc)
	return raw[:n], true, err
}

// encode is decode's inverse for a value sealed with the current key.
func (c *sealedColumn) encode(raw []byte) []byte {
	s := base64.StdEncoding.EncodeToString(raw)
	if c.prefixed {
		s = sealedPrefix + s
	}
	return []byte(s)
}

// value is v as the column's parameter: bytes for bytea, text otherwise.
func (c *sealedColumn) value(v []byte) any {
	if c.bytea {
		return v
	}
	return string(v)
}

// keyMatch is the WHERE clause selecting the row by its primary key, $1..$n, with its arguments.
func (c *sealedColumn) keyMatch(key []string) (string, []any) {
	conds := make([]string, len(c.keys))
	args := make([]any, len(key))
	for i, col := range c.keys {
		conds[i] = col + " = $" + strconv.Itoa(i+1)
		args[i] = key[i]
	}
	return strings.Join(conds, " AND "), args
}

func (p *resealPass) compareAndSet(ctx context.Context, c *sealedColumn, key []string, old, updated []byte) (bool, error) {
	match, args := c.keyMatch(key)
	args = append(args, c.value(updated), c.value(old))
	n := len(args)
	// #nosec G202 -- table and column names come from the sealedColumn lists in this file; values are parameters.
	q := "UPDATE " + c.table + " SET " + c.column + " = $" + strconv.Itoa(n-1) + " WHERE " + match + " AND " + c.column + " = $" + strconv.Itoa(n)
	res, err := p.db.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	return affected == 1, err
}

func (p *resealPass) reread(ctx context.Context, c *sealedColumn, key []string) ([]byte, bool, error) {
	match, args := c.keyMatch(key)
	where := match + " AND " + c.column + " IS NOT NULL"
	if c.live != "" {
		where += " AND " + c.live
	}
	var stored []byte
	// #nosec G202 -- table and column names come from the sealedColumn lists in this file; values are parameters.
	err := p.db.QueryRowContext(ctx, "SELECT "+c.column+" FROM "+c.table+" WHERE "+where, args...).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	return stored, err == nil, err
}
