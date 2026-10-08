package api

import (
	"os"
	"testing"

	"github.com/zalando/go-keyring"
	"golang.org/x/crypto/bcrypt"

	"pad-analyzer/internal/auth"
)

// TestMain runs the package at bcrypt's minimum cost. The register, login and
// password tests hash and verify through the real handlers, and at the
// production cost (12, ~0.2 s per operation) they were ~90% of this
// package's run time — over 170 s of it under -race, which is how CI runs.
// Stored hashes verify at the cost they embed, so nothing a test asserts
// depends on the cost.
//
// The OS keychain is replaced by go-keyring's in-memory mock: a test that reaches the keyring store (revoking a
// sign-in, saving a key) must never touch the developer's own desktop credentials.
func TestMain(m *testing.M) {
	keyring.MockInit()
	restore := auth.SetBcryptCostForTesting(bcrypt.MinCost)
	code := m.Run()
	restore()
	os.Exit(code)
}
