package database

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"
)

// pendingBackfill is a copy a step left to after boot: the rows of a table that grows with use, which a step must not
// read while it holds its locks. pending answers whether a row is left to copy (one anti-join, so a finished copy
// costs one statement per boot); page copies the rows after $1 (the last key copied), at most $2, and answers the
// page's last key, NULL past the end. Idempotent: a page skips what is already there.
type pendingBackfill struct {
	since   int
	name    string
	pending string
	page    string
}

// schemaUpkeepBackfills are the copies the steps left to after boot.
//
// v50 (0050_knowledge_chunk_search.up.sql): the chunks stored before the step, into the lexical search's side table;
// its trigger copies every chunk written since. Until the copy ends, the lexical search does not see the older
// chunks, and the vector search is unchanged.
var schemaUpkeepBackfills = []pendingBackfill{
	{50, "knowledge_chunk_search",
		`SELECT EXISTS (SELECT 1 FROM knowledge_chunks c WHERE NOT EXISTS (SELECT 1 FROM knowledge_chunk_search s WHERE s.chunk_id = c.id))`,
		`WITH page AS (
			SELECT c.id, c.org_id, c.content FROM knowledge_chunks c WHERE c.id > $1 ORDER BY c.id LIMIT $2
		), copied AS (
			INSERT INTO knowledge_chunk_search (chunk_id, org_id, content)
			SELECT id, org_id, content FROM page
			ON CONFLICT (chunk_id) DO NOTHING
		)
		SELECT max(id) FROM page`},
}

// backfillPageSize is how many rows one page of a backfill copies, in a transaction of its own (autocommit on the
// job's connection): short enough that a page never holds its row locks for long. A var for tests.
var backfillPageSize = 500

// backfillPageAttempts bounds how often one page is read again after a row of it was deleted between the page's read
// and its write (the foreign key refuses the copy of a row that is gone).
const backfillPageAttempts = 3

// runBackfill copies what bf left to copy, page by page in key order.
func runBackfill(ctx context.Context, conn *sql.Conn, bf pendingBackfill) error {
	var pending bool
	if err := conn.QueryRowContext(ctx, bf.pending).Scan(&pending); err != nil || !pending {
		return err
	}
	start, after, pages := time.Now(), "", 0
	for {
		var last sql.NullString
		var err error
		for attempt := 1; ; attempt++ {
			err = conn.QueryRowContext(ctx, bf.page, after, backfillPageSize).Scan(&last)
			if !isPgErrCode(err, pgErrForeignKeyViolation) || attempt >= backfillPageAttempts {
				break
			}
		}
		if err != nil {
			return fmt.Errorf("copy the page after %q: %w", after, err)
		}
		if !last.Valid {
			break
		}
		after = last.String
		pages++
	}
	slog.Info("schema upkeep: backfill finished", "backfill", bf.name, "pages", pages, "took", time.Since(start).Round(time.Millisecond))
	return nil
}
