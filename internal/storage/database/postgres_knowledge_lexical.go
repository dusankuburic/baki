package database

import (
	"context"
	"database/sql"
	"fmt"

	"pad-analyzer/internal/storage/interfaces"
)

// knowledgeMinWordSimilarity is the lexical search's floor: a phrase of the question counts for a chunk when pg_trgm's
// word_similarity of the phrase against the chunk text reaches it. 1 is the phrase verbatim (case and punctuation
// aside); 0.8 still takes an inflection ("Run DOS commands" for "dos command" is 0.92) and refuses the same words in
// another order ("Run the command in DOS" for "run dos command" is 0.70). The fake's lexical search uses the same.
const knowledgeMinWordSimilarity = 0.8

// knowledgeLexicalPerPhrase bounds the candidates one phrase hands the score: a phrase that half the org's chunks hold
// says little about any of them. They are the phrase's best matches among the chunks the caller may see, taken from
// at most knowledgeLexicalWindow visible matches: the window bounds the similarity tests of a phrase the whole org
// holds, which would otherwise cost more than the knowledge lookup's whole deadline.
const (
	knowledgeLexicalPerPhrase = 20
	knowledgeLexicalWindow    = 200
)

// knowledgeLexicalSetup is what the lexical statement's transaction sets first: the floor as the threshold of the %>
// operator, which the trigram index answers (pg_trgm.word_similarity_threshold, 0.6 by default), so the index hands
// over only the candidates at the floor; and no sequential scan, since the side table's trigram index is the only
// access path the per-phrase probe wants and the planner, which prices pg_trgm's similarity tests far under their
// cost, would otherwise read the table and test every row.
func knowledgeLexicalSetup() []string {
	return []string{
		fmt.Sprintf(`SET LOCAL pg_trgm.word_similarity_threshold = %g`, knowledgeMinWordSimilarity),
		`SET LOCAL enable_seqscan = off`,
	}
}

// knowledgeLexicalSearchSQL is the lexical statement. Args: $1 org, $2 phrases (text[]), $3 limit, $4 embedding
// identity ("" = any; else that identity or none, the vector search's rule), $5 the floor, $6 the candidates per
// phrase, $7 the window per phrase. For each phrase the trigram index of knowledge_chunk_search (v50), the side table
// without row-level security, answers the org's matches (knowledge_chunk_search_trgm_idx, over the org and the text;
// the OFFSET 0 keeps the planner from driving the probe from knowledge_chunks instead): on knowledge_chunks, FORCE RLS
// keeps the planner from using an index for an operator that is not LEAKPROOF. The matches are joined to the chunks
// themselves, under their policy and with the vector search's filters (the org, the identity, never a pending chunk),
// BEFORE the cap, so chunks the caller may not see never crowd out one it may; of at most $7 such matches the $6 most
// similar are the phrase's candidates. A chunk's score is the sum of the word similarities of the phrases it is a
// candidate of, at the floor, so a chunk naming more of the question's phrases ranks first and the floor is exact
// whatever the threshold the probe ran with.
const knowledgeLexicalSearchSQL = `
		SELECT c.id, c.doc_id, c.content, d.filename, d.chunker_version
		FROM (
			SELECT m.chunk_id, sum(m.ws) AS score
			FROM unnest($2::text[]) p
			CROSS JOIN LATERAL (
				SELECT y.chunk_id, y.ws
				FROM (
					SELECT v.chunk_id, word_similarity(p, v.content) AS ws
					FROM (
						SELECT k.chunk_id, k.content FROM knowledge_chunk_search k
						WHERE k.org_id = $1 AND k.content %> p
						OFFSET 0
					) v
					JOIN knowledge_chunks vc ON vc.id = v.chunk_id
					WHERE vc.org_id = $1
					  AND NOT vc.pending
					  AND ($4::text = '' OR vc.embedding_identity IS NULL OR vc.embedding_identity = $4::text)
					LIMIT $7
				) y
				WHERE y.ws >= $5
				ORDER BY y.ws DESC, y.chunk_id
				LIMIT $6
			) m
			GROUP BY m.chunk_id
		) s
		JOIN knowledge_chunks c ON c.id = s.chunk_id
		JOIN knowledge_documents d ON d.id = c.doc_id
		ORDER BY s.score DESC, c.doc_id, c.id
		LIMIT $3`

// SearchKnowledgeLexical returns up to limit of the org's chunks that hold one of phrases at the floor
// (knowledgeMinWordSimilarity), those matching more of them first. It runs where SearchKnowledge runs: in the
// request's transaction when there is one (its row-level security applies), else in its own, since the threshold is
// SET LOCAL.
func (b *PostgresStorageBackend) SearchKnowledgeLexical(ctx context.Context, orgID string, phrases []string, limit int, identity string) ([]interfaces.KnowledgeChunk, error) {
	if len(phrases) == 0 || limit <= 0 {
		return nil, nil
	}
	var chunks []interfaces.KnowledgeChunk
	err := b.withTx(ctx, func(_ context.Context, tx *sql.Tx) error {
		for _, stmt := range knowledgeLexicalSetup() {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
		}
		rows, err := tx.QueryContext(ctx, knowledgeLexicalSearchSQL, orgID, textArray(phrases), limit, identity, knowledgeMinWordSimilarity, knowledgeLexicalPerPhrase, knowledgeLexicalWindow)
		if err != nil {
			return err
		}
		defer rows.Close()
		chunks, err = scanKnowledgeHits(rows)
		return err
	})
	if err != nil {
		return nil, err
	}
	return chunks, nil
}
