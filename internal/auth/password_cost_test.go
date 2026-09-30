package auth

import (
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// The production cost must stay at or above OWASP's bcrypt minimum (10), and
// this package's own tests run at it (no TestMain lowers it here).
func TestBcryptCost_ProductionDefault(t *testing.T) {
	if defaultBcryptCost < 10 {
		t.Fatalf("defaultBcryptCost = %d, below the OWASP minimum of 10", defaultBcryptCost)
	}
	if bcryptCost != defaultBcryptCost {
		t.Fatalf("bcryptCost = %d, want the default %d", bcryptCost, defaultBcryptCost)
	}
}

// The dummy hash an unknown-account login is checked against must cost what
// a real account's hash costs, or the two branches are told apart by timing.
func TestDummyBcryptHash_MatchesDefaultCost(t *testing.T) {
	cost, err := bcrypt.Cost([]byte(dummyBcryptHash))
	if err != nil {
		t.Fatalf("dummy hash does not parse: %v", err)
	}
	if cost != defaultBcryptCost {
		t.Fatalf("dummy hash cost = %d, want defaultBcryptCost %d", cost, defaultBcryptCost)
	}
}

func TestSetBcryptCostForTesting_LowersBothAndRestores(t *testing.T) {
	restore := SetBcryptCostForTesting(bcrypt.MinCost)
	hash, err := HashPassword("pw")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if c, _ := bcrypt.Cost([]byte(hash)); c != bcrypt.MinCost {
		t.Errorf("new hash cost = %d, want %d", c, bcrypt.MinCost)
	}
	if c, _ := bcrypt.Cost([]byte(dummyBcryptHash)); c != bcrypt.MinCost {
		t.Errorf("dummy hash cost = %d, want %d", c, bcrypt.MinCost)
	}
	if !CheckPasswordHash("pw", hash) {
		t.Error("a low-cost hash does not verify")
	}
	restore()
	if bcryptCost != defaultBcryptCost {
		t.Errorf("restore left bcryptCost = %d", bcryptCost)
	}
	if c, _ := bcrypt.Cost([]byte(dummyBcryptHash)); c != defaultBcryptCost {
		t.Errorf("restore left the dummy hash at cost %d", c)
	}
}
