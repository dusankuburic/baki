package database

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"pad-core/logger"

	"pad-analyzer/internal/storage/interfaces"
)

// The flows domain: flows and everything keyed by a flow (versions, analysis, sharing, triage, comments, alerts,
// conversations).
func init() {
	registerLifecycle(lifecycleDomain{
		name: "flows",
		tables: []tableClaim{
			{table: "flows", export: "flows", erase: erasedByStep, org: orgByStep,
				noPurge: "user content: a flow goes when its owner deletes it", legacy: "flows"},
			{table: "conversations", export: "conversations", erase: erasedByStep, org: orgNotKeyed,
				noPurge: "user content: it goes with its flow, never on a timer", legacy: "conversations"},
			{table: "flow_versions", noExport: "earlier versions of the exported flows (open: the bundle carries current content only)",
				erase: erasedByStep | erasedByCascade, org: orgNotKeyed, noPurge: "pruned per flow on every save"},
			{table: "flow_snapshots", noExport: "pre-mutation copies of the exported flows' source, an undo ring kept for 24 hours",
				erase: erasedByCascade, org: orgNotKeyed},
			{table: "flow_analysis", noExport: "derived from the flow's content",
				erase: erasedByCascade, org: orgNotKeyed, noPurge: "one row per flow"},
			{table: "flow_analysis_history", noExport: "derived from the flow's content",
				erase: erasedByCascade, org: orgNotKeyed},
			{table: "flow_collaborators", noExport: "sharing grants: who may open which flow",
				erase: erasedByCascade, org: orgNotKeyed, noPurge: "a grant lives until it is revoked or its flow goes"},
			{table: "flow_search", noExport: "the search index of the flows",
				erase: erasedByCascade, org: orgNotKeyed, noPurge: "derived, one row per flow"},
			{table: "flow_baselines", noExport: "the baseline finding keys of a flow, derived from its analysis",
				erase: erasedByStep | erasedByCascade, org: orgNotKeyed, noPurge: "one row per flow"},
			{table: "finding_status", export: "triageDecisions", erase: erasedByStep | erasedByCascade, org: orgNotKeyed,
				noPurge: "triage decisions stay until their flow goes"},
			{table: "finding_comments", export: "comments", erase: erasedByStep | erasedByCascade, org: orgNotKeyed,
				noPurge: "user content: it goes with its flow, never on a timer"},
			{table: "share_tokens", noExport: "hashes of public links (open: the links the user created are not in the bundle)",
				erase: erasedByStep | erasedByCascade, org: orgNotKeyed},
			{table: "gov_alerts", noExport: "notifications derived from analysis and from teammates' actions",
				erase: erasedByStep | erasedByCascade,
				org:   orgKept, orgWhy: "an alert is history of its flow; it goes with the flow or the retention window"},
		},
		export: []exportSection{
			flowsSection(),
			section("conversations", func(ctx context.Context, b *PostgresStorageBackend, x *exportRun) ([]*interfaces.ExportedConversation, error) {
				return b.exportConversations(ctx, x.flowIDs)
			}, func(out *interfaces.UserDataExport, v []*interfaces.ExportedConversation) { out.Conversations = v }),
			section("comments", func(ctx context.Context, b *PostgresStorageBackend, x *exportRun) ([]*interfaces.FindingComment, error) {
				return b.exportAuthoredComments(ctx, x.userID)
			}, func(out *interfaces.UserDataExport, v []*interfaces.FindingComment) { out.Comments = v }),
			section("triageDecisions", func(ctx context.Context, b *PostgresStorageBackend, x *exportRun) ([]*interfaces.FindingStatus, error) {
				return b.exportTriageDecisions(ctx, x.userID, x.flowIDs)
			}, func(out *interfaces.UserDataExport, v []*interfaces.FindingStatus) { out.TriageDecisions = v }),
		},
		erase: []eraseStep{
			// conversations.flow_id has no foreign key (DeleteFlow removes them explicitly), so the owned flows'
			// conversations go first.
			eraseByUser(eraseDetached, "conversations",
				`DELETE FROM conversations WHERE flow_id IN (SELECT id FROM flows WHERE owner_id = $1)`),
			{phase: eraseOwned, table: "flows", deletes: true, run: eraseOwnedFlows},
			// The user's identity on what stays with other people's flows: the content is theirs to keep.
			eraseByUser(eraseIdentity, "flow_versions", `UPDATE flow_versions SET created_by = '' WHERE created_by = $1`),
			eraseByUser(eraseIdentity, "finding_status", `UPDATE finding_status SET
		updated_by  = CASE WHEN updated_by = $1 THEN '' ELSE updated_by END,
		assignee_id = CASE WHEN assignee_id = $1 THEN '' ELSE assignee_id END
		WHERE updated_by = $1 OR assignee_id = $1`),
			eraseByUser(eraseIdentity, "flow_baselines", `UPDATE flow_baselines SET created_by = '' WHERE created_by = $1`),
			eraseByUser(eraseIdentity, "finding_comments", `UPDATE finding_comments SET author_id = '', author_name = '' WHERE author_id = $1`),
			// A link the user created on someone else's flow is the flow's: it stays, as when a collaborator is removed.
			eraseByUser(eraseIdentity, "share_tokens", `UPDATE share_tokens SET created_by = '' WHERE created_by = $1`),
			// Detached, not identity: it finds the flows the user worked on through the comment and triage
			// identities the identity phase clears.
			{phase: eraseDetached, table: "gov_alerts", run: eraseActorFromAlerts},
			eraseByUser(erasePersonal, "gov_alerts", `DELETE FROM gov_alerts WHERE target_user_id = $1`),
		},
		orgDelete: []orgDeleteStep{
			// The organisation's flows become their owners' own: an org id naming no organisation would still bill
			// chat on the flow to it and scope its analysis to its (deleted) rules.
			{phase: orgDetach, table: "flows", sql: `UPDATE flows SET org_id = '' WHERE org_id = $1`},
		},
		purge: []purgeStep{
			// The undo ring is a 24 hour affordance: reads hide an older snapshot at once, the sweep frees its space.
			// Batched because a long-idle deployment can hold many large rows; the count bound is per flow only.
			{table: "flow_snapshots", cond: fmt.Sprintf(`created_at < NOW() - INTERVAL '%d seconds'`, int(interfaces.FlowSnapshotTTL.Seconds())), batched: true,
				count: func(r *interfaces.PurgeResult) *int { return &r.FlowSnapshots }},
			// An expired share link can never be redeemed again, so its row is dead weight — and one that still
			// names a flow. No-expiry rows are permanent links; kept.
			{table: "share_tokens", cond: `expires_at IS NOT NULL AND expires_at < NOW()`,
				count: func(r *interfaces.PurgeResult) *int { return &r.ShareTokens }},
			// flow_analysis_history is append-only and the dashboard only reads the last 14–30 days.
			{table: "flow_analysis_history", cond: `analyzed_at < NOW() - ($1 * INTERVAL '1 day')`, batched: true, windowed: true,
				count: func(r *interfaces.PurgeResult) *int { return &r.FlowAnalysisHistory }},
			// gov_alerts is one row per alert per sweep, forever — ageing rows out cannot cause a re-alert (the
			// scanner's dedup is in-memory).
			{table: "gov_alerts", cond: `created_at < NOW() - ($1 * INTERVAL '1 day')`, batched: true, windowed: true,
				count: func(r *interfaces.PurgeResult) *int { return &r.GovAlerts }},
		},
	})
}

// eraseOwnedFlows deletes the flows the user owns (ON DELETE CASCADE takes their versions, analysis, triage,
// baselines, comments, share links, alerts and collaborator rows) and schedules their blobs' cleanup for after the
// commit: deleting blobs before it would orphan content a rollback puts back.
func eraseOwnedFlows(ctx context.Context, b *PostgresStorageBackend, tx *sql.Tx, e *erasure) error {
	ids, err := rowsOfStrings(ctx, tx, `DELETE FROM flows WHERE owner_id = $1 RETURNING id`, e.userID)
	if err != nil || b.blobClient == nil {
		return err
	}
	for _, id := range ids {
		flowID := id
		e.after = append(e.after, func(context.Context) {
			b.scheduleBlobCleanup(time.Now(), "delete-user-flow:"+flowID, func(ctx context.Context) {
				if err := b.deleteBlobs(ctx, fmt.Sprintf("flows/%s/", flowID)); err != nil {
					logger.Warn("DeleteUser: failed to delete flow blobs", "flow_id", flowID, "error", err)
				}
			})
		})
	}
	return nil
}

// eraseActorFromAlerts replaces the user's name in teammates' notifications about the user's actions. The comment
// and assignment alerts read exactly "New comment from <name>" and "Assigned to you by <name>", the name being the
// display name or, without one, the email; it becomes "a teammate", what the alert says when no name resolves. Only
// whole messages match, and only on flows the user reached: as a collaborator or an organisation member, or where
// they wrote a comment or decided a finding, which stays true after their access is gone (the alerts of owned flows
// go with them). The step runs outside the caller's RLS scope, and a substring or a common name would otherwise
// rewrite other tenants' alerts. A name the user had before renaming is not found.
func eraseActorFromAlerts(ctx context.Context, _ *PostgresStorageBackend, tx *sql.Tx, e *erasure) error {
	if e.email == "" && e.displayName == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE gov_alerts SET message = CASE type
		WHEN 'comment_added' THEN 'New comment from a teammate' ELSE 'Assigned to you by a teammate' END
		WHERE target_user_id <> '' AND target_user_id <> $1
		  AND ((type = 'comment_added' AND message IN ('New comment from ' || NULLIF($2, ''), 'New comment from ' || NULLIF($3, '')))
		    OR (type = 'finding_assigned' AND message IN ('Assigned to you by ' || NULLIF($2, ''), 'Assigned to you by ' || NULLIF($3, ''))))
		  AND flow_id IN (SELECT fc.flow_id FROM flow_collaborators fc WHERE fc.user_id = $1
		                  UNION
		                  SELECT f.id FROM flows f JOIN org_members om ON om.org_id = f.org_id
		                   WHERE om.user_id = $1 AND f.org_id <> ''
		                  UNION
		                  SELECT c.flow_id FROM finding_comments c WHERE c.author_id = $1
		                  UNION
		                  SELECT s.flow_id FROM finding_status s WHERE s.updated_by = $1 OR s.assignee_id = $1)`,
		e.userID, e.email, e.displayName)
	return err
}

// exportFlowPageSize bounds how many flows (with content) the export holds in memory at once on the streamed path.
const exportFlowPageSize = 100

// flowsSection is the bundle's flows: the ones the user owns, with content. A flow shared with the user is its
// owner's data, and an organisation's flows are their owners' (the UserDataExport contract). The streamed path
// writes them a page at a time.
func flowsSection() exportSection {
	s := section("flows", func(ctx context.Context, b *PostgresStorageBackend, x *exportRun) ([]*interfaces.FlowDocument, error) {
		all := []*interfaces.FlowDocument{}
		err := b.walkOwnedFlows(ctx, x, func(page []*interfaces.FlowDocument) error {
			all = append(all, page...)
			return nil
		})
		return all, err
	}, func(out *interfaces.UserDataExport, v []*interfaces.FlowDocument) { out.Flows = v })
	s.write = func(ctx context.Context, b *PostgresStorageBackend, x *exportRun, w *bufio.Writer) error {
		if err := w.WriteByte('['); err != nil {
			return err
		}
		first := true
		if err := b.walkOwnedFlows(ctx, x, func(page []*interfaces.FlowDocument) error {
			for _, f := range page {
				if !first {
					if err := w.WriteByte(','); err != nil {
						return err
					}
				}
				first = false
				if err := writeJSON(w, f); err != nil {
					return fmt.Errorf("marshal flow %s: %w", f.ID, err)
				}
			}
			return nil
		}); err != nil {
			return err
		}
		return w.WriteByte(']')
	}
	return s
}

// walkOwnedFlows hands fn the flows the user owns a page at a time and records their ids in x. The walk is a keyset
// over the immutable id: a flow saved mid-export cannot move across the cursor (updated_at would let it jump the
// page boundary and drop or repeat a row), and every page costs the same.
func (b *PostgresStorageBackend) walkOwnedFlows(ctx context.Context, x *exportRun, fn func([]*interfaces.FlowDocument) error) error {
	after := ""
	for {
		page, err := b.ownedFlowsPage(ctx, x.userID, after, exportFlowPageSize)
		if err != nil {
			return err
		}
		for _, f := range page {
			x.flowIDs = append(x.flowIDs, f.ID)
		}
		if err := fn(page); err != nil {
			return err
		}
		if len(page) < exportFlowPageSize {
			return nil
		}
		after = page[len(page)-1].ID
	}
}

// ownedFlowsPage is one page of the flows userID owns after the id afterID, with content. A data-subject export must
// be complete or fail: a flow whose content blob is permanently missing fails it instead of shipping empty.
func (b *PostgresStorageBackend) ownedFlowsPage(ctx context.Context, userID, afterID string, limit int) ([]*interfaces.FlowDocument, error) {
	rows, err := b.query(ctx).QueryContext(ctx,
		`SELECT `+flowListColumns+` FROM flows WHERE owner_id = $1 AND id > $2 ORDER BY id LIMIT $3`, userID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list flows: %w", err)
	}
	defer rows.Close()
	page, _, err := scanFlowList(rows, false, false)
	if err != nil {
		return nil, err
	}
	if b.blobClient != nil {
		if err := b.backfillBlobContent(ctx, page); err != nil {
			return nil, err
		}
	}
	for _, f := range page {
		if len(f.Content) == 0 && metadataRecordsContent(f.Metadata) {
			return nil, fmt.Errorf("content for flow %s is unavailable; refusing to produce an incomplete export", f.ID)
		}
	}
	return page, nil
}

// exportConversations returns every stored chat conversation on the given (owned) flows — chat about your
// own flow is user-authored content and belongs in the bundle.
func (b *PostgresStorageBackend) exportConversations(ctx context.Context, flowIDs []string) ([]*interfaces.ExportedConversation, error) {
	out := []*interfaces.ExportedConversation{}
	if len(flowIDs) == 0 {
		return out, nil
	}
	rows, err := b.query(ctx).QueryContext(ctx,
		`SELECT flow_id, scope, messages FROM conversations WHERE flow_id = ANY($1) ORDER BY flow_id, scope`,
		flowIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c interfaces.ExportedConversation
		var data []byte
		if err := rows.Scan(&c.FlowID, &c.Scope, &data); err != nil {
			return nil, err
		}
		if len(data) > 0 {
			if err := json.Unmarshal(data, &c.Messages); err != nil {
				return nil, fmt.Errorf("unmarshal conversation %s/%s: %w", c.FlowID, c.Scope, err)
			}
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// exportAuthoredComments returns the comments the user wrote, on any flow — they are the user's own words even
// when the flow belongs to someone else, and even on a flow the user can no longer open, so the read runs without
// the row-level-security user (erasure clears the same rows).
func (b *PostgresStorageBackend) exportAuthoredComments(ctx context.Context, userID string) ([]*interfaces.FindingComment, error) {
	q := b.query(ctx)
	out := []*interfaces.FindingComment{}
	err := asSystem(ctx, q, func() error {
		rows, err := q.QueryContext(ctx,
			`SELECT id, flow_id, finding_key, author_id, author_name, body, created_at
			 FROM finding_comments WHERE author_id = $1 ORDER BY created_at ASC, id ASC`, userID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c := &interfaces.FindingComment{}
			if err := rows.Scan(&c.ID, &c.FlowID, &c.FindingKey, &c.AuthorID, &c.AuthorName, &c.Body, &c.CreatedAt); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// exportTriageDecisions returns the triage rows the user last updated on the given (owned) flows — rows on
// those flows last touched by a collaborator are that collaborator's content and stay out.
func (b *PostgresStorageBackend) exportTriageDecisions(ctx context.Context, userID string, flowIDs []string) ([]*interfaces.FindingStatus, error) {
	out := []*interfaces.FindingStatus{}
	if len(flowIDs) == 0 {
		return out, nil
	}
	rows, err := b.query(ctx).QueryContext(ctx, `
		SELECT flow_id, finding_key, rule_id, status, justification, assignee_id, updated_by, updated_at
		FROM finding_status WHERE flow_id = ANY($1) AND updated_by = $2 ORDER BY flow_id, finding_key`,
		flowIDs, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var st interfaces.FindingStatus
		if err := rows.Scan(&st.FlowID, &st.FindingKey, &st.RuleID, &st.Status, &st.Justification, &st.AssigneeID, &st.UpdatedBy, &st.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &st)
	}
	return out, rows.Err()
}
