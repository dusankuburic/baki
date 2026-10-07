package reqinfo

import (
	"context"
	"sync"
	"testing"

	"pad-analyzer/internal/auth"
)

func TestFrom_IsNilOutsideARequest(t *testing.T) {
	if From(context.Background()) != nil {
		t.Fatal("From on a context without an Info must be nil")
	}
	info := &Info{ID: "r-1"}
	if got := From(With(context.Background(), info)); got != info {
		t.Fatalf("From(With(info)) = %p, want %p", got, info)
	}
}

// TestSetActor_IsWriteOnce: once a layer has authenticated the request, nothing further in can re-identify it.
func TestSetActor_IsWriteOnce(t *testing.T) {
	info := &Info{}
	if info.Actor() != nil {
		t.Fatal("a fresh Info has no actor")
	}
	if info.SetActor(nil) {
		t.Error("SetActor(nil) must record nothing")
	}
	alice := &auth.Actor{Kind: auth.ActorUser, UserID: "alice"}
	if !info.SetActor(alice) {
		t.Fatal("the first SetActor must record")
	}
	if info.SetActor(&auth.Actor{Kind: auth.ActorUser, UserID: "mallory", Role: auth.RoleAdmin}) {
		t.Error("a second SetActor must be refused")
	}
	if info.SetActor(alice) {
		t.Error("re-recording the same actor must be refused too")
	}
	if got := info.Actor(); got != alice {
		t.Errorf("Actor() = %+v, want the first actor %+v", got, alice)
	}

	var none *Info
	if none.SetActor(alice) || none.Actor() != nil {
		t.Error("a nil Info (no RequestInfo layer) records and returns nothing")
	}
}

func TestSetActor_ConcurrentCallersHaveOneWinner(t *testing.T) {
	info := &Info{}
	const n = 32
	actors := make([]*auth.Actor, n)
	won := make([]bool, n)
	var wg sync.WaitGroup
	for i := range actors {
		actors[i] = &auth.Actor{Kind: auth.ActorUser, UserID: string(rune('a' + i))}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			won[i] = info.SetActor(actors[i])
		}(i)
	}
	wg.Wait()
	winners := 0
	for i, w := range won {
		if w {
			winners++
			if info.Actor() != actors[i] {
				t.Errorf("Actor() = %+v, but %+v won", info.Actor(), actors[i])
			}
		}
	}
	if winners != 1 {
		t.Fatalf("%d SetActor calls succeeded, want exactly 1", winners)
	}
}
