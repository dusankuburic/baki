package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-core/cache"
	"pad-core/models"
)

// progressRecorder captures the target user of every emitted event.
type progressRecorder struct {
	mu      sync.Mutex
	targets []string
}

func (r *progressRecorder) EmitToAll(name string, data any) { r.EmitTo("<broadcast>", name, data) }
func (r *progressRecorder) EmitToOrg(_ context.Context, orgID, name string, data any) {
	r.EmitTo("<org:"+orgID+">", name, data)
}
func (r *progressRecorder) EmitTo(userID, name string, data any) {
	if name != "analysis:progress" {
		return
	}
	r.mu.Lock()
	r.targets = append(r.targets, userID)
	r.mu.Unlock()
}
func (r *progressRecorder) HasSubscriber(string) bool { return true }

func (r *progressRecorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.targets
	r.targets = nil
	return out
}

var progressDocSeq int

// progressDoc returns a fresh, never-cached document so every analysis walks
// (only a walking leader emits progress).
func progressDoc() *models.FlowDocument {
	progressDocSeq++
	id := fmt.Sprintf("progress-scope-%d", progressDocSeq)
	return makeAnalyzableDoc(id,
		models.Block{ID: id + "-b1", Type: models.BlockTypeAction, RawType: "Display.ShowMessage", Name: "Show " + id, SubflowID: "main", LineNumber: 1},
		models.Block{ID: id + "-b2", Type: models.BlockTypeAction, RawType: "WebAutomation.Click", Name: "Click " + id, SubflowID: "main", LineNumber: 2},
	)
}

func newProgressScopeService(t *testing.T, rec *progressRecorder) *AnalysisService {
	t.Helper()
	svc, err := NewAnalysisService(rec, nilSettings{}, nil, AnalysisOptions{})
	if err != nil {
		t.Fatalf("NewAnalysisService: %v", err)
	}
	return svc
}

// TestAnalyzeFlowBackground_RecordsWithoutProgress: the scanner's context has
// no claims, so AnalyzeFlow emitted its progress with userID "" — a broadcast
// to every connected client of every tenant. AnalyzeFlowBackground records
// exactly like AnalyzeFlow but emits nothing.
func TestAnalyzeFlowBackground_RecordsWithoutProgress(t *testing.T) {
	rec := &progressRecorder{}
	svc := newProgressScopeService(t, rec)

	doc := progressDoc()
	report, err := svc.AnalyzeFlowBackground(context.Background(), doc)
	if err != nil || report == nil {
		t.Fatalf("AnalyzeFlowBackground: %v", err)
	}
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("background analysis emitted %d progress event(s) to %q; want none", len(got), got)
	}
	if cur, ok := svc.CurrentReport(doc); !ok || cur != report {
		t.Fatal("background analysis was not recorded as the flow's current report")
	}

	// Control: the interactive entry point does emit on a fresh walk.
	ctx := auth.WithClaims(context.Background(), &auth.Claims{UserID: "u-1"})
	if _, err := svc.AnalyzeFlow(ctx, progressDoc()); err != nil {
		t.Fatal(err)
	}
	if got := rec.take(); len(got) == 0 {
		t.Fatal("AnalyzeFlow emitted no progress")
	}
}

// TestAnalyzeFlow_NeverBroadcastsProgressWhenAuthEnabled: with auth enabled a
// claim-less context emits no progress; with auth off (local/desktop mode: no
// claims ever) progress goes to LocalUser, the clients without an identity.
func TestAnalyzeFlow_NeverBroadcastsProgressWhenAuthEnabled(t *testing.T) {
	rec := &progressRecorder{}
	svc := newProgressScopeService(t, rec)

	// Local mode (default): LocalUser is the desktop's clients.
	if _, err := svc.AnalyzeFlow(context.Background(), progressDoc()); err != nil {
		t.Fatal(err)
	}
	got := rec.take()
	if len(got) == 0 {
		t.Fatal("local mode: no progress emitted")
	}
	for _, u := range got {
		if u != "" {
			t.Fatalf("local mode: progress addressed to %q, want \"\"", u)
		}
	}

	svc.authEnabled = true
	if _, err := svc.AnalyzeFlow(context.Background(), progressDoc()); err != nil {
		t.Fatal(err)
	}
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("auth enabled, no claims: emitted %d progress event(s) to %q; want none", len(got), got)
	}

	ctx := auth.WithClaims(context.Background(), &auth.Claims{UserID: "u-2"})
	if _, err := svc.AnalyzeFlow(ctx, progressDoc()); err != nil {
		t.Fatal(err)
	}
	got = rec.take()
	if len(got) == 0 {
		t.Fatal("auth enabled, with claims: no progress emitted")
	}
	for _, u := range got {
		if u != "u-2" {
			t.Fatalf("auth enabled: progress addressed to %q, want the caller u-2", u)
		}
	}
}

// TestAnalyzeFlow_CancelledIsNotRecorded: a cancelled analysis is truncated;
// it must surface the cancellation and record nothing into the report pair /
// history the diff and dashboards read.
func TestAnalyzeFlow_CancelledIsNotRecorded(t *testing.T) {
	rec := &progressRecorder{}
	svc := newProgressScopeService(t, rec)

	doc := progressDoc()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, err := svc.AnalyzeFlow(ctx, doc)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled AnalyzeFlow: err = %v, want context.Canceled", err)
	}
	if report != nil {
		t.Fatalf("cancelled AnalyzeFlow returned a report with %d findings", len(report.Findings))
	}
	if _, ok := svc.CurrentReport(doc); ok {
		t.Fatal("cancelled analysis was recorded as the flow's current report")
	}
	if n := len(svc.analyzer.Reports(cache.Local())); n != 0 {
		t.Fatalf("cancelled analysis left %d report(s) in the dashboard cache", n)
	}

	// The same flow analyzed normally afterwards gets the full result.
	full, err := svc.AnalyzeFlow(context.Background(), doc)
	if err != nil || full == nil || len(full.Findings) == 0 {
		t.Fatalf("follow-up analysis: report=%v err=%v", full, err)
	}
}
