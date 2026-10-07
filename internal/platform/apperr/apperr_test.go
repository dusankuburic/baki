package apperr

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"testing"
)

func TestConstructors_KindStatusAndDefaultCode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		build  func(code, msg string) *Error
		kind   Kind
		status int
		code   string
	}{
		{Invalid, KindInvalid, http.StatusBadRequest, "BAD_REQUEST"},
		{Unauthenticated, KindUnauthenticated, http.StatusUnauthorized, "UNAUTHORIZED"},
		{Forbidden, KindForbidden, http.StatusForbidden, "FORBIDDEN"},
		{NotFound, KindNotFound, http.StatusNotFound, "NOT_FOUND"},
		{Conflict, KindConflict, http.StatusConflict, "CONFLICT"},
		{Unprocessable, KindUnprocessable, http.StatusUnprocessableEntity, "UNPROCESSABLE"},
		{RateLimited, KindRateLimited, http.StatusTooManyRequests, "RATE_LIMITED"},
		{Unavailable, KindUnavailable, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE"},
		{NotImplemented, KindNotImplemented, http.StatusNotImplemented, "NOT_IMPLEMENTED"},
		{Internal, KindInternal, http.StatusInternalServerError, "INTERNAL_ERROR"},
	}
	screamingSnake := regexp.MustCompile(`^[A-Z][A-Z0-9]*(_[A-Z0-9]+)*$`)
	for _, c := range cases {
		e := c.build("", "msg")
		if e.Kind != c.kind || e.Kind.Status() != c.status || e.Code != c.code || e.Message != "msg" {
			t.Errorf("%v: got kind %v status %d code %q message %q, want %v %d %q", c.kind, e.Kind, e.Kind.Status(), e.Code, e.Message, c.kind, c.status, c.code)
		}
		if !screamingSnake.MatchString(e.Code) {
			t.Errorf("%v: default code %q is not SCREAMING_SNAKE", c.kind, e.Code)
		}
		if got := c.build("FLOW_LOCKED", "msg").Code; got != "FLOW_LOCKED" {
			t.Errorf("%v: explicit code became %q", c.kind, got)
		}
	}
	if k := Kind(200); k.Status() != http.StatusInternalServerError || k.Code() != "INTERNAL_ERROR" {
		t.Errorf("an unknown kind must be internal, got %d %q", k.Status(), k.Code())
	}
	if (&Error{}).Kind.Status() != http.StatusInternalServerError {
		t.Error("the zero Error must be a 500")
	}
}

func TestError_TextAndCause(t *testing.T) {
	t.Parallel()
	cause := errors.New("dial tcp: connection refused")
	e := Unavailable("AI_PROVIDER_DOWN", "the AI provider is not reachable").Wrap(cause)
	if got, want := e.Error(), "the AI provider is not reachable: dial tcp: connection refused"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(e, cause) || !errors.Is(fmt.Errorf("handler: %w", e), cause) {
		t.Error("errors.Is must reach the cause")
	}
	if got := (&Error{Kind: KindNotFound}).Error(); got != "not found" {
		t.Errorf("an Error without a message reads %q, want the kind's name", got)
	}

	got, ok := As(fmt.Errorf("context: %w", e))
	if !ok || got != e {
		t.Errorf("As = %v, %v; want the wrapped Error", got, ok)
	}
	if _, ok := As(cause); ok {
		t.Error("As found an Error in a plain error")
	}
}

// A package-level Error is shared by every request: WithDetails and Wrap copy it, and the copies still match it.
func TestError_CopiesLeaveTheOriginalAndMatchIt(t *testing.T) {
	t.Parallel()
	base := Conflict("FLOW_LOCKED", "the flow is locked").WithDetails(map[string]any{"flowId": "f-1"})
	withMore := base.WithDetails(map[string]any{"lockedBy": "u-2"})
	wrapped := base.Wrap(errors.New("row locked"))

	if len(base.Details) != 1 || base.Err != nil {
		t.Errorf("the original changed: details %v, cause %v", base.Details, base.Err)
	}
	if withMore.Details["flowId"] != "f-1" || withMore.Details["lockedBy"] != "u-2" {
		t.Errorf("WithDetails must add to the existing details, got %v", withMore.Details)
	}
	for _, err := range []error{withMore, wrapped, fmt.Errorf("save: %w", wrapped)} {
		if !errors.Is(err, base) {
			t.Errorf("%v does not match the Error it was copied from", err)
		}
	}
	if errors.Is(base, Conflict("OTHER", "x")) || errors.Is(base, NotFound("FLOW_LOCKED", "x")) {
		t.Error("Is matched another code or kind")
	}
	if !errors.Is(&Error{Kind: KindNotFound}, NotFound("", "x")) {
		t.Error("an empty code must match the kind's default")
	}
}
