// Package downloader_test provides unit tests for the downloader library package.
//
// OBJECTIVE:
// Validate Downloader constructor logic, token resolution, secretprotector decryption, and AuthProvider resolution.
//
// CORE COMPONENTS:
// 1. TestDownloader_New: Tests Downloader instance creation and nil config validation.
// 2. TestDownloader_LoadAccessToken: Tests token resolution from strings, files, and environment variables with secretprotector decryption.
// 3. TestDownloader_ResolveAuthProvider_ClientCredentials: Tests provider resolution for Client Credentials flow.
// 4. TestDownloader_ValidateFinalConfig: Tests configuration constraint checks.
//
// TEST STRATEGY:
// Uses temporary directories, environment variable overrides, and secretprotector AES-256-GCM test keys to test library API behavior.
package downloader_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"criticalsys.net/secretprotector/pkg/libsecsecrets"

	"criticalsys.net/o365mbx/downloader"
	"criticalsys.net/o365mbx/engine"

	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDownloader_New(t *testing.T) {
	cfg := &engine.Config{
		MailboxName:   "user@example.com",
		WorkspacePath: "/tmp/workspace",
		TokenString:   "test-token",
	}

	dl, err := downloader.New(cfg, log.WithFields(log.Fields{}))
	require.NoError(t, err)
	assert.NotNil(t, dl)

	// Test nil config error
	_, errNil := downloader.New(nil, log.WithFields(log.Fields{}))
	assert.Error(t, errNil)
}

func TestDownloader_LoadAccessToken(t *testing.T) {
	ctx := context.Background()

	// 1. Streamed in-memory token (TokenString)
	cfgString := &engine.Config{TokenString: "my-jwt-string"}
	token, err := downloader.LoadAccessToken(cfgString)
	require.NoError(t, err)
	assert.Equal(t, "my-jwt-string", token)

	// Generate a valid 32-byte master key via libsecsecrets
	masterKeyHex, err := libsecsecrets.GenerateKey()
	require.NoError(t, err)
	masterKeyBytes, err := libsecsecrets.ResolveKey(ctx, masterKeyHex, "", "")
	require.NoError(t, err)

	// Encrypt a test token using secretprotector
	rawPlaintextToken := "super-secret-oauth-jwt-token"
	encryptedCiphertext, err := libsecsecrets.Encrypt(ctx, rawPlaintextToken, masterKeyBytes)
	require.NoError(t, err)

	// 2. Encrypted Env token (JWT_TOKEN)
	t.Setenv("JWT_TOKEN", encryptedCiphertext)
	cfgEnv := &engine.Config{
		TokenEnv:        true,
		SecretMasterKey: masterKeyHex,
	}
	tokenEnv, errEnv := downloader.LoadAccessToken(cfgEnv)
	require.NoError(t, errEnv)
	assert.Equal(t, rawPlaintextToken, tokenEnv)

	// 3. Encrypted File token with direct master key flag
	tmpFile := filepath.Join(t.TempDir(), "encrypted_token.txt")
	require.NoError(t, os.WriteFile(tmpFile, []byte(encryptedCiphertext), 0o600))

	cfgFile := &engine.Config{
		TokenFile:       tmpFile,
		SecretMasterKey: masterKeyHex,
	}
	tokenFile, errFile := downloader.LoadAccessToken(cfgFile)
	require.NoError(t, errFile)
	assert.Equal(t, rawPlaintextToken, tokenFile)

	// 4. Encrypted File token with Master Key File (-secret-master-key-file)
	keyDir := filepath.Join(".", ".test_keys")
	require.NoError(t, os.MkdirAll(keyDir, 0o700))
	defer func() { _ = os.RemoveAll(keyDir) }()
	tmpKeyFile := filepath.Join(keyDir, "master_key.txt")
	require.NoError(t, os.WriteFile(tmpKeyFile, []byte(masterKeyHex), 0o600))

	cfgKeyFile := &engine.Config{
		TokenFile:           tmpFile,
		SecretMasterKeyFile: tmpKeyFile,
	}
	tokenKeyFile, errKeyFile := downloader.LoadAccessToken(cfgKeyFile)
	require.NoError(t, errKeyFile)
	assert.Equal(t, rawPlaintextToken, tokenKeyFile)

	// 4. SECURITY POLICY VIOLATION: Unencrypted plaintext stored file MUST BE REJECTED
	tmpUnencryptedFile := filepath.Join(t.TempDir(), "unencrypted_token.txt")
	require.NoError(t, os.WriteFile(tmpUnencryptedFile, []byte("eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.unencrypted.token"), 0o600))

	cfgUnencrypted := &engine.Config{
		TokenFile:       tmpUnencryptedFile,
		SecretMasterKey: masterKeyHex,
	}
	_, errUnencrypted := downloader.LoadAccessToken(cfgUnencrypted)
	assert.Error(t, errUnencrypted, "Unencrypted stored secrets must be rejected under Zero-Trust policy")

	// 5. SECURITY POLICY VIOLATION: Missing master key MUST BE REJECTED
	cfgNoKey := &engine.Config{
		TokenFile: tmpFile,
	}
	_, errNoKey := downloader.LoadAccessToken(cfgNoKey)
	assert.Error(t, errNoKey, "Stored secrets without a master key must be rejected")

	// 6. Multiple token sources error
	cfgMulti := &engine.Config{TokenString: "abc", TokenEnv: true}
	_, errMulti := downloader.LoadAccessToken(cfgMulti)
	assert.Error(t, errMulti)
}

func TestDownloader_ResolveAuthProvider_ClientCredentials(t *testing.T) {
	ctx := context.Background()

	// 1. Plaintext Client Credentials
	cfgPlain := &engine.Config{
		TenantID:     "tenant-123",
		ClientID:     "client-456",
		ClientSecret: "secret-789",
	}
	providerPlain, err := downloader.ResolveAuthProvider(cfgPlain)
	require.NoError(t, err)
	assert.NotNil(t, providerPlain)

	// 2. Encrypted Client Secret File via secretprotector
	masterKeyHex, err := libsecsecrets.GenerateKey()
	require.NoError(t, err)
	masterKeyBytes, err := libsecsecrets.ResolveKey(ctx, masterKeyHex, "", "")
	require.NoError(t, err)

	rawSecret := "my-super-secret-azure-client-secret"
	encryptedSecretCiphertext, err := libsecsecrets.Encrypt(ctx, rawSecret, masterKeyBytes)
	require.NoError(t, err)

	tmpSecretFile := filepath.Join(t.TempDir(), "encrypted_client_secret.txt")
	require.NoError(t, os.WriteFile(tmpSecretFile, []byte(encryptedSecretCiphertext), 0o600))

	cfgEncrypted := &engine.Config{
		TenantID:         "tenant-123",
		ClientID:         "client-456",
		ClientSecretFile: tmpSecretFile,
		SecretMasterKey:  masterKeyHex,
	}
	providerEncrypted, err := downloader.ResolveAuthProvider(cfgEncrypted)
	require.NoError(t, err)
	assert.NotNil(t, providerEncrypted)
}

func TestDownloader_ValidateFinalConfig(t *testing.T) {
	// Valid full config
	cfg := &engine.Config{
		MailboxName:   "user@example.com",
		WorkspacePath: "/tmp/workspace",
	}
	assert.NoError(t, downloader.ValidateFinalConfig(cfg))

	// Invalid email
	cfgInvalidEmail := &engine.Config{
		MailboxName:   "invalid-email",
		WorkspacePath: "/tmp/workspace",
	}
	assert.Error(t, downloader.ValidateFinalConfig(cfgInvalidEmail))

	// Missing workspace for non-healthcheck
	cfgNoWorkspace := &engine.Config{
		MailboxName: "user@example.com",
	}
	assert.Error(t, downloader.ValidateFinalConfig(cfgNoWorkspace))

	// Valid healthcheck without workspace
	cfgHealth := &engine.Config{
		MailboxName: "user@example.com",
		HealthCheck: true,
	}
	assert.NoError(t, downloader.ValidateFinalConfig(cfgHealth))
}

func TestDownloader_IsValidEmail(t *testing.T) {
	assert.True(t, downloader.IsValidEmail("test@domain.com"))
	assert.False(t, downloader.IsValidEmail("not-an-email"))
}

func TestDownloader_Execute_ValidationFailure(t *testing.T) {
	cfg := &engine.Config{
		MailboxName: "invalid-mailbox",
		TokenString: "token",
	}
	dl, err := downloader.New(cfg, log.WithFields(log.Fields{}))
	require.NoError(t, err)

	out := &bytes.Buffer{}
	ctx := context.Background()
	execErr := dl.Execute(ctx, out)
	assert.Error(t, execErr)
}
