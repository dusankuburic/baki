package database

import (
	"context"
	"fmt"
	"strings"

	"pad-analyzer/internal/storage/interfaces"
)

// trgmRunSQL asks pg_trgm whether $1 holds a run of $2 word characters: a character is one when show_trgm draws
// trigrams from it alone. That is the test pg_trgm splits a LIKE pattern into words with, under this database's
// ctype and its C library's Unicode tables, whatever its collation provider.
const trgmRunSQL = `SELECT strpos(coalesce(string_agg(CASE WHEN show_trgm(c) = '{}' THEN ' ' ELSE 'w' END, '' ORDER BY n), ''),
	repeat('w', $2)) > 0
	FROM regexp_split_to_table($1::text, '') WITH ORDINALITY AS t(c, n)`

// maxTrgmCheckRunes bounds how much of a term the database classifies, one show_trgm call per character.
const maxTrgmCheckRunes = 256

// shortSearchTerm is interfaces.ShortSearchTerm as this database counts word characters. A term the Go rule calls
// short is short here too (SearchWordChar counts every word character the database's tables know,
// TestSearchWordChar_CountsEveryWordCharacterOfTheDatabase), and a run of ASCII letters and digits is a word under
// every ctype. Any other term goes to the database: under a C ctype it counts no other character, and Go's Unicode
// tables can know letters its C library does not. A term the database calls short gets no trigram-less probe of an
// index: content search refuses it, the name filters match a prefix.
func (b *PostgresStorageBackend) shortSearchTerm(ctx context.Context, term string) (bool, error) {
	term = strings.TrimSpace(term)
	switch {
	case term == "":
		return false, nil
	case interfaces.ShortSearchTerm(term):
		return true, nil
	case asciiWordRun(term):
		return false, nil
	}
	if runes := []rune(term); len(runes) > maxTrgmCheckRunes {
		term = string(runes[:maxTrgmCheckRunes])
	}
	var served bool
	if err := b.query(ctx).QueryRowContext(ctx, trgmRunSQL, term, interfaces.MinSearchTermRunes).Scan(&served); err != nil {
		return false, fmt.Errorf("classify the search term: %w", err)
	}
	return !served, nil
}

// asciiWordRun reports whether term has a run of MinSearchTermRunes ASCII letters or digits.
func asciiWordRun(term string) bool {
	run := 0
	for i := 0; i < len(term); i++ {
		if c := term[i]; 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' {
			if run++; run >= interfaces.MinSearchTermRunes {
				return true
			}
			continue
		}
		run = 0
	}
	return false
}

// nameQuery is filter with QueryPrefix set when the database draws no trigram from its name term (shortSearchTerm),
// so the list and its count match a prefix of the name on the caller's flows instead of reading flow_search.
func (b *PostgresStorageBackend) nameQuery(ctx context.Context, filter interfaces.FlowFilter) (interfaces.FlowFilter, error) {
	if filter.QueryPrefix || strings.TrimSpace(filter.Query) == "" {
		return filter, nil
	}
	short, err := b.shortSearchTerm(ctx, filter.Query)
	filter.QueryPrefix = short
	return filter, err
}
