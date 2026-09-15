package models

import "testing"

// TestChatRequest_ConversationKey pins the key precedence: the client thread
// id wins, then the legacy block scope, then the legacy flow-wide scope.
// Older clients that never send conversationId keep their existing keys.
func TestChatRequest_ConversationKey(t *testing.T) {
	cases := []struct {
		name string
		req  ChatRequest
		want string
	}{
		{"thread id wins", ChatRequest{ConversationID: "t-1", ContextBlockID: "b-1"}, "t-1"},
		{"block scope without thread", ChatRequest{ContextBlockID: "b-1"}, "b-1"},
		{"legacy flow scope", ChatRequest{}, "flow"},
	}
	for _, c := range cases {
		if got := c.req.ConversationKey(); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}
