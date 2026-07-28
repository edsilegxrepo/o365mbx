// Package main is the entry point for the o365mbx application.
//
// This file contains unit and integration tests for the CLI and configuration logic.
package main

import (
	"context"
	"criticalsys/secretprotector/pkg/libsecsecrets"
	"flag"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"o365mbx/engine"
)

func TestLoadAccessToken(t *testing.T) {
	ctx := context.Background()
	masterKeyHex, errKey := libsecsecrets.GenerateKey()
	require.NoError(t, errKey)
	masterKeyBytes, errRes := libsecsecrets.ResolveKey(ctx, masterKeyHex, "", "")
	require.NoError(t, errRes)

	encryptedFileToken, errEncFile := libsecsecrets.Encrypt(ctx, "file-token", masterKeyBytes)
	require.NoError(t, errEncFile)

	encryptedEnvToken, errEncEnv := libsecsecrets.Encrypt(ctx, "env-token", masterKeyBytes)
	require.NoError(t, errEncEnv)

	tests := []struct {
		name    string
		cfg     *engine.Config
		setup   func()
		cleanup func()
		want    string
		wantErr bool
	}{
		{
			name: "TokenString source",
			cfg:  &engine.Config{TokenString: "test-token"},
			want: "test-token",
		},
		{
			name: "TokenFile source",
			cfg: &engine.Config{
				TokenFile:       "temp-token.txt",
				SecretMasterKey: masterKeyHex,
			},
			setup: func() {
				_ = os.WriteFile("temp-token.txt", []byte(encryptedFileToken), 0o600)
			},
			cleanup: func() {
				_ = os.Remove("temp-token.txt")
			},
			want: "file-token",
		},
		{
			name: "TokenFile read error",
			cfg: &engine.Config{
				TokenFile:       "non-existent.txt",
				SecretMasterKey: masterKeyHex,
			},
			wantErr: true,
		},
		{
			name: "TokenEnv source",
			cfg: &engine.Config{
				TokenEnv:        true,
				SecretMasterKey: masterKeyHex,
			},
			setup: func() {
				_ = os.Setenv("JWT_TOKEN", encryptedEnvToken)
			},
			cleanup: func() {
				_ = os.Unsetenv("JWT_TOKEN")
			},
			want: "env-token",
		},
		{
			name: "TokenEnv missing",
			cfg: &engine.Config{
				TokenEnv:        true,
				SecretMasterKey: masterKeyHex,
			},
			setup: func() {
				_ = os.Unsetenv("JWT_TOKEN")
			},
			wantErr: true,
		},
		{
			name:    "No token source",
			cfg:     &engine.Config{},
			wantErr: true,
		},
		{
			name:    "Multiple token sources",
			cfg:     &engine.Config{TokenString: "s", TokenEnv: true},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup()
			}
			if tt.cleanup != nil {
				defer tt.cleanup()
			}

			got, err := loadAccessToken(tt.cfg)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestIsValidEmail(t *testing.T) {
	assert.True(t, isValidEmail("test@example.com"))
	assert.True(t, isValidEmail("user.name+tag@domain.co.uk"))
	assert.False(t, isValidEmail("invalid-email"))
	assert.False(t, isValidEmail("test@"))
	assert.False(t, isValidEmail("@domain.com"))
}

func TestValidateFinalConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *engine.Config
		wantErr bool
		msg     string
	}{
		{
			name:    "Missing mailbox",
			cfg:     &engine.Config{},
			wantErr: true,
			msg:     "mailbox name is a required argument",
		},
		{
			name:    "Invalid email",
			cfg:     &engine.Config{MailboxName: "invalid"},
			wantErr: true,
			msg:     "invalid mailbox name format",
		},
		{
			name:    "Missing workspace",
			cfg:     &engine.Config{MailboxName: "test@test.com", HealthCheck: false, WorkspacePath: ""},
			wantErr: true,
			msg:     "workspace path is a required argument",
		},
		{
			name:    "Valid healthcheck without workspace",
			cfg:     &engine.Config{MailboxName: "test@test.com", HealthCheck: true, WorkspacePath: ""},
			wantErr: false,
		},
		{
			name:    "Missing state for incremental",
			cfg:     &engine.Config{MailboxName: "test@test.com", WorkspacePath: "path", ProcessingMode: "incremental", StateFilePath: ""},
			wantErr: true,
			msg:     "state file path must be provided",
		},
		{
			name:    "Missing processed folder for route",
			cfg:     &engine.Config{MailboxName: "test@test.com", WorkspacePath: "path", ProcessingMode: "route", ProcessedFolder: ""},
			wantErr: true,
			msg:     "processed folder name must be provided",
		},
		{
			name:    "Missing error folder for route",
			cfg:     &engine.Config{MailboxName: "test@test.com", WorkspacePath: "path", ProcessingMode: "route", ProcessedFolder: "p", ErrorFolder: ""},
			wantErr: true,
			msg:     "error folder name must be provided",
		},
		{
			name:    "Valid full mode",
			cfg:     &engine.Config{MailboxName: "test@test.com", WorkspacePath: "path", ProcessingMode: "full"},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateFinalConfig(tt.cfg)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.msg)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestOverrideConfigWithFlags(t *testing.T) {
	// Reset flags for testing
	fs := flag.NewFlagSet("test", flag.ContinueOnError)

	tokenString := fs.String("token-string", "", "")
	tokenFile := fs.String("token-file", "", "")
	tokenEnv := fs.Bool("token-env", false, "")
	removeTokenFile := fs.Bool("remove-token-file", false, "")
	mailbox := fs.String("mailbox", "", "")
	workspace := fs.String("workspace", "", "")
	debug := fs.Bool("debug", false, "")
	mode := fs.String("processing-mode", "full", "")
	inbox := fs.String("inbox-folder", "Inbox", "")
	state := fs.String("state", "", "")
	processed := fs.String("processed-folder", "", "")
	errorF := fs.String("error-folder", "", "")
	timeout := fs.Int("timeout", 120, "")
	parallel := fs.Int("parallel", 10, "")
	apiRate := fs.Float64("api-rate", 5.0, "")
	apiBurst := fs.Int("api-burst", 10, "")
	retries := fs.Int("max-retries", 2, "")
	backoff := fs.Int("initial-backoff-seconds", 5, "")
	chunk := fs.Int("chunk-size-mb", 8, "")
	large := fs.Int("large-attachment-threshold-mb", 20, "")
	band := fs.Float64("bandwidth-limit-mbs", 0, "")
	convert := fs.String("convert-body", "none", "")
	chromium := fs.String("chromium-path", "", "")
	msgH := fs.String("msg-handler", "raw", "")
	extL1 := fs.String("attachment-extraction-l1", "default", "")
	health := fs.Bool("healthcheck", false, "")
	details := fs.String("message-details", "", "")
	maxExec := fs.Int("max-execution-time-msg", 120, "")

	dummyStr := ""
	cfg := &engine.Config{}
	cfg.SetDefaults()

	// Set ALL flags
	_ = fs.Parse([]string{
		"-token-string", "ts",
		"-token-file", "tf",
		"-token-env",
		"-remove-token-file",
		"-mailbox", "m@t.com",
		"-workspace", "wp",
		"-debug",
		"-processing-mode", "route",
		"-inbox-folder", "if",
		"-state", "sf",
		"-processed-folder", "pf",
		"-error-folder", "ef",
		"-timeout", "10",
		"-parallel", "5",
		"-api-rate", "2.0",
		"-api-burst", "1",
		"-max-retries", "3",
		"-initial-backoff-seconds", "4",
		"-chunk-size-mb", "1",
		"-large-attachment-threshold-mb", "2",
		"-bandwidth-limit-mbs", "0.5",
		"-convert-body", "text",
		"-chromium-path", "cp",
		"-msg-handler", "extractor",
		"-attachment-extraction-l1", "inlines",
		"-healthcheck",
		"-message-details", "md",
		"-max-execution-time-msg", "60",
	})

	flags := &cliFlags{
		configPath:                 &dummyStr,
		tokenString:                tokenString,
		tokenFile:                  tokenFile,
		tokenEnv:                   tokenEnv,
		removeTokenFile:            removeTokenFile,
		mailboxName:                mailbox,
		workspacePath:              workspace,
		debug:                      debug,
		processingMode:             mode,
		inboxFolder:                inbox,
		stateFilePath:              state,
		processedFolder:            processed,
		errorFolder:                errorF,
		timeoutSeconds:             timeout,
		maxParallelDownloads:       parallel,
		apiCallsPerSecond:          apiRate,
		apiBurst:                   apiBurst,
		maxRetries:                 retries,
		initialBackoffSeconds:      backoff,
		chunkSizeMB:                chunk,
		largeAttachmentThresholdMB: large,
		bandwidthLimitMBs:          band,
		convertBody:                convert,
		chromiumPath:               chromium,
		msgHandler:                 msgH,
		attachmentExtractionL1:     extL1,
		healthCheck:                health,
		messageDetailsFolder:       details,
		maxExecutionTimeMsg:        maxExec,
	}

	overrideConfigWithFlagsLocal(cfg, fs, flags)

	assert.Equal(t, "ts", cfg.TokenString)
	assert.Equal(t, "tf", cfg.TokenFile)
	assert.True(t, cfg.TokenEnv)
	assert.True(t, cfg.RemoveTokenFile)
	assert.Equal(t, "m@t.com", cfg.MailboxName)
	assert.Equal(t, "wp", cfg.WorkspacePath)
	assert.True(t, cfg.DebugLogging)
	assert.Equal(t, "route", cfg.ProcessingMode)
	assert.Equal(t, "if", cfg.InboxFolder)
	assert.Equal(t, "sf", cfg.StateFilePath)
	assert.Equal(t, "pf", cfg.ProcessedFolder)
	assert.Equal(t, "ef", cfg.ErrorFolder)
	assert.Equal(t, 10, cfg.HTTPClientTimeoutSeconds)
	assert.Equal(t, 5, cfg.MaxParallelDownloads)
	assert.Equal(t, 2.0, cfg.APICallsPerSecond)
	assert.Equal(t, 1, cfg.APIBurst)
	assert.Equal(t, 3, cfg.MaxRetries)
	assert.Equal(t, 4, cfg.InitialBackoffSeconds)
	assert.Equal(t, 1, cfg.ChunkSizeMB)
	assert.Equal(t, 2, cfg.LargeAttachmentThresholdMB)
	assert.Equal(t, 0.5, cfg.BandwidthLimitMBs)
	assert.Equal(t, "text", cfg.ConvertBody)
	assert.Equal(t, "cp", cfg.ChromiumPath)
	assert.Equal(t, "extractor", cfg.MsgHandler)
	assert.Equal(t, "inlines", cfg.AttachmentExtractionL1)
	assert.True(t, cfg.HealthCheck)
	assert.Equal(t, "md", cfg.MessageDetailsFolder)
	assert.Equal(t, 60, cfg.MaxExecutionTimeMsg)
}

func TestCheckLongPathSupportMock(t *testing.T) {
	orig := checkLongPathSupport
	defer func() { checkLongPathSupport = orig }()

	called := false
	checkLongPathSupport = func() {
		called = true
	}

	checkLongPathSupport()
	assert.True(t, called)
}

func TestRun(t *testing.T) {
	orig := checkLongPathSupport
	defer func() { checkLongPathSupport = orig }()
	checkLongPathSupport = func() {} // Mock it out

	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{
			name:    "Version flag",
			args:    []string{"o365mbx", "-version"},
			wantErr: false,
		},
		{
			name:    "Missing required flags",
			args:    []string{"o365mbx"},
			wantErr: true,
		},
		{
			name:    "Invalid email",
			args:    []string{"o365mbx", "-mailbox", "invalid", "-token-string", "t", "-workspace", "w"},
			wantErr: true,
		},
		{
			name:    "Healthcheck mode",
			args:    []string{"o365mbx", "-mailbox", "test@test.com", "-token-string", "t", "-healthcheck"},
			wantErr: true, // Will fail because it tries to create a client with a dummy token
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := run(tt.args, io.Discard)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestRun_TokenFileRemoval(t *testing.T) {
	ctx := context.Background()
	masterKeyHex, errKey := libsecsecrets.GenerateKey()
	require.NoError(t, errKey)
	masterKeyBytes, errRes := libsecsecrets.ResolveKey(ctx, masterKeyHex, "", "")
	require.NoError(t, errRes)

	encryptedToken, errEnc := libsecsecrets.Encrypt(ctx, "test-token", masterKeyBytes)
	require.NoError(t, errEnc)

	tokenFile := "temp-token-removal.txt"
	_ = os.WriteFile(tokenFile, []byte(encryptedToken), 0o600)
	// We don't defer removal here because we want to test if run() removes it

	args := []string{"o365mbx", "-mailbox", "test@test.com", "-token-file", tokenFile, "-secret-master-key", masterKeyHex, "-remove-token-file", "-workspace", "test-wp"}

	_ = run(args, io.Discard)

	_, err := os.Stat(tokenFile)
	assert.True(t, os.IsNotExist(err), "Token file should have been removed")
}
