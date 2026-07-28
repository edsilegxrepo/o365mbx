// Package main is the entry point for the o365mbx application.
//
// OBJECTIVE:
// This application is designed to download and process email messages and attachments
// from a Microsoft 365 (O365) mailbox using the Microsoft Graph API. It supports
// full, incremental, and "route" (move-after-process) modes, along with health checks
// and various body conversion options (Text, PDF).
//
// CORE SECTIONS:
// 1. Argument Parsing: Uses flag.FlagSet to handle CLI arguments and configuration overrides.
// 2. Configuration: Loads settings from JSON or CLI, validates them, and manages authentication tokens.
// 3. Dependency Injection: Initializes core services (O365Client, EmailProcessor, FileHandler) and injects them into the Engine.
// 4. Execution: Runs either the diagnostic Health Check or the primary download Engine (RunEngine).
//
// CORE FUNCTIONALITY:
// - Parallelized Extraction: Downloads multiple messages and attachments concurrently.
// - Content Transformation: Sanitizes HTML and optionally converts bodies to PDF or Text.
// - Reliable Storage: Persists data to a structured workspace with detailed metadata and state tracking.
// - Mailbox Management: Automates message relocation (routing) based on processing success or failure.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"criticalsys.net/o365mbx/apperrors"
	"criticalsys.net/o365mbx/downloader"
	"criticalsys.net/o365mbx/engine"

	log "github.com/sirupsen/logrus"
)

var version = "dev"

func main() {
	if err := run(os.Args, os.Stdout); err != nil {
		exitCode := apperrors.GetExitCode(err)
		log.Errorf("Application failed (exit code %d): %v", exitCode, err)
		os.Exit(exitCode)
	}
}

// run is the internal entry point that handles flag parsing, configuration setup,
// and execution of either the diagnostic health check or the main engine.
func run(args []string, out io.Writer) error {
	// --- Pre-flight Checks ---
	checkLongPathSupport()

	// --- Flag Definition ---
	// We use a local FlagSet to make run() testable and avoid global state issues.
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(out)

	configPath := fs.String("config", "", "Path to a JSON configuration file.")
	tokenString := fs.String("token-string", "", "JWT token as a string.")
	tokenFile := fs.String("token-file", "", "Path to a file containing the JWT token.")
	tokenEnv := fs.Bool("token-env", false, "Read JWT token from JWT_TOKEN environment variable.")
	removeTokenFile := fs.Bool("remove-token-file", false, "Remove the token file after use (only if -token-file is specified).")
	tenantID := fs.String("tenant-id", "", "Microsoft Entra ID Tenant ID.")
	clientID := fs.String("client-id", "", "Application / Client ID.")
	clientSecret := fs.String("client-secret", "", "Plaintext Client Secret.")
	clientSecretFile := fs.String("client-secret-file", "", "Path to AES-256-GCM encrypted Client Secret file via secretprotector.")
	clientSecretEnv := fs.Bool("client-secret-env", false, "Read encrypted Client Secret from CLIENT_SECRET environment variable.")
	secretMasterKey := fs.String("secret-master-key", "", "Raw 64-character hex master key for secretprotector AES decryption.")
	secretMasterKeyEnv := fs.String("secret-master-key-env", "SECRETPROTECTOR_MASTER_KEY", "Environment variable name containing the master key for secretprotector.")
	secretMasterKeyFile := fs.String("secret-master-key-file", "", "File path containing the master key for secretprotector.")
	mailboxName := fs.String("mailbox", "", "Mailbox name (e.g., name@domain.com)")
	workspacePath := fs.String("workspace", "", "Unique folder to store all artifacts")
	displayVersion := fs.Bool("version", false, "Display application version")
	healthCheck := fs.Bool("healthcheck", false, "Perform a health check on the mailbox and exit")
	messageDetailsFolder := fs.String("message-details", "", "When used with -healthcheck, displays message details for the specified folder.")
	debug := fs.Bool("debug", false, "Enable debug logging")
	processingMode := fs.String("processing-mode", "full", "Processing mode: 'full', 'incremental', or 'route'.")
	inboxFolder := fs.String("inbox-folder", "Inbox", "The source folder from which to process messages.")
	stateFilePath := fs.String("state", "", "Path to the state file for incremental processing")
	processedFolder := fs.String("processed-folder", "", "Destination folder for successfully processed messages in route mode.")
	errorFolder := fs.String("error-folder", "", "Destination folder for messages that failed processing in route mode.")
	timeoutSeconds := fs.Int("timeout", 120, "HTTP client timeout in seconds.")
	maxParallelDownloads := fs.Int("parallel", 10, "Maximum number of parallel downloads.")
	apiCallsPerSecond := fs.Float64("api-rate", 5.0, "API calls per second for client-side rate limiting.")
	apiBurst := fs.Int("api-burst", 10, "API burst capacity for client-side rate limiting.")
	maxRetries := fs.Int("max-retries", 2, "Maximum number of retries for failed API calls.")
	initialBackoffSeconds := fs.Int("initial-backoff-seconds", 5, "Initial backoff in seconds for retries.")
	chunkSizeMB := fs.Int("chunk-size-mb", 8, "Chunk size in MB for large attachment downloads.")
	largeAttachmentThresholdMB := fs.Int("large-attachment-threshold-mb", 20, "Threshold in MB for large attachments.")
	bandwidthLimitMBs := fs.Float64("bandwidth-limit-mbs", 0, "Bandwidth limit in MB/s for downloads (0 for disabled).")
	convertBody := fs.String("convert-body", "none", "Convert body to 'text' or 'pdf'. Default is 'none'.")
	chromiumPath := fs.String("chromium-path", "", "Path to headless chromium binary for PDF conversion.")
	msgHandler := fs.String("msg-handler", "raw", "Handler for .msg/.eml attachments: 'raw' or 'extractor'.")
	attachmentExtractionL1 := fs.String("attachment-extraction-l1", "default", "Level 1 extraction for .msg/.eml: 'default' (attachments only) or 'inlines' (attachments + inlines).")
	maxExecutionTimeMsg := fs.Int("max-execution-time-msg", 120, "Maximum time in seconds to spend on one email message.")

	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	// --- Configuration Loading ---
	cfg, err := engine.LoadConfig(*configPath)
	if err != nil {
		return fmt.Errorf("error loading configuration: %w", err)
	}

	// --- Early Exit flags ---
	if *displayVersion {
		_, _ = fmt.Fprintf(out, "O365 Mailbox Downloader Version: %s\n", version)
		return nil
	}

	flags := &cliFlags{
		configPath:                 configPath,
		tokenString:                tokenString,
		tokenFile:                  tokenFile,
		tokenEnv:                   tokenEnv,
		removeTokenFile:            removeTokenFile,
		tenantID:                   tenantID,
		clientID:                   clientID,
		clientSecret:               clientSecret,
		clientSecretFile:           clientSecretFile,
		clientSecretEnv:            clientSecretEnv,
		secretMasterKey:            secretMasterKey,
		secretMasterKeyEnv:         secretMasterKeyEnv,
		secretMasterKeyFile:        secretMasterKeyFile,
		mailboxName:                mailboxName,
		workspacePath:              workspacePath,
		debug:                      debug,
		processingMode:             processingMode,
		inboxFolder:                inboxFolder,
		stateFilePath:              stateFilePath,
		processedFolder:            processedFolder,
		errorFolder:                errorFolder,
		timeoutSeconds:             timeoutSeconds,
		maxParallelDownloads:       maxParallelDownloads,
		apiCallsPerSecond:          apiCallsPerSecond,
		apiBurst:                   apiBurst,
		maxRetries:                 maxRetries,
		initialBackoffSeconds:      initialBackoffSeconds,
		chunkSizeMB:                chunkSizeMB,
		largeAttachmentThresholdMB: largeAttachmentThresholdMB,
		bandwidthLimitMBs:          bandwidthLimitMBs,
		convertBody:                convertBody,
		chromiumPath:               chromiumPath,
		msgHandler:                 msgHandler,
		attachmentExtractionL1:     attachmentExtractionL1,
		healthCheck:                healthCheck,
		messageDetailsFolder:       messageDetailsFolder,
		maxExecutionTimeMsg:        maxExecutionTimeMsg,
	}

	// --- Command-line Override ---
	overrideConfigWithFlagsLocal(cfg, fs, flags)

	// --- Logging Setup ---
	log.SetFormatter(&log.TextFormatter{FullTimestamp: true})
	if cfg.DebugLogging {
		log.SetLevel(log.DebugLevel)
		log.Debugln("Debug logging enabled.")
	} else {
		log.SetLevel(log.InfoLevel)
	}

	// --- Configuration Validation ---
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	// --- Context and Signal Handling ---
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig, ok := <-sigChan
		if ok {
			log.WithField("signal", sig).Warn("Received interrupt signal, initiating graceful shutdown...")
			cancel()
		}
	}()

	dl, err := downloader.New(cfg, log.WithFields(log.Fields{}))
	if err != nil {
		return err
	}
	dl.SetVersion(version)

	return dl.Execute(ctx, out)
}

// loadAccessToken wraps downloader.LoadAccessToken to resolve access tokens from configuration.
func loadAccessToken(cfg *engine.Config) (string, error) {
	return downloader.LoadAccessToken(cfg)
}

// isValidEmail wraps downloader.IsValidEmail to validate email address syntax.
func isValidEmail(email string) bool {
	return downloader.IsValidEmail(email)
}

// validateFinalConfig wraps downloader.ValidateFinalConfig to validate runtime config constraints.
func validateFinalConfig(cfg *engine.Config) error {
	return downloader.ValidateFinalConfig(cfg)
}

// cliFlags holds references to parsed command-line flags.
type cliFlags struct {
	configPath                 *string
	tokenString                *string
	tokenFile                  *string
	tokenEnv                   *bool
	removeTokenFile            *bool
	tenantID                   *string
	clientID                   *string
	clientSecret               *string
	clientSecretFile           *string
	clientSecretEnv            *bool
	secretMasterKey            *string
	secretMasterKeyEnv         *string
	secretMasterKeyFile        *string
	mailboxName                *string
	workspacePath              *string
	debug                      *bool
	processingMode             *string
	inboxFolder                *string
	stateFilePath              *string
	processedFolder            *string
	errorFolder                *string
	timeoutSeconds             *int
	maxParallelDownloads       *int
	apiCallsPerSecond          *float64
	apiBurst                   *int
	maxRetries                 *int
	initialBackoffSeconds      *int
	chunkSizeMB                *int
	largeAttachmentThresholdMB *int
	bandwidthLimitMBs          *float64
	convertBody                *string
	chromiumPath               *string
	msgHandler                 *string
	attachmentExtractionL1     *string
	healthCheck                *bool
	messageDetailsFolder       *string
	maxExecutionTimeMsg        *int
}

func overrideConfigWithFlagsLocal(cfg *engine.Config, fs *flag.FlagSet, flags *cliFlags) {
	// Override config file settings with any flags set on the command line.
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "token-string":
			cfg.TokenString = *flags.tokenString
		case "token-file":
			cfg.TokenFile = *flags.tokenFile
		case "token-env":
			cfg.TokenEnv = *flags.tokenEnv
		case "remove-token-file":
			cfg.RemoveTokenFile = *flags.removeTokenFile
		case "tenant-id":
			cfg.TenantID = *flags.tenantID
		case "client-id":
			cfg.ClientID = *flags.clientID
		case "client-secret":
			cfg.ClientSecret = *flags.clientSecret
		case "client-secret-file":
			cfg.ClientSecretFile = *flags.clientSecretFile
		case "client-secret-env":
			cfg.ClientSecretEnv = *flags.clientSecretEnv
		case "secret-master-key":
			cfg.SecretMasterKey = *flags.secretMasterKey
		case "secret-master-key-env":
			cfg.SecretMasterKeyEnv = *flags.secretMasterKeyEnv
		case "secret-master-key-file":
			cfg.SecretMasterKeyFile = *flags.secretMasterKeyFile
		case "mailbox":
			cfg.MailboxName = *flags.mailboxName
		case "workspace":
			cfg.WorkspacePath = *flags.workspacePath
		case "debug":
			cfg.DebugLogging = *flags.debug
		case "processing-mode":
			cfg.ProcessingMode = *flags.processingMode
		case "inbox-folder":
			cfg.InboxFolder = *flags.inboxFolder
		case "state":
			cfg.StateFilePath = *flags.stateFilePath
		case "processed-folder":
			cfg.ProcessedFolder = *flags.processedFolder
		case "error-folder":
			cfg.ErrorFolder = *flags.errorFolder
		case "timeout":
			cfg.HTTPClientTimeoutSeconds = *flags.timeoutSeconds
		case "parallel":
			cfg.MaxParallelDownloads = *flags.maxParallelDownloads
		case "api-rate":
			cfg.APICallsPerSecond = *flags.apiCallsPerSecond
		case "api-burst":
			cfg.APIBurst = *flags.apiBurst
		case "max-retries":
			cfg.MaxRetries = *flags.maxRetries
		case "initial-backoff-seconds":
			cfg.InitialBackoffSeconds = *flags.initialBackoffSeconds
		case "chunk-size-mb":
			cfg.ChunkSizeMB = *flags.chunkSizeMB
		case "large-attachment-threshold-mb":
			cfg.LargeAttachmentThresholdMB = *flags.largeAttachmentThresholdMB
		case "bandwidth-limit-mbs":
			cfg.BandwidthLimitMBs = *flags.bandwidthLimitMBs
		case "convert-body":
			cfg.ConvertBody = *flags.convertBody
		case "chromium-path":
			cfg.ChromiumPath = *flags.chromiumPath
		case "msg-handler":
			cfg.MsgHandler = *flags.msgHandler
		case "attachment-extraction-l1":
			cfg.AttachmentExtractionL1 = *flags.attachmentExtractionL1
		case "healthcheck":
			cfg.HealthCheck = *flags.healthCheck
		case "message-details":
			cfg.MessageDetailsFolder = *flags.messageDetailsFolder
		case "max-execution-time-msg":
			cfg.MaxExecutionTimeMsg = *flags.maxExecutionTimeMsg
		}
	})
}
