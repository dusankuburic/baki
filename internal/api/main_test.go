package api

import (
	"os"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"pad-analyzer/internal/auth"
)

// TestMain runs the package at bcrypt's minimum cost. The register, login and
// password tests hash and verify through the real handlers, and at the
// production cost (12, ~0.2 s per operation) they were ~90% of this
// package's run time — over 170 s of it under -race, which is how CI runs.
// Stored hashes verify at the cost they embed, so nothing a test asserts
// depends on the cost.
func TestMain(m *testing.M) {
	restore := auth.SetBcryptCostForTesting(bcrypt.MinCost)
	code := m.Run()
	restore()
	os.Exit(code)
}
