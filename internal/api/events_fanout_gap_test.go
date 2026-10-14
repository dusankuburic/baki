package api

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"pad-analyzer/internal/redisx"
)

// A reconnect that missed more than the log replays (or whose replay ran out of write budget) is told so, with an
// event the client already understands as "refetch", instead of getting a prefix followed by the live frames with a
// silent hole between them (REVIEW_REDIS H2).

func TestSSEReplay_AGapIsSignalledNotSilentlySkipped(t *testing.T) {
	cluster, rs := newFanReplicas(t, 1)
	a := rs[0]
	a.EmitTo("u1", "flow:loaded", "e0")
	entries, err := cluster.Redis.Stream(redisx.SSELogKey.Key("u1"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("log = %v, %v", entries, err)
	}
	cursor := entries[0].ID
	// The client is away while far more durable frames than one replay carries are logged.
	var tail string
	for i := range redisx.SSEReplayMax + 60 {
		tail = fmt.Sprintf("%s-%d", strings.SplitN(cursor, "-", 2)[0], i+1)
		if err := a.rep.Client.XAdd(t.Context(), &redis.XAddArgs{Stream: redisx.SSELogKey.Key("u1"), ID: tail,
			Values: map[string]any{"v": 1, "d": fmt.Sprintf(`{"name":"flow:loaded","data":"missed-%d"}`, i)}}).Err(); err != nil {
			t.Fatal(err)
		}
	}

	again, _ := openSSE(t, a.srv, "u1", cursor)
	marker := next(t, again, "the gap marker")
	if marker.Name != "replay:gap" {
		t.Fatalf("first frame = %+v, want the replay:gap marker (a prefix of the missed frames would hide the hole)", marker)
	}
	if marker.ID != tail {
		t.Errorf("marker id = %q, want the log's newest entry %q so the client's cursor moves past what it refetches", marker.ID, tail)
	}
	quiet(t, again, 150*time.Millisecond, "a replayed prefix after the marker")

	// Live frames continue after the marker, once, with ids after it.
	a.EmitTo("u1", "flow:loaded", "live")
	live := next(t, again, "a live frame")
	if !strings.Contains(live.Data, "live") || redisx.CompareStreamIDs(live.ID, marker.ID) <= 0 {
		t.Errorf("live frame = %+v, want it after the marker %s", live, marker.ID)
	}

	// The cursor the marker carried is a proper cursor: reconnecting with it replays just what came after.
	third, _ := openSSE(t, a.srv, "u1", marker.ID)
	f := next(t, third, "the frame after the marker's id")
	if !strings.Contains(f.Data, "live") {
		t.Errorf("reconnect from the marker's id got %+v, want only the live frame", f)
	}
	quiet(t, third, 100*time.Millisecond, "another gap marker")
}

func TestSSEReplay_ARunOutWriteBudgetIsAGapToo(t *testing.T) {
	prev := replayWrite
	replayWrite = -time.Second // the budget is gone before the first frame is written
	t.Cleanup(func() { replayWrite = prev })
	_, rs := newFanReplicas(t, 1)
	a := rs[0]
	a.EmitTo("u1", "flow:loaded", "e1")
	a.EmitTo("u1", "flow:loaded", "e2")
	a.EmitTo("u1", "flow:loaded", "e3")
	entries, _ := a.cluster.Redis.Stream(redisx.SSELogKey.Key("u1"))

	again, _ := openSSE(t, a.srv, "u1", entries[0].ID)
	marker := next(t, again, "the gap marker")
	if marker.Name != "replay:gap" {
		t.Fatalf("first frame = %+v, want the replay:gap marker", marker)
	}
	if marker.ID != entries[2].ID {
		t.Errorf("marker id = %q, want the last entry the replay covered (%q)", marker.ID, entries[2].ID)
	}
	quiet(t, again, 150*time.Millisecond, "frames the replay did not write")
}

// An old client has no listener for the marker's name and must not trip over it: the frame is a plain data event of an
// unknown name, plus an id line the browser already handles.
func TestSSEReplay_TheMarkerIsAnOrdinaryEventToAnOldClient(t *testing.T) {
	_, rs := newFanReplicas(t, 1)
	a := rs[0]
	a.EmitTo("u1", "flow:loaded", "e0")
	again, _ := openSSE(t, a.srv, "u1", fmt.Sprintf("%d-0", time.Now().Add(-10*time.Second).UnixMilli())) // older than the log's first entry
	marker := next(t, again, "the gap marker")
	if marker.Name != "replay:gap" || !strings.HasPrefix(marker.Data, `{"name":"replay:gap","data":`) {
		t.Fatalf("marker = %+v, want a standard {name,data} event", marker)
	}
}
