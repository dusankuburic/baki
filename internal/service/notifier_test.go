package service

import (
	"testing"
)

func TestNilNotifier_EmitIsNoOp(t *testing.T) {
	n := NilNotifier{}
	n.Emit("test", "data")
	n.EmitTo("u1", "test", "data")
	if !n.HasSubscriber("u1") {
		t.Error("NilNotifier must always report a subscriber (false signals 'client gone')")
	}
}
