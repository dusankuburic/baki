package testutil

import (
	"os"
	"testing"
)

// TestSuite provides test infrastructure for backend testing
type TestSuite struct {
	tempDir string
	cleanup func()
}

// NewTestSuite creates a new test suite with temporary directory
func NewTestSuite(t *testing.T) *TestSuite {
	// Create temporary directory for tests
	tempDir, err := os.MkdirTemp("", "pad-analyzer-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}

	return &TestSuite{
		tempDir: tempDir,
		cleanup: func() {
			_ = os.RemoveAll(tempDir)
		},
	}
}

// Cleanup removes test resources
func (ts *TestSuite) Cleanup() {
	if ts.cleanup != nil {
		ts.cleanup()
	}
}

// GetTempDir returns the temporary directory path
func (ts *TestSuite) GetTempDir() string {
	return ts.tempDir
}

// AssertNoError fails the test if there is an error
func AssertNoError(t *testing.T, err error, msg string) {
	if err != nil {
		t.Fatalf("%s: %v", msg, err)
	}
}

// AssertEqual fails the test if values are not equal
func AssertEqual(t *testing.T, expected, actual interface{}, msg string) {
	if expected != actual {
		t.Fatalf("%s: expected %v, got %v", msg, expected, actual)
	}
}
