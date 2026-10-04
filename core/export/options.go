package export

import "pad-core/models"

// Options carries CI-oriented context the plain ReportTo* signatures have no
// room for: the policy verdict a gate was decided on, and where the analysed
// root sits relative to the repository so SARIF locations resolve in a code-
// scanning UI. The zero value reproduces the plain ReportTo* output exactly.
type Options struct {
	// Policy is the -policy evaluation result when a policy gated the run,
	// surfaced as SARIF run.properties.policy (plus an invocation whose
	// executionSuccessful is false on a failed gate) and JUnit testsuite
	// <properties>, so an uploaded report says WHY the gate failed.
	Policy *models.PolicyResult

	// SourceRoot is the analysed file's directory (or analysed folder)
	// RELATIVE to the directory %SRCROOT% denotes (the CI checkout), slash-
	// separated, no trailing slash; empty when the analysed root IS that
	// directory. Prefixed onto every artifactLocation.uri — a folder import
	// of ./flows yields "flows/teamA/Main.txt", the path GitHub needs to
	// place an inline PR annotation (a bare "Main.txt" placed nothing).
	SourceRoot string

	// SourceRootURI is the absolute file:// URI recorded as
	// originalUriBaseIds["%SRCROOT%"] (trailing slash added if missing) so a
	// consumer outside the checkout can resolve the relative URIs; when empty
	// no originalUriBaseIds/uriBaseId is emitted.
	SourceRootURI string
}
