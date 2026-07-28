// Package utils provides common utility functions for the application,
// such as safe pointer dereferencing.
//
// OBJECTIVE:
// This package contains stateless, reusable helper functions that simplify
// common operations used across multiple packages in the application.
//
// CORE FUNCTIONALITY:
//  1. Safe Dereferencing: Provides "Value" helpers (StringValue, TimeValue, etc.)
//     that safely dereference pointers and return a default value if the pointer is nil.
//     This is particularly useful when working with the Microsoft Graph SDK's model
//     where almost all fields are pointers.
package utils

import (
	"strings"
	"time"
)

// StringValue returns the value of the string pointer or a default value if nil.
func StringValue(s *string, defaultValue string) string {
	if s == nil {
		return defaultValue
	}
	return *s
}

// TimeValue returns the value of the time.Time pointer or a default value if nil.
func TimeValue(t *time.Time, defaultValue time.Time) time.Time {
	if t == nil {
		return defaultValue
	}
	return *t
}

// BoolValue returns the value of the bool pointer or a default value if nil.
func BoolValue(b *bool, defaultValue bool) bool {
	if b == nil {
		return defaultValue
	}
	return *b
}

// Int32Value returns the value of the int32 pointer or a default value if nil.
func Int32Value(i *int32, defaultValue int32) int32 {
	if i == nil {
		return defaultValue
	}
	return *i
}

// SanitizeControlCharacters strips non-printable ASCII control characters (\x00-\x1F, \x7F)
// and ANSI escape codes to prevent terminal output garbling or injection vulnerabilities.
func SanitizeControlCharacters(s string) string {
	var builder strings.Builder
	builder.Grow(len(s))

	inEscapeSeq := false
	for i := 0; i < len(s); i++ {
		b := s[i]

		// Handle ANSI escape sequences (\x1b[...)
		if b == 0x1b {
			inEscapeSeq = true
			continue
		}
		if inEscapeSeq {
			if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || b == '~' {
				inEscapeSeq = false
			}
			continue
		}

		// Strip non-printable ASCII control characters (\x00-\x1F and \x7F) except tab (\t), newline (\n), and carriage return (\r)
		if (b < 0x20 && b != '\t' && b != '\n' && b != '\r') || b == 0x7f {
			continue
		}

		builder.WriteByte(b)
	}

	return builder.String()
}
