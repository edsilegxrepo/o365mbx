// Package apperrors defines custom error types for structured error handling
// throughout the application.
//
// OBJECTIVE:
// Provide unit tests for APIError formatting, FileSystemError unwrapping, sentinel errors, and exit code classification.
//
// CORE COMPONENTS:
// 1. TestAPIError_Error: Tests API error string generation.
// 2. TestFileSystemError_Error: Tests FileSystemError wrapping and errors.Unwrap.
// 3. TestGetExitCode: Tests exit code mapping for auth, API, config, filesystem, and generic errors.
//
// TEST STRATEGY:
// Uses testify assertion helpers to verify error message formatting, unwrap functionality, and exit code classification.
package apperrors

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAPIError_Error(t *testing.T) {
	err := &APIError{
		StatusCode: 404,
		Msg:        "Not Found",
	}
	assert.Equal(t, "API error (status: 404): Not Found", err.Error())
}

func TestFileSystemError_Error(t *testing.T) {
	innerErr := errors.New("disk full")
	err := &FileSystemError{
		Path: "/tmp/test",
		Msg:  "failed to write",
		Err:  innerErr,
	}
	assert.Equal(t, "file system error on path '/tmp/test': failed to write: disk full", err.Error())
	assert.Equal(t, innerErr, errors.Unwrap(err))
}

func TestErrMissingDeltaLink(t *testing.T) {
	assert.Equal(t, "API did not provide a delta link on the final page of an incremental sync", ErrMissingDeltaLink.Error())
}

func TestGetExitCode(t *testing.T) {
	assert.Equal(t, ExitSuccess, GetExitCode(nil))
	assert.Equal(t, ExitAuthError, GetExitCode(&APIError{StatusCode: 401, Msg: "Unauthorized"}))
	assert.Equal(t, ExitAuthError, GetExitCode(&APIError{StatusCode: 403, Msg: "Forbidden"}))
	assert.Equal(t, ExitAPIError, GetExitCode(&APIError{StatusCode: 500, Msg: "Internal Error"}))
	assert.Equal(t, ExitFileSystemError, GetExitCode(&FileSystemError{Path: "/tmp", Msg: "read error", Err: errors.New("io error")}))
	assert.Equal(t, ExitAuthError, GetExitCode(errors.New("invalid token string")))
	assert.Equal(t, ExitConfigError, GetExitCode(errors.New("invalid configuration option")))
	assert.Equal(t, 1, GetExitCode(errors.New("generic unknown error")))
}
