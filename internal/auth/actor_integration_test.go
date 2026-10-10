package auth

import (
	"context"
	"testing"
)

// An integration (a CI credential) is recorded by audit as itself, and holds nothing authorization could grant: no
// user, no role, no scope.
func TestIntegrationActor(t *testing.T) {
	a := IntegrationActor("org-1", "key-1")
	if a.Kind != ActorIntegration || a.Kind.String() != "integration" || a.OrgID != "org-1" || a.TokenID != "key-1" {
		t.Errorf("IntegrationActor = %+v (%s)", a, a.Kind)
	}
	if a.UserID != "" || a.Role != "" || a.Scoped() {
		t.Errorf("an integration carries a user, role or scopes: %+v", a)
	}
	for _, scope := range ValidTokenScopes {
		if a.Allows(scope) {
			t.Errorf("an integration allows %q", scope)
		}
	}
	if got := ActorFromContext(WithActor(context.Background(), a)); got != a {
		t.Errorf("ActorFromContext = %+v", got)
	}
	global := IntegrationActor("", "")
	if global.Kind != ActorIntegration || global.OrgID != "" || global.TokenID != "" {
		t.Errorf("the deployment secret's actor = %+v", global)
	}
}
