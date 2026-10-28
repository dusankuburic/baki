package database

import (
	"bytes"
	"context"
	"crypto/cipher"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// The key check (R-security-7, `bakicli keys check`): which configured key opens each stored sealed value, so an
// operator can see, before rotating PAD_AUTH_SECRET or removing PAD_ENCRYPTION_KEY_PREVIOUS, what that would make
// unreadable. It walks the columns the re-seal pass walks (fieldSealedColumns, keystoreSealedColumns) and only reads.

// SealKey is one key the check tries, named by where it is configured ("PAD_ENCRYPTION_KEY", ...). The report
// carries the name only, never the key.
type SealKey struct {
	Name string
	Key  []byte
}

// SealKeyColumn is what the check found in one sealed column.
type SealKeyColumn struct {
	Table, Column string
	// Checked counts the sealed values read; Plaintext the values stored unsealed (written before field encryption,
	// the at-rest backfill's to seal), which no key needs.
	Checked   int
	Plaintext int
	// Unreadable values open with none of the keys tried.
	Unreadable int
	// OpenedBy counts the values each key opens, by the key's name: the first key in the order given that opens a
	// value takes it.
	OpenedBy map[string]int
}

// SealKeyReport is the check over every sealed column: the totals, the columns in walk order, and the first values
// no key opened, named by table, column and row id (maxUnreadableRefs of them).
type SealKeyReport struct {
	Checked        int
	Plaintext      int
	Unreadable     int
	UnreadableRefs []SealedValueRef
	OpenedBy       map[string]int
	Columns        []SealKeyColumn
}

// CheckSealKeys reports which of keys opens each sealed value, trying them in order. It reads on db outside any
// row-level-security scope (a maintenance connection, as the re-seal pass), pages each column by its primary key,
// and writes nothing.
func CheckSealKeys(ctx context.Context, db *sql.DB, keys []SealKey) (SealKeyReport, error) {
	type namedAEAD struct {
		name string
		aead cipher.AEAD
	}
	aeads := make([]namedAEAD, 0, len(keys))
	for i, k := range keys {
		// A key listed twice opens what it opens under its first name.
		if slices.ContainsFunc(keys[:i], func(o SealKey) bool { return bytes.Equal(o.Key, k.Key) }) {
			continue
		}
		aead, err := newKeystoreAEAD(k.Key)
		if err != nil {
			return SealKeyReport{}, fmt.Errorf("key %s: %w", k.Name, err)
		}
		aeads = append(aeads, namedAEAD{name: k.Name, aead: aead})
	}
	rep := SealKeyReport{OpenedBy: map[string]int{}, UnreadableRefs: []SealedValueRef{}}
	for _, k := range keys {
		rep.OpenedBy[k.Name] += 0
	}
	walk := &resealPass{db: db}
	cols := append(append([]sealedColumn{}, fieldSealedColumns...), keystoreSealedColumns...)
	for i := range cols {
		c := &cols[i]
		col := SealKeyColumn{Table: c.table, Column: c.column, OpenedBy: map[string]int{}}
		size := c.pageSize
		if size <= 0 {
			size = defaultResealPageSize
		}
		var after []string
		for {
			rows, err := walk.page(ctx, c, after, size)
			if err != nil {
				return rep, fmt.Errorf("check %s.%s: %w", c.table, c.column, err)
			}
			for _, r := range rows {
				raw, sealed, decodeErr := c.decode(r.stored)
				if !sealed {
					if len(r.stored) > 0 {
						col.Plaintext++
					}
					continue
				}
				col.Checked++
				opened := ""
				if decodeErr == nil {
					aad := c.aad(r.key)
					for _, a := range aeads {
						ns := a.aead.NonceSize()
						if len(raw) < ns {
							break
						}
						if _, err := a.aead.Open(nil, raw[:ns], raw[ns:], aad); err == nil {
							opened = a.name
							break
						}
					}
				}
				if opened == "" {
					col.Unreadable++
					if len(rep.UnreadableRefs) < maxUnreadableRefs {
						rep.UnreadableRefs = append(rep.UnreadableRefs, SealedValueRef{Table: c.table, Column: c.column, RowID: strings.Join(r.key, "/")})
					}
					continue
				}
				col.OpenedBy[opened]++
			}
			if len(rows) < size {
				break
			}
			if err := ctx.Err(); err != nil {
				return rep, err
			}
			after = rows[len(rows)-1].key
		}
		rep.Checked += col.Checked
		rep.Plaintext += col.Plaintext
		rep.Unreadable += col.Unreadable
		for name, n := range col.OpenedBy {
			rep.OpenedBy[name] += n
		}
		rep.Columns = append(rep.Columns, col)
	}
	return rep, nil
}
