package service

import "errors"

var (
	// ErrNotFound is returned when a requested resource (flow, user, org) does not exist.
	ErrNotFound = errors.New("resource not found")

	// ErrPermissionDenied is returned when a user attempts to perform an action
	// without sufficient permissions (e.g. reading a private flow).
	ErrPermissionDenied = errors.New("permission denied")

	// ErrInvalidInput is returned when a request payload fails validation.
	ErrInvalidInput = errors.New("invalid input")

	// ErrConflict is returned when an action conflicts with existing state
	// (e.g. creating a duplicate, or demoting the last admin).
	ErrConflict = errors.New("conflict")

	// ErrUninitialized is returned when an operation requires a loaded flow
	// but none is present (local mode).
	ErrUninitialized = errors.New("no flow loaded")

	// ErrNotImplemented is a placeholder for planned but unfinished features.
	ErrNotImplemented = errors.New("not implemented")
)
