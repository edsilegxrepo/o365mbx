// Package utils provides common utility functions for the application,
// such as safe pointer dereferencing.
//
// OBJECTIVE:
// Provide unit testing for safe dereferencing helper functions and ASCII control character stripping.
//
// CORE COMPONENTS:
// 1. TestStringValue, TestTimeValue, TestBoolValue, TestInt32Value: Tests nil-pointer fallback safety.
// 2. TestSanitizeControlCharacters: Tests control character removal while preserving standard whitespace (\n).
//
// TEST STRATEGY:
// Uses table-based and assertion-based unit tests to verify pointer dereferencing and string sanitization.
package utils

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestStringValue(t *testing.T) {
	s := "test"
	assert.Equal(t, "test", StringValue(&s, "default"))
	assert.Equal(t, "default", StringValue(nil, "default"))
}

func TestTimeValue(t *testing.T) {
	now := time.Now().UTC()
	assert.Equal(t, now, TimeValue(&now, time.Time{}))
	assert.Equal(t, time.Time{}, TimeValue(nil, time.Time{}))
}

func TestBoolValue(t *testing.T) {
	b := true
	assert.Equal(t, true, BoolValue(&b, false))
	assert.Equal(t, false, BoolValue(nil, false))
}

func TestInt32Value(t *testing.T) {
	i := int32(42)
	assert.Equal(t, int32(42), Int32Value(&i, 0))
	assert.Equal(t, int32(0), Int32Value(nil, 0))
}

func TestSanitizeControlCharacters(t *testing.T) {
	assert.Equal(t, "Hello World", SanitizeControlCharacters("Hello\x00 World"))
	assert.Equal(t, "Clean Text", SanitizeControlCharacters("\x1b[31mClean Text\x1b[0m"))
	assert.Equal(t, "Line1\nLine2", SanitizeControlCharacters("Line1\nLine2"))
}
