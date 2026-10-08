package testutil_test

import (
	"testing"

	"pad-analyzer/internal/storage/contract"
	"pad-analyzer/internal/testutil"
)

// TestFakeBackend_Contract runs the storage contract against the test double that over 90 test files build on, with
// no known divergences: a test passing against the fake has to hold on Postgres too. Keep it that way; a scenario
// the fake cannot meet is a bug in the fake.
func TestFakeBackend_Contract(t *testing.T) {
	contract.RunSuite(t, testutil.NewFakeBackend())
}

// TestFakeBackend_AuditContract holds the fake's audit log to the contract Postgres meets.
func TestFakeBackend_AuditContract(t *testing.T) {
	contract.RunAuditSuite(t, testutil.NewFakeBackend())
}
