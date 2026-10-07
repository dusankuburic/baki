package testutil_test

import (
	"testing"

	"pad-analyzer/internal/storage/contract"
	"pad-analyzer/internal/testutil"
)

// TestFakeBackend_Contract runs the storage contract against the test double that over 90 test files build on.
// Until now only Postgres and the filesystem backend were held to it, so the fake could drift unseen: a test
// passing against the fake proved nothing about the real stores. Where the fake still diverges is listed below
// with the reason; the list may only shrink (CONTRACT_STRICT=1 runs the listed scenarios anyway). Bringing the
// fake to parity is step 1 of Phase 3 in docs/architecture/backend-refactoring-proposal.md.
func TestFakeBackend_Contract(t *testing.T) {
	contract.RunSuite(t, testutil.NewFakeBackend(), knownFakeDivergences...)
}

var knownFakeDivergences = []contract.Option{
	contract.KnownDivergence("LoadConversation_missing_returns_empty_slice_not_nil",
		"LoadConversation returns nil for a missing conversation, not an empty slice"),
	contract.KnownDivergence("ListCollaborators_empty_returns_non_nil_slice",
		"ListCollaborators returns nil when a flow has none"),
	contract.KnownDivergence("CreateUser_duplicate_email_returns_ErrEmailExists",
		"CreateUser does not check email uniqueness"),
	contract.KnownDivergence("LoadUsersByIDs_resolves_present_and_skips_missing",
		"LoadUsersByIDs is a stub that resolves nobody"),
	contract.KnownDivergence("QueryUsers_filters_pages_and_counts",
		"QueryUsers is a stub that returns no users"),
	contract.KnownDivergence("UpdateUserRole_missing_returns_ErrNotFound",
		"UpdateUserRole succeeds for an unknown user"),
	contract.KnownDivergence("ListFlows_AfterID_keyset_pages_by_id",
		"ListFlows/CountFlows ignore AfterID"),
	contract.KnownDivergence("ListFlows_unscoped_matches_nothing_AllFlows_enumerates",
		"ListFlows lacks the unscoped-filter guard and returns every flow"),
	contract.KnownDivergence("SaveFlow_stale_version_returns_ErrVersionConflict",
		"SaveFlow has no optimistic version check"),
}

// TestFakeBackend_AuditContract holds the fake's audit log to the contract Postgres meets.
func TestFakeBackend_AuditContract(t *testing.T) {
	contract.RunAuditSuite(t, testutil.NewFakeBackend())
}
