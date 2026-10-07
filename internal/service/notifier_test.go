package service

import (
	"context"
	"testing"
)

func TestNilNotifier_EmitIsNoOp(t *testing.T) {
	n := NilNotifier{}
	n.EmitTo("u1", "test", "data")
	n.EmitToOrg(context.Background(), "org-1", "test", "data")
	n.EmitToAll("test", "data")
	if !n.HasSubscriber("u1") {
		t.Error("NilNotifier must always report a subscriber (false signals 'client gone')")
	}
}
