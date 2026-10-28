package service

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
)

// R-streaming-2: the bounded journal drops only tool calls (tool / tool_result), never an approval card or its
// decision, and leaves one marker tool_result saying how many calls it dropped, so the stored trail and a
// reconnecting client show the truncation.

// recordToolPair journals one tool call: its start and its result.
func recordToolPair(ctl *streamCtl, i int) {
	ctl.recordJournal("tool", map[string]interface{}{"name": "list_findings", "i": i})
	ctl.recordJournal("tool_result", map[string]interface{}{"name": "list_findings", "ok": true, "summary": fmt.Sprint(i), "i": i})
}

// The item's done-when: 150 tool pairs, a fix_proposal at index 3 and its fix_decision at index 149. The proposal
// keeps its decided status, the trail starts with the marker, and the journal stays bounded.
func TestJournal_ApprovalsSurviveTruncationAndAMarkerSaysSo(t *testing.T) {
	ctl := &streamCtl{}
	ctl.mu.Lock()
	n := 0 // events recorded so far: the index the next one takes
	record := func(typ string, data map[string]interface{}) {
		ctl.recordJournal(typ, data)
		n++
	}
	for pair := 0; pair < 150; pair++ {
		for _, typ := range []string{"tool", "tool_result"} {
			if n == 3 {
				record("fix_proposal", map[string]interface{}{"proposalId": "p1", "tool": "apply_fix", "ruleId": "unhandled-error", "summary": "wrap it"})
			}
			if n == 149 {
				record("fix_decision", map[string]interface{}{"proposalId": "p1", "status": "applied", "message": "verified"})
			}
			data := map[string]interface{}{"name": "list_findings", "i": pair}
			if typ == "tool_result" {
				data["ok"], data["summary"] = true, fmt.Sprint(pair)
			}
			record(typ, data)
		}
	}
	events := ctl.snapshotEvents()
	toolCalls, approvals := ctl.turn.toolCalls, ctl.turn.approvals["applied"]
	ctl.mu.Unlock()

	if len(events) > maxJournalEvents+2 {
		t.Errorf("journal holds %d events, want at most %d (the cap and the two approval events)", len(events), maxJournalEvents+2)
	}
	calls, fixes := journalTurnRecord(events)
	if len(fixes) != 1 || fixes[0].ProposalID != "p1" || fixes[0].Status != "applied" || fixes[0].Message != "verified" {
		t.Fatalf("fix proposals %+v, want p1 with its decided status", fixes)
	}
	if len(calls) == 0 || calls[0].Name != journalTruncatedName {
		t.Fatalf("tool calls %+v, want the truncation marker first", calls)
	}
	var omitted int
	if _, err := fmt.Sscanf(calls[0].Summary, "%d earlier tool calls omitted", &omitted); err != nil {
		t.Fatalf("marker summary %q: %v", calls[0].Summary, err)
	}
	if !calls[0].Ok || calls[0].Label == "" {
		t.Errorf("marker %+v, want ok with a label (it is not a failed tool)", calls[0])
	}
	if got := omitted + len(calls) - 1; got != 150 {
		t.Errorf("the marker's %d omitted calls and the %d kept ones add up to %d, want all 150", omitted, len(calls)-1, got)
	}
	// The turn's record counts every call and decision once, whatever the journal dropped.
	if toolCalls != 150 || approvals != 1 {
		t.Errorf("turn counted %d tool calls and %d applied approvals, want 150 and 1", toolCalls, approvals)
	}
	markers := 0
	for _, ev := range events {
		if ev.Data["name"] == journalTruncatedName {
			markers++
		}
	}
	if markers != 1 {
		t.Errorf("%d truncation markers, want one (it is updated, not repeated)", markers)
	}
}

// Memory stays bounded with approvals kept: len(journal) <= cap + approval events, after any number of trims.
func TestJournal_BoundHoldsWithApprovalsKept(t *testing.T) {
	ctl := &streamCtl{}
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	approvals := 0
	for i := 0; i < 1000; i++ {
		recordToolPair(ctl, i)
		if i%50 == 0 {
			ctl.recordJournal("fix_proposal", map[string]interface{}{"proposalId": fmt.Sprint("p", i)})
			ctl.recordJournal("fix_decision", map[string]interface{}{"proposalId": fmt.Sprint("p", i), "status": "declined"})
			approvals += 2
		}
		if len(ctl.journal) > maxJournalEvents+approvals {
			t.Fatalf("after %d pairs the journal holds %d events, want at most %d", i+1, len(ctl.journal), maxJournalEvents+approvals)
		}
	}
	_, fixes := journalTurnRecord(ctl.snapshotEvents())
	if len(fixes) != 20 {
		t.Errorf("%d fix proposals kept, want all 20", len(fixes))
	}
	for _, f := range fixes {
		if f.Status != "declined" {
			t.Errorf("proposal %s status %q, want declined", f.ProposalID, f.Status)
		}
	}
}

// A drop never leaves the result of a dropped call behind: batched calls journal their starts first, then their
// results, and the cut moves past the results of the starts it took.
func TestJournal_TruncationDropsWholeCalls(t *testing.T) {
	ctl := &streamCtl{}
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	for batch := 0; batch < 20; batch++ {
		for i := 0; i < 4; i++ {
			ctl.recordJournal("tool", map[string]interface{}{"name": "list_findings", "batch": batch})
		}
		for i := 0; i < 4; i++ {
			ctl.recordJournal("tool_result", map[string]interface{}{"name": "list_findings", "ok": true, "batch": batch})
		}
	}
	events := ctl.snapshotEvents()
	if events[0].Data["name"] != journalTruncatedName {
		t.Fatalf("first event %+v, want the truncation marker", events[0])
	}
	if events[1].Type != "tool" {
		t.Errorf("the first kept event is a %s of batch %v, want a call's start (no orphaned result)", events[1].Type, events[1].Data["batch"])
	}
	starts, results := 0, 0
	for _, ev := range events[1:] {
		switch ev.Type {
		case "tool":
			starts++
		case "tool_result":
			results++
		}
	}
	var omitted int
	if _, err := fmt.Sscanf(journalString(events[0].Data["summary"]), "%d earlier tool calls omitted", &omitted); err != nil {
		t.Fatal(err)
	}
	if starts != results || omitted+results != 80 {
		t.Errorf("kept %d starts and %d results, %d omitted, want whole calls adding up to 80", starts, results, omitted)
	}
}

// End to end: a turn with an approved fix followed by more tool calls than the journal holds stores the fix with
// its decision and a trail that starts with the marker; a reconnecting client gets both.
func TestStream_LongToolTurnKeepsItsApprovalAndMarksTheTruncation(t *testing.T) {
	doc := fixTestDoc()
	var many []ai.ToolCall
	for i := 0; i < 60; i++ {
		many = append(many, ai.ToolCall{ID: fmt.Sprint("r", i), Name: "list_findings", Input: json.RawMessage(fmt.Sprintf(`{"offset":%d}`, i))})
	}
	p := &testutil.FakeProvider{Tools: true, Turns: []testutil.FakeTurn{
		{ToolCalls: []ai.ToolCall{{ID: "f1", Name: "apply_fix", Input: []byte(`{"blockId":"b1","ruleId":"unhandled-error"}`)}}, TokensIn: 10, TokensOut: 5},
		{ToolCalls: many, TokensIn: 10, TokensOut: 5},
		{Text: "Done.", TokensIn: 10, TokensOut: 5},
	}}
	svc := failedTurnService(t, "journal-long-turn", p, doc)
	req, ctl := streamTurn(t, svc, doc, "journal-long-turn", true)
	if !ctl.done {
		t.Fatalf("the turn failed: %s", ctl.errMsg)
	}

	answer := storedAnswer(t, svc, doc.ID, req)
	if answer == nil {
		t.Fatal("no stored answer")
	}
	if len(answer.FixProposals) != 1 || answer.FixProposals[0].Status != "applied" {
		t.Errorf("stored fix proposals %+v, want the applied fix", answer.FixProposals)
	}
	if len(answer.ToolCalls) == 0 || answer.ToolCalls[0].Name != journalTruncatedName {
		t.Errorf("stored trail %+v, want the truncation marker first", answer.ToolCalls)
	}

	res, err := svc.ResumeStream(context.Background(), ctl.streamID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var marker, proposal bool
	for _, ev := range res.Events {
		marker = marker || ev.Data["name"] == journalTruncatedName
		proposal = proposal || ev.Type == "fix_proposal"
	}
	if !marker || !proposal {
		t.Errorf("resume events carry the marker %v and the proposal %v, want both", marker, proposal)
	}
}

// Several "tool" events never get a tool_result: the context-block row, provider status lines, tools_unsupported,
// step_limit_reached, a hosted step whose completion never arrives. One of them in the dropped part must not make the
// cut run on to the end of the journal (it dropped all 100 droppable events and left an orphaned result first): the
// cut pairs real calls only, keeps about half, keeps whole calls and still counts every dropped call.
func TestJournal_UnpairedStartsDoNotWipeTheTrail(t *testing.T) {
	for _, unpaired := range []map[string]interface{}{
		contextBlockMissingNotice(),
		{"name": "tools_unsupported", "label": "This model can't use tools — answering without them"},
		{"name": "step_limit_reached", "label": "step limit"},
		{"name": "code_interpreter", "label": "Running code"}, // a hosted step whose completion never came
	} {
		t.Run(journalString(unpaired["name"]), func(t *testing.T) {
			ctl := &streamCtl{}
			ctl.mu.Lock()
			defer ctl.mu.Unlock()
			ctl.recordJournal("tool", unpaired)
			for i := 0; i < 60; i++ {
				recordToolPair(ctl, i)
			}
			events := ctl.snapshotEvents()
			if len(events) < maxJournalEvents/2 {
				t.Fatalf("the journal holds %d events after one trim, want about half the cap kept", len(events))
			}
			if events[0].Data["name"] != journalTruncatedName {
				t.Fatalf("first event %+v, want the truncation marker", events[0])
			}
			if events[1].Type != "tool" {
				t.Errorf("the first kept event is a %s %v, want a call's start (no orphaned result)", events[1].Type, events[1].Data)
			}
			starts, results := 0, 0
			for _, ev := range events[1:] {
				switch {
				case ev.Type == "tool" && ev.Data["name"] == "list_findings":
					starts++
				case ev.Type == "tool_result":
					results++
				}
			}
			var omitted int
			if _, err := fmt.Sscanf(journalString(events[0].Data["summary"]), "%d earlier tool calls omitted", &omitted); err != nil {
				t.Fatal(err)
			}
			if starts != results || omitted+results != 60 {
				t.Errorf("kept %d starts and %d results, %d omitted, want whole calls adding up to 60", starts, results, omitted)
			}
		})
	}
}

// Repeated trims with notices between the calls keep the count exact.
func TestJournal_RepeatedTrimsWithNoticesCountEveryCall(t *testing.T) {
	ctl := &streamCtl{}
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	for i := 0; i < 500; i++ {
		ctl.recordJournal("notice", map[string]interface{}{"kind": noticeHistoryTruncated})
		ctl.recordJournal("tool", map[string]interface{}{"name": "a"})
		ctl.recordJournal("tool_result", map[string]interface{}{"name": "a", "ok": true})
		if len(ctl.journal) > maxJournalEvents {
			t.Fatalf("journal holds %d events, want at most %d", len(ctl.journal), maxJournalEvents)
		}
	}
	calls, _ := journalTurnRecord(ctl.snapshotEvents())
	var omitted int
	if _, err := fmt.Sscanf(calls[0].Summary, "%d earlier tool calls omitted", &omitted); err != nil {
		t.Fatal(err)
	}
	if got := omitted + len(calls) - 1; got != 500 {
		t.Errorf("omitted %d + kept %d = %d, want 500", omitted, len(calls)-1, got)
	}
}
