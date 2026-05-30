package service

import (
	"context"
	"errors"
	"testing"
)

func TestValidateUserPath_RejectsEmptyAndWhitespace(t *testing.T) {
	cases := []string{"", "   ", "\t\n"}
	for _, c := range cases {
		if err := validateUserPath(c); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("validateUserPath(%q): want ErrInvalidPath, got %v", c, err)
		}
	}
}

func TestValidateUserPath_RejectsNullByte(t *testing.T) {
	// A null byte in a path can confuse Go's syscall layer on some platforms
	// and is a classic exploit vector when paths are passed to C-based APIs.
	bad := "ok-prefix\x00../../etc/passwd"
	if err := validateUserPath(bad); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("expected ErrInvalidPath for null-byte path, got %v", err)
	}
}

func TestValidateUserPath_AcceptsOrdinaryPaths(t *testing.T) {
	cases := []string{
		"/tmp/flow.txt",
		"C:\\Users\\me\\flow.txt",
		"./relative/flow.txt",
		"../sibling/flow.txt", // legitimate in local mode
	}
	for _, c := range cases {
		if err := validateUserPath(c); err != nil {
			t.Errorf("validateUserPath(%q): unexpected error %v", c, err)
		}
	}
}

func TestLoadFlowFromPath_RejectsInvalidPath(t *testing.T) {
	svc := &FlowService{ctx: context.Background()}
	if _, err := svc.LoadFlowFromPath(""); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("empty path should yield ErrInvalidPath, got %v", err)
	}
	if _, err := svc.LoadFlowFromPath("bad\x00path.txt"); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("null-byte path should yield ErrInvalidPath, got %v", err)
	}
}
