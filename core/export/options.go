package export

import "pad-core/models"

// Options carries CI-oriented context that the plain ReportTo* signatures have
// no room for: the policy verdict a gate was decided on, and where the analysed
// root sits relative to the repository so SARIF locations resolve in a code-
// scanning UI. The zero value reproduces the plain ReportTo* output exactly.
type Options struct {
	// Policy is the -policy evaluation result when a policy gated the run. It
	// is surfaced as SARIF run.properties.policy (+ an invocation whose
	// executionSuccessful is false on a failed gate) and as JUnit testsuite
	// <properties>, so an uploaded report says WHY the gate failed rather than
	// leaving the verdict in the terminal only.
	Policy *models.PolicyResult

	// SourceRoot is the analysed file's directory (or the analysed folder)
	// RELATIVE to the directory %SRCROOT% denotes — the CI checkout, i.e. the
	// CLI's working directory — slash-separated and without a trailing slash.
	// Empty when the analysed root IS that directory. It is prefixed onto every
	// artifactLocation.uri: a folder import of ./flows yields
	// "flows/teamA/Main.txt", which is the path GitHub needs to place an inline
	// PR annotation; a bare "Main.txt" matched nothing and placed nothing.
	SourceRoot string

	// SourceRootURI is the absolute file:// URI recorded as
	// originalUriBaseIds["%SRCROOT%"] (trailing slash added if missing) so a
	// consumer outside the checkout can still resolve the relative URIs. When
	// empty no originalUriBaseIds/uriBaseId is emitted.
	SourceRootURI string
}
