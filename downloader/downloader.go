// Package downloader provides the high-level reusable library entry point for o365mbx.
//
// OBJECTIVE:
// Provide a clean, programmatic Go library API for embedding o365mbx into downstream microservices
// (e.g. service-gateway) without requiring CLI flag parsing or process execution.
//
// CORE COMPONENTS:
// 1. Downloader: High-level downloader instance managing configuration and dependencies.
// 2. ResolveAuthProvider: Auth resolver supporting OAuth2 Client Credentials and Static Tokens (encrypted or raw).
// 3. LoadAccessToken: Zero-Trust secret resolution supporting secretprotector AES-256-GCM decryption.
//
// CORE FUNCTIONALITY:
// - Manages access token resolution and Client Credentials auto-refresh.
// - Initializes dependency graph (O365Client, EmailProcessor, FileHandler).
// - Manages browser pool lifecycles for HTML-to-PDF rendering.
// - Executes Health Check diagnostic runs and primary Engine sync runs.
//
// DATA FLOW:
// Downloader.Run(ctx, cfg) -> ResolveAuthProvider -> NewO365Client -> NewFileHandler -> NewEmailProcessor -> Engine.RunEngine
//
// USAGE AS A LIBRARY (e.g. in service-gateway):
//
//	import (
//	    "context"
//	    "o365mbx/downloader"
//	    "o365mbx/engine"
//	)
//
//	cfg := &engine.Config{
//	    MailboxName:   "user@company.com",
//	    WorkspacePath: "/var/data/workspace",
//	    TokenString:   "eyJhbG...",
//	}
//	err := downloader.Run(ctx, cfg)
package downloader

import (
	"context"
	"fmt"
	"io"
	// #nosec G404 - math/rand is used exclusively for exponential backoff jitter and retry timing, not cryptographic security.
	// nosemgrep: go.lang.security.audit.crypto.math_random.math-random-used
	"math/rand" // nosemgrep: go.lang.security.audit.crypto.math_random.math-random-used
	"os"
	"regexp"
	"strings"
	"time"

	"criticalsys.net/o365mbx/apperrors"
	"criticalsys.net/o365mbx/emailprocessor"
	"criticalsys.net/o365mbx/engine"
	"criticalsys.net/o365mbx/filehandler"
	"criticalsys.net/o365mbx/o365client"
	"criticalsys.net/o365mbx/presenter"
	"criticalsys.net/secretprotector/pkg/libsecsecrets"

	log "github.com/sirupsen/logrus"
)

var emailRegex = regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)

// IsValidEmail returns true if the given string is a valid email address format.
func IsValidEmail(email string) bool {
	return emailRegex.MatchString(email)
}

// Downloader is the high-level application component that manages configuration validation,
// dependency injection, access token resolution, and execution orchestration.
type Downloader struct {
	cfg     *engine.Config
	logger  *log.Entry
	version string
}

// New creates a new Downloader library instance given a configuration and optional logger.
func New(cfg *engine.Config, logger *log.Entry) (*Downloader, error) {
	if cfg == nil {
		return nil, fmt.Errorf("configuration cannot be nil")
	}
	if logger == nil {
		logger = log.WithFields(log.Fields{})
	}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	return &Downloader{
		cfg:     cfg,
		logger:  logger,
		version: "1.0.0",
	}, nil
}

// SetVersion overrides the application version reported in logs and status files.
func (d *Downloader) SetVersion(v string) {
	if v != "" {
		d.version = v
	}
}

// LoadAccessToken resolves the access token from literal string, token file, or environment variable.
// Streamed in-memory tokens (-token-string) are passed directly.
// Stored tokens (-token-file or -token-env) MUST be encrypted with secretprotector (AES-256-GCM).
func LoadAccessToken(cfg *engine.Config) (string, error) {
	sourceCount := 0
	if cfg.TokenString != "" {
		sourceCount++
	}
	if cfg.TokenFile != "" {
		sourceCount++
	}
	if cfg.TokenEnv {
		sourceCount++
	}

	if sourceCount == 0 {
		return "", fmt.Errorf("no token source specified. Please use one of TokenString, TokenFile, or TokenEnv")
	}
	if sourceCount > 1 {
		return "", fmt.Errorf("multiple token sources specified. Please use only one of TokenString, TokenFile, or TokenEnv")
	}

	if cfg.TokenString != "" {
		log.Info("Using in-memory token from TokenString.")
		return cfg.TokenString, nil
	}

	// For stored tokens (-token-file or -token-env), resolve secretprotector master key
	ctx := context.Background()
	masterKey, err := libsecsecrets.ResolveKey(ctx, cfg.SecretMasterKey, cfg.SecretMasterKeyEnv, cfg.SecretMasterKeyFile)
	if err != nil {
		return "", &apperrors.APIError{
			StatusCode: 401,
			Msg:        fmt.Sprintf("security policy violation: master key required for stored secrets: %v", err),
		}
	}
	defer libsecsecrets.ZeroBuffer(masterKey)

	var rawCiphertext string
	if cfg.TokenFile != "" {
		log.Info("Loading encrypted token from TokenFile via secretprotector.")
		content, readErr := os.ReadFile(cfg.TokenFile)
		if readErr != nil {
			return "", fmt.Errorf("failed to read token file %s: %w", cfg.TokenFile, readErr)
		}
		rawCiphertext = strings.TrimSpace(string(content))
	} else if cfg.TokenEnv {
		log.Info("Loading encrypted token from JWT_TOKEN environment variable via secretprotector.")
		rawCiphertext = strings.TrimSpace(os.Getenv("JWT_TOKEN"))
		if rawCiphertext == "" {
			return "", fmt.Errorf("TokenEnv specified, but JWT_TOKEN environment variable is not set")
		}
	}

	decryptedToken, err := libsecsecrets.Decrypt(ctx, rawCiphertext, masterKey)
	if err != nil {
		return "", &apperrors.APIError{
			StatusCode: 401,
			Msg:        fmt.Sprintf("security policy violation: stored token must be an AES-256-GCM encrypted ciphertext created by secretprotector: %v", err),
		}
	}

	return decryptedToken, nil
}

// ResolveAuthProvider determines whether to create a ClientCredentialsAuthenticationProvider or StaticTokenAuthenticationProvider based on cfg.
func ResolveAuthProvider(cfg *engine.Config) (o365client.AuthenticationProvider, error) {
	if cfg.TenantID != "" {
		log.Info("Initializing Client Credentials Authentication Provider with auto-refresh.")
		clientSecret := cfg.ClientSecret
		if clientSecret == "" {
			ctx := context.Background()
			masterKey, err := libsecsecrets.ResolveKey(ctx, cfg.SecretMasterKey, cfg.SecretMasterKeyEnv, cfg.SecretMasterKeyFile)
			if err != nil {
				return nil, &apperrors.APIError{
					StatusCode: 401,
					Msg:        fmt.Sprintf("security policy violation: master key required for encrypted client secret: %v", err),
				}
			}
			defer libsecsecrets.ZeroBuffer(masterKey)

			var rawCiphertext string
			if cfg.ClientSecretFile != "" {
				content, readErr := os.ReadFile(cfg.ClientSecretFile)
				if readErr != nil {
					return nil, fmt.Errorf("failed to read client secret file %s: %w", cfg.ClientSecretFile, readErr)
				}
				rawCiphertext = strings.TrimSpace(string(content))
			} else if cfg.ClientSecretEnv {
				rawCiphertext = strings.TrimSpace(os.Getenv("CLIENT_SECRET"))
				if rawCiphertext == "" {
					return nil, fmt.Errorf("ClientSecretEnv specified, but CLIENT_SECRET environment variable is not set")
				}
			}

			decryptedSecret, err := libsecsecrets.Decrypt(ctx, rawCiphertext, masterKey)
			if err != nil {
				return nil, &apperrors.APIError{
					StatusCode: 401,
					Msg:        fmt.Sprintf("security policy violation: stored client secret must be an AES-256-GCM encrypted ciphertext created by secretprotector: %v", err),
				}
			}
			clientSecret = decryptedSecret
		}

		return o365client.NewClientCredentialsAuthenticationProvider(cfg.TenantID, cfg.ClientID, clientSecret)
	}

	token, err := LoadAccessToken(cfg)
	if err != nil {
		return nil, err
	}
	return o365client.NewStaticTokenAuthenticationProvider(token)
}

// ValidateFinalConfig performs cross-field validation on the configuration object.
func ValidateFinalConfig(cfg *engine.Config) error {
	if cfg.MailboxName == "" {
		return fmt.Errorf("mailbox name is a required argument")
	}
	if !IsValidEmail(cfg.MailboxName) {
		return fmt.Errorf("invalid mailbox name format: %s", cfg.MailboxName)
	}
	if !cfg.HealthCheck && cfg.WorkspacePath == "" {
		return fmt.Errorf("workspace path is a required argument")
	}
	if cfg.ProcessingMode == "incremental" && cfg.StateFilePath == "" {
		return fmt.Errorf("state file path must be provided for incremental processing mode")
	}
	if cfg.ProcessingMode == "route" {
		if cfg.ProcessedFolder == "" {
			return fmt.Errorf("processed folder name must be provided for route mode")
		}
		if cfg.ErrorFolder == "" {
			return fmt.Errorf("error folder name must be provided for route mode")
		}
	}
	return nil
}

// Execute performs complete execution of either the diagnostic health check or primary engine pipeline.
func (d *Downloader) Execute(ctx context.Context, out io.Writer) error {
	if out == nil {
		out = os.Stdout
	}

	// Authentication Provider Resolution (Static Token or Client Credentials with Auto-Refresh)
	authProvider, err := ResolveAuthProvider(d.cfg)
	if err != nil {
		return fmt.Errorf("error resolving authentication provider: %w", err)
	}

	// Validation
	if err := ValidateFinalConfig(d.cfg); err != nil {
		return err
	}

	// #nosec G404 - math/rand is used for jitter/backoff, not security
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	// Dependency Injection
	o365Client, err := o365client.NewO365ClientWithAuthProvider(authProvider, rng)
	if err != nil {
		return fmt.Errorf("error creating O365 client: %w", err)
	}

	emailProcessor := emailprocessor.NewEmailProcessor(d.logger)
	if d.cfg.ConvertBody == "pdf" {
		if err := emailProcessor.Initialize(ctx, d.cfg.ChromiumPath, d.cfg.MaxParallelDownloads); err != nil {
			return fmt.Errorf("failed to initialize email processor: %w", err)
		}
		defer func() {
			if err := emailProcessor.Close(); err != nil {
				d.logger.Errorf("Error closing email processor: %v", err)
			}
		}()
	}

	fileHandler := filehandler.NewFileHandler(
		d.cfg.WorkspacePath,
		o365Client,
		emailProcessor,
		d.cfg.LargeAttachmentThresholdMB,
		d.cfg.ChunkSizeMB,
		d.cfg.BandwidthLimitMBs,
		d.cfg.MsgHandler,
		d.cfg.AttachmentExtractionL1,
		d.logger,
	)

	// Health Check or Engine Run
	if d.cfg.HealthCheck {
		if d.cfg.MessageDetailsFolder != "" {
			err = presenter.RunMessageDetailsMode(ctx, o365Client, d.cfg.MailboxName, d.cfg.MessageDetailsFolder, out)
		} else {
			err = presenter.RunHealthCheckMode(ctx, o365Client, d.cfg.MailboxName, out)
		}
		if err != nil {
			return fmt.Errorf("diagnostic run failed: %w", err)
		}
		return nil
	}

	defer func() {
		if d.cfg.TokenFile != "" && d.cfg.RemoveTokenFile {
			d.logger.WithField("file", d.cfg.TokenFile).Info("Removing token file as requested.")
			if err := os.Remove(d.cfg.TokenFile); err != nil {
				d.logger.WithField("file", d.cfg.TokenFile).Errorf("Failed to remove token file: %v", err)
			}
		}
	}()

	if err := engine.RunEngine(ctx, d.cfg, o365Client, emailProcessor, fileHandler, d.version); err != nil {
		return fmt.Errorf("engine failed: %w", err)
	}
	return nil
}

// Run is a convenient one-line wrapper for running a mailbox download task with default logger and output.
func Run(ctx context.Context, cfg *engine.Config) error {
	dl, err := New(cfg, log.WithFields(log.Fields{}))
	if err != nil {
		return err
	}
	return dl.Execute(ctx, os.Stdout)
}
