# Changelog

All notable changes to the `o365mbx` project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [1.1.0] - 2026-07-28

### Added
* **OAuth2 Client Credentials Flow & Automatic Token Refresh (`o365client/auth`)**:
  * Implemented native support for Microsoft Entra ID Client Credentials grant via `TenantID`, `ClientID`, and `ClientSecret` (or `ClientSecretFile` / `ClientSecretEnv` encrypted via `secretprotector`).
  * Added `ClientCredentialsAuthenticationProvider` (`./o365client/auth.go`) featuring thread-safe token caching (`sync.RWMutex`) and **proactive background token refresh** (refreshes 5 minutes prior to expiration or upon detecting expired tokens).
* **CLI Flags for Client Credentials (`main.go`)**:
  * Added CLI flags `-tenant-id`, `-client-id`, `-client-secret`, `-client-secret-file`, and `-client-secret-env`.
* **Service Gateway Integration (`service-gateway`)**:
  * Completed full integration into `service-gateway` via `O365MBXService` (`../service-gateway/internal/services/o365mbx/handler.go`), mapping REST API operations (`/op/{opName}`) to `o365mbx/downloader`.
* **Full Token Lifecycle Test Suite (`o365client_test.go` & `resilience_test.go`)**:
  * Added unit, error branch, provider resolution, and Microsoft Dev Proxy resilience tests for initial token acquisition, expiration handling, and auto-refresh during live API calls.

### Fixed
* **Accurate Status Summary Counts & Mailbox Stats (`engine/engine.go` & `filehandler/filehandler.go`) [GH-38]**:
  * Fixed an issue where `job_processed_count` and `job_error_count` remained 0 in `status_*.json` summary reports during `full` and `incremental` sync modes by adding atomic stat tracking across all non-route processing branches.
  * Added fallback initialization for `sourceCounts` map to ensure valid JSON formatting (`"source_mailbox_counts": {}`) when initial mailbox stats retrieval returns `nil`.
* **Accurate Attachment Counts in Metadata (`filehandler/filehandler.go`) [GH-39]**:
  * Fixed an issue where `metadata.json` reported `"attachment_counts": 0` despite downloaded attachments by setting `metadata.AttachmentCount = len(attachments)` during the final atomic metadata JSON update.

---

## [1.0.0] - 2026-07-27

### Refactored & Architecture
* **Downstream Integration Library Decoupling (`o365mbx/downloader`)**:
  * Major architectural refactoring decoupling core mailbox sync execution from CLI flag parsing into the high-level reusable `downloader` package.
  * Enables native, programmatic integration into downstream Go microservices (such as `service-gateway`) via `downloader.Run(ctx, cfg)` or `downloader.New(cfg, logger)` without subprocess execution overhead or code duplication.
  * Encapsulates token lifecycle resolution, dependency injection (`o365client`, `emailprocessor`, `filehandler`), and browser pool lifecycle management.

### Added
* **Zero-Trust Credential Security (`secretprotector` Integration)**:
  * Enforced mandatory AES-256-GCM authenticated encryption at rest via `libsecsecrets` for stored secrets (`-token-file` and `-token-env` / `JWT_TOKEN`).
  * Added Master Key resolution hierarchy (`-secret-master-key`, `-secret-master-key-env`, `-secret-master-key-file`) with strict OS permission enforcement (`0400`/`0600` on Linux, non-temp paths on Windows) and instant RAM key buffer zeroing (`libsecsecrets.ZeroBuffer`).
* **Headless Browser PDF Generation (`emailprocessor`)**:
  * Migrated from `go-rod` to official `chromedp` driver with long-lived browser singleton, incognito tab isolation, and Chrome daemon support.
* **Reusable Go Library Package (`o365mbx/downloader`)**:
  * Decoupled core engine execution from CLI flag parsing into a high-level library package (`downloader`).
  * Enables seamless embedding into external Go microservices (such as `service-gateway`) using `downloader.Run(ctx, cfg)` or `downloader.New(cfg, logger)`.
* **Granular Process Exit Codes (`apperrors`)**:
  * Implemented standardized diagnostic exit codes (`ExitSuccess=0`, `ExitConfigError=2`, `ExitAuthError=10`, `ExitAPIError=20`, `ExitFileSystemError=30`, `ExitInterrupted=130`).
  * Updated `main.go` to exit with `apperrors.GetExitCode(err)` for automated orchestrator diagnostics (Kubernetes, Airflow, systemd).
* **Always-Running Chrome Daemon Support**:
  * Added DevTools WebSocket URL connection support (`ws://127.0.0.1:9222` or `http://...`) in `emailprocessor`.
  * Allows connecting directly to pre-launched Chrome/Chromium daemons on Windows (`d:\inet\www\chromium\bin\chrome.exe`) and Linux (`/u01/chromium/chrome`), eliminating process startup overhead.
* **Terminal & Presenter Output Sanitization**:
  * Added `utils.SanitizeControlCharacters` to strip ANSI escape sequences and non-printable control characters (`\x00`–`\x1F`) before rendering terminal tabwriter output.
* **Bandwidth Throttling & Live Resilience Tests**:
  * Added `-bandwidth-limit-mbs` rate limiting and live Dev Proxy resilience tests for bandwidth caps (`TestResilience_BandwidthLimiter`), per-message timeouts (`TestResilience_PerMessageTimeout`), and PDF body conversions (`TestResilience_BodyConversionHTMLToPDF`).

### Fixed & Hardened
* **Atomic File System Persistence (`filehandler`)**:
  * Updated `SaveState` to construct temporary files inside `filepath.Dir(stateFilePath)` to prevent `EXDEV` cross-device mount errors on Linux.
  * Updated `WriteAttachmentsToMetadata` to perform atomic write-and-rename (`metadata.json.tmp` ➔ `metadata.json`) via `os.OpenRoot`.
* **Chrome Process Lifecycle & Memory Safety**:
  * Ensured native process tree termination via `ep.launcher.Kill()` and `ep.launcher.Cleanup()` on `EmailProcessor.Close()`.
  * Enforced V8 heap browser recycling after 1,000 conversions and added `cleanupMutex` map eviction.
* **Goroutine & Channel Drain Safety (`engine`)**:
  * Select-guarded worker semaphore acquisition against `<-ctx.Done()` to guarantee instant goroutine shutdown on cancellation.
* **Test Export Encapsulation**:
  * Moved test helper exports into `filehandler/export_test.go`, keeping production binaries 100% free of exported test hooks.

---

## [0.1.8] - 2026-03-12

### Added
* **High-Fidelity `.msg` & `.eml` Extraction**:
  * Added `extractor` handler (`-msg-handler extractor`) to parse nested ItemAttachments via raw RFC822 MIME streams (`enmime`).
  * Added Level 1 attachment and inline image separation (`-attachment-extraction-l1`).
* **Nil-Safe Dereferencing & Subject Handling**:
  * Resolved `panic: SIGSEGV` crashes on missing/null subject emails (`utils.StringValue`).
* **Code Coverage Expansion**:
  * Expanded statement coverage across all packages to >91% with zero-config Dev Proxy auto-management.

### Fixed
* Fixed Kiota `UntypedNode` and `UntypedNumber` deserialization for Graph API folder size numbers.
* Corrected default folder handling in `route` processing mode.
* Updated module dependencies and linter rules.

---

## [0.1.7] - 2026-03-03

### Added
* **Health Check & Message Details Diagnostic Modes**:
  * Added `-healthcheck` flag to verify authentication and output folder statistics.
  * Added `-message-details <folder>` streaming mode for inspecting folder message metadata without downloading.
* **JSON Configuration File Support**:
  * Added `-config` flag to load settings from JSON configuration files with flag override synchronization.

---

## [0.1.0] - 2025-08-25

### Added
* Initial project release with Microsoft Graph SDK for Go.
* Core producer-consumer engine and incremental delta sync (`-processing-mode incremental`).
