// Package o365client handles all interactions with the Microsoft Graph API,
// including message retrieval, attachment streaming, and mailbox management.
//
// OBJECTIVE:
// Provide fast, pure unit testing for O365Client initialization, authentication providers, and helper utilities.
// All Graph API endpoint interactions and OData schema validations are tested against Microsoft Dev Proxy in resilience_test.go.
//
// CORE COMPONENTS:
// 1. TestNewO365Client: Tests client constructor and random source validation.
// 2. TestStaticTokenAuthenticationProvider*: Tests static bearer token header injection.
// 3. TestClientCredentialsAuthenticationProvider_ErrorBranches: Tests Entra ID token provider validation.
// 4. TestO365Client_ParseFolderSize: Tests Kiota UntypedNode normalization and size type assertions.
// 5. TestO365Client_HandleError: Tests error classification helpers.
//
// TEST STRATEGY:
// Pure unit tests for memory data structures and helpers without external HTTP mocking libraries.
package o365client

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"testing"

	kiota "github.com/microsoft/kiota-abstractions-go"
	"github.com/stretchr/testify/assert"

	"criticalsys.net/o365mbx/apperrors"
)

func TestNewO365Client(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(1))
	client, err := NewO365Client("token", rng)
	assert.NoError(t, err)
	assert.NotNil(t, client)

	_, err = NewO365Client("", rng)
	assert.Error(t, err)
}

func TestStaticTokenAuthenticationProvider(t *testing.T) {
	t.Parallel()
	provider, err := NewStaticTokenAuthenticationProvider("test-token")
	assert.NoError(t, err)
	assert.NotNil(t, provider)

	_, err = NewStaticTokenAuthenticationProvider("")
	assert.Error(t, err)

	req := kiota.NewRequestInformation()
	err = provider.AuthenticateRequest(context.Background(), req, nil)
	assert.NoError(t, err)
	assert.True(t, req.Headers.ContainsKey("Authorization"))
}

func TestStaticTokenAuthenticationProvider_HeadersNil(t *testing.T) {
	t.Parallel()
	provider, _ := NewStaticTokenAuthenticationProvider("test-token")

	req := kiota.NewRequestInformation()
	req.Headers = nil

	err := provider.AuthenticateRequest(context.Background(), req, nil)
	assert.NoError(t, err)
	assert.True(t, req.Headers.ContainsKey("Authorization"))
}

func TestClientCredentialsAuthenticationProvider_ErrorBranches(t *testing.T) {
	t.Parallel()
	_, err := NewClientCredentialsAuthenticationProvider("", "client-id", "secret")
	assert.Error(t, err)

	_, err = NewClientCredentialsAuthenticationProvider("tenant-id", "", "secret")
	assert.Error(t, err)

	_, err = NewClientCredentialsAuthenticationProvider("tenant-id", "client-id", "")
	assert.Error(t, err)
}

type mockUntypedNode struct {
	val any
}

func (m mockUntypedNode) GetValue() any { return m.val }

func TestO365Client_ParseFolderSize(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name string
		size interface{}
		want int64
	}{
		{"int64 pointer", Ptr(int64(1024)), 1024},
		{"int32 pointer", Ptr(int32(512)), 512},
		{"int64 literal", int64(256), 256},
		{"int32 literal", int32(128), 128},
		{"float64 literal", float64(64.0), 64},
		{"float64 pointer", Ptr(float64(32.0)), 32},
		{"nil pointer", (*int64)(nil), 0},
		{"string number", "102400", 102400},
		{"string float", "5120.5", 5120},
		{"string pointer", Ptr("81920"), 81920},
		{"json.Number", json.Number("2048"), 2048},
		{"Kiota UntypedNode float64 pointer", mockUntypedNode{val: Ptr(float64(4096.0))}, 4096},
		{"Kiota UntypedNode string", mockUntypedNode{val: "8192"}, 8192},
		{"unsupported non-number string", "invalid-number", 0},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ExportParseFolderSize(tc.size)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestO365Client_HandleError_Complete(t *testing.T) {
	t.Parallel()
	assert.Nil(t, handleError(nil))

	err := errors.New("simple error")
	assert.Equal(t, err, handleError(err))

	appErr := &apperrors.APIError{StatusCode: 401, Msg: "unauthorized"}
	assert.Equal(t, appErr, handleError(appErr))
}
