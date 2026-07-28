# O365 Mailbox Downloader

`o365mbx` is a command-line application written in Go to download emails and attachments from Microsoft Office 365 (O365) mailboxes using the Microsoft Graph API. It leverages the official Microsoft Graph SDK for Go to ensure reliable and efficient communication with the API.

It is designed for high-performance, parallelized downloading and is robust and efficient, with features for handling large mailboxes, large attachments, and transient network errors.

## Features

*   **High-Performance Parallel Processing**: Utilizes a decoupled producer-consumer architecture to download multiple messages and attachments concurrently, maximizing throughput within configurable API limits.
*   **Reliable Attachment Handling**: Uses a two-phase download strategy. It first fetches messages and then fetches attachments for each message individually. This approach improves reliability and reduces memory usage, preventing API timeouts when processing mailboxes with large numbers of attachments.
*   **Email and Attachment Download**: Downloads emails and their attachments from a specified O365 mailbox.
*   **HTML to Plain Text Conversion**: Cleans email bodies by converting HTML to plain text, preserving links and image alt text.
*   **Incremental Downloads**: Performs incremental downloads by saving the timestamp of the last run, fetching only new emails since that time.
*   **Robust Error Handling**: Implements custom error types for better error identification and handling.
*   **Configurable Retry Mechanism**: Uses the Microsoft Graph SDK's built-in retry mechanism to handle transient network errors and API rate limiting.
*   **Flexible Configuration**: Supports configuration via both a JSON file and command-line arguments, with arguments overriding file settings.
*   **Bandwidth Limiting**: Allows throttling of download bandwidth to avoid hitting data egress limits on large-scale downloads.
*   **Secure Token Management**: Provides multiple, mutually exclusive options for securely supplying the access token.
*   **Health Check Mode**: Provides a "health check" mode to verify connectivity and authentication with the O365 mailbox without performing a full download.
*   **Per-Message Execution Timeout**: Implements a configurable timeout for processing individual email messages. This prevents the application from hanging on exceptionally large or complex emails (e.g., those with massive HTML bodies or deeply nested structures). If a message exceeds the timeout, it is automatically moved to the error folder, allowing the pipeline to continue with the next item.
*   **Structured Logging**: Uses `logrus` for structured and informative logging, with a configurable debug level.
*   **Comprehensive Error Reporting and Job Tracking**: Generates detailed job-level and message-level JSON reports to facilitate downstream processing and error reconciliation.

## System Requirements

### File Path Length

The application now supports a maximum file path length of 512 characters. On systems where this limit might be an issue (e.g., older Windows versions without long path support), please see the requirements below.

### Windows Long Path Support

Due to the way email subjects and attachment names can create very long file paths, `o365mbx` requires that "Win32 long path" support is enabled on Windows systems. The application will check for this at startup and will exit with an error if it is not enabled.

You can enable this feature using one of the following methods:

**Using the Group Policy Editor (`gpedit.msc`):**
1.  Navigate to: `Local Computer Policy` -> `Computer Configuration` -> `Administrative Templates` -> `System` -> `Filesystem`.
2.  Find and enable the "Enable Win32 long paths" option.

**Using the Registry Editor (`regedit.exe`):**
1.  Navigate to: `HKEY_LOCAL_MACHINE\SYSTEM\CurrentControlSet\Control\FileSystem`.
2.  Set the value of `LongPathsEnabled` (a `DWORD` type) to `1`.

A system restart may be required for the change to take effect.

## Workspace Directory Structure

The application saves each email into a dedicated folder within the specified workspace. The folder is named after the message's unique ID. Here is an example of the directory structure for a single downloaded email:

```
/path/to/your/workspace/
└── AAMkAGI0ZD... (message ID)
    ├── attachments
    │   ├── 01_quarterly_report.pdf
    │   └── 02_logo.png
    ├── body.html
    ├── error.json
    └── metadata.json
```

*   **`metadata.json`**: A JSON file containing detailed metadata about the email, including sender, recipients, subject, date, and information about the attachments.
*   **`body.html` / `body.txt` / `body.pdf`**: The body of the email. The extension depends on the original content type or the conversion option specified.
*   **`attachments/`**: A sub-directory containing all attachments from the email. Each attachment is prefixed with a two-digit sequence number.
*   **`error.json`**: Created only if errors occur during the processing of this specific message. Contains a list of error details with timestamps.

### Error JSON Example

```json
[
  {
    "timestamp": "2026-03-14T05:30:05Z",
    "message": "failed to fetch attachments for message: API error (status: 500): Internal Server Error"
  }
]
```

## Job Status Reporting

At the end of each run, `o365mbx` generates a status report at the root of the workspace to facilitate job tracking and downstream automation. This file is named `status_<timestamp>.json`.

### Status Fields

*   **`mailbox`**: The email address of the target mailbox.
*   **`timestamp`**: The UTC timestamp when the report was generated.
*   **`source_mailbox_counts`**: A snapshot of item counts for all folders in the mailbox at the start of the job.
*   **`job_processed_count`**: The number of messages successfully processed during the current run.
*   **`job_error_count`**: The number of messages that encountered errors and were routed to the error folder.

### Status JSON Example

```json
{
  "mailbox": "user@domain.com",
  "timestamp": "2026-03-14T05:30:00Z",
  "source_mailbox_counts": {
    "Inbox": 150,
    "Processed": 1200,
    "Error": 15
  },
  "job_processed_count": 45,
  "job_error_count": 2
```

## Diagnostic Process Exit Codes

`o365mbx` returns granular diagnostic exit codes on failure to simplify integration with orchestrators (Kubernetes, Airflow, systemd, Cron):

| Exit Code | Constant Name | Description & Cause |
| :---: | :--- | :--- |
| **`0`** | `ExitSuccess` | Process completed successfully with zero fatal errors. |
| **`2`** | `ExitConfigError` | Invalid CLI flags, missing required configuration options, or invalid email format. |
| **`10`** | `ExitAuthError` | Authentication failure, missing token source, or expired JWT token. |
| **`20`** | `ExitAPIError` | Fatal Microsoft Graph API error (e.g. invalid delta link token or unrecoverable HTTP response). |
| **`30`** | `ExitFileSystemError` | Local storage/filesystem error (e.g. workspace creation failure or permission denied). |
| **`130`** | `ExitInterrupted` | Execution interrupted via OS signal (`SIGINT` / `SIGTERM` / `Ctrl+C`). |

---

## Metadata JSON Specifications

The `metadata.json` file provides a detailed overview of the downloaded email.

### Field Descriptions

*   **`to`**: A list of recipients in the "To" field. Each recipient object contains an `emailAddress` object with `name` and `address`.
*   **`cc`**: A list of recipients in the "Cc" field. Same structure as `to`.
*   **`from`**: The sender of the email. Same structure as a recipient object.
*   **`subject`**: The subject line of the email.
*   **`received_date`**: The date and time the email was received, in ISO 8601 format (UTC).
*   **`body`**: The filename of the email body (e.g., `body.html`, `body.txt` or `body.pdf`).
*   **`content_type_of_body`**: The content type of the saved body file (`text/html`, `text/plain`, or `application/pdf`).
*   **`attachment_counts`**: The total number of attachments in the email.
*   **`list_of_attachments`**: A list of objects, where each object represents an attachment and contains the following fields:
    *   `attachment_name_in_message`: The original filename of the attachment.
    *   `content_type_of_attachment`: The MIME type of the attachment.
    *   `size_of_attachment_in_bytes`: The size of the attachment in bytes.
    *   `attachment_name_stored_after_download`: The filename used to save the attachment in the `attachments` folder (e.g., `01_report.pdf`).

### Example `metadata.json`

```json
{
  "to": [
    {
      "emailAddress": {
        "name": "Jane Doe",
        "address": "jane.doe@example.com"
      }
    }
  ],
  "cc": [
    {
      "emailAddress": {
        "name": "John Smith",
        "address": "john.smith@example.com"
      }
    }
  ],
  "from": {
    "emailAddress": {
      "name": "Marketing Team",
      "address": "marketing@example.com"
    }
  },
  "subject": "Q3 Financial Report and Project Updates",
  "received_date": "2024-07-21T14:30:00Z",
  "body": "body.html",
  "content_type_of_body": "text/html",
  "attachment_counts": 3,
  "list_of_attachments": [
    {
      "attachment_name_in_message": "Q3_Financials.xlsx",
      "content_type_of_attachment": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
      "size_of_attachment_in_bytes": 123456,
      "attachment_name_stored_after_download": "01_Q3_Financials.xlsx"
    },
    {
      "attachment_name_in_message": "Project_Timeline.pdf",
      "content_type_of_attachment": "application/pdf",
      "size_of_attachment_in_bytes": 789012,
      "attachment_name_stored_after_download": "02_Project_Timeline.pdf"
    },
    {
      "attachment_name_in_message": "company_logo.png",
      "content_type_of_attachment": "image/png",
      "size_of_attachment_in_bytes": 34567,
      "attachment_name_stored_after_download": "03_company_logo.png"
    }
  ]
}
```

## Token & Credential Management

The application requires authentication to interact with the Microsoft Graph API. You can authenticate using either **OAuth2 Client Credentials Flow** (with automatic background token refresh) or a **Static Bearer Token**:

### Option 1: OAuth2 Client Credentials Flow (Automatic Token Refresh)

Provide `tenantID`, `clientID`, and a client secret source (`clientSecret`, `clientSecretFile`, or `clientSecretEnv`). The library automatically requests access tokens from Microsoft Entra ID (`https://login.microsoftonline.com/{tenant_id}/oauth2/v2.0/token`) and handles **automatic token refresh in the background** before expiration.

| Config / Flag        | Environment Variable | Description                                                     |
| -------------------- | -------------------- | --------------------------------------------------------------- |
| `-tenant-id` / `tenantID`   |                     | Microsoft Azure AD Tenant ID (UUID).                            |
| `-client-id` / `clientID`   |                     | Application (Client) ID (UUID).                                 |
| `-client-secret` / `clientSecret` |               | Plaintext Client Secret.                                        |
| `-client-secret-file` / `clientSecretFile` |         | Path to file containing AES-256-GCM encrypted Client Secret.   |
| `-client-secret-env` / `clientSecretEnv` | `CLIENT_SECRET` | Read AES-256-GCM encrypted Client Secret from environment.    |

### Option 2: Static Bearer Token

For ephemeral single-run execution, supply a pre-acquired JWT access token using **exactly one** of the following methods:

| Flag                | Environment Variable | Description                                           |
| ------------------- | -------------------- | ----------------------------------------------------- |
| `-token-string`     |                      | Pass the token directly as a string.                  |
| `-token-file`       |                      | Provide the path to an AES-256-GCM encrypted token file. |
| `-token-env`        | `JWT_TOKEN`          | Read AES-256-GCM encrypted token from `JWT_TOKEN` env. |

For added security, stored credentials at rest (`-token-file`, `-token-env`, `-client-secret-file`, `-client-secret-env`) **MUST** be encrypted via `secretprotector` (`libsecsecrets`). Use `-remove-token-file` to delete temporary token files after execution.

## Command-line Arguments

All configuration options can be controlled via command-line arguments. Any flag you set will override the corresponding value in the configuration file.

| Argument                        | Description                                                               | Required | Default |
| ------------------------------- | ------------------------------------------------------------------------- | -------- | ------- |
| **Required**                    |                                                                           |          |         |
| `-mailbox`                      | The email address of the mailbox to download. Can also be set in config.  | **Yes**  |         |
| `-workspace`                    | The absolute path for storing artifacts. Required unless `-healthcheck` is used. | Conditional |         |
| **Client Credentials (Auto-Refresh)** |                                                                     |          |         |
| `-tenant-id`                    | Microsoft Entra ID Tenant ID.                                             | No       |         |
| `-client-id`                    | Application / Client ID.                                                  | No       |         |
| `-client-secret`                | Plaintext Client Secret.                                                  | No       |         |
| `-client-secret-file`           | Path to AES-256-GCM encrypted Client Secret file via `secretprotector`.  | No       |         |
| `-client-secret-env`            | Read encrypted Client Secret from `CLIENT_SECRET` environment variable.   | No       | `false` |
| **Token (Choose one)**          | Static bearer token source (string, file, or environment variable)       | Conditional |      |
| `-token-string`                 | JWT token as a string.                                                    |          |         |
| `-token-file`                   | Path to AES-256-GCM encrypted JWT token file.                             |          |         |
| `-token-env`                    | Read encrypted JWT token from `JWT_TOKEN` environment variable.           |          | `false` |
| `-remove-token-file`            | Remove the token file after use.                                          | No       | `false` |
| `-secret-master-key`            | Raw 64-char hex master key for `secretprotector` AES-256-GCM decryption. | No       |         |
| `-secret-master-key-env`        | Env var name storing master key (defaults to `SECRETPROTECTOR_MASTER_KEY`). | No     | `SECRETPROTECTOR_MASTER_KEY` |
| `-secret-master-key-file`       | File path storing 64-char hex master key (requires 0400/0600 on Linux).   | No       |         |
| **General**                     |                                                                           |          |         |
| `-config`                       | Path to a JSON configuration file.                                        | No       |         |
| `-debug`                        | Enable debug logging.                                                     | No       | `false` |
| `-healthcheck`                  | Perform a health check and exit.                                          | No       | `false` |
| `-message-details <folder>`     | When used with `-healthcheck`, displays message details for the specified folder. | No       |         |
| `-version`                      | Display the application version and exit.                                 | No       | `false` |
| **Processing & State**          |                                                                           |          |         |
| `-processing-mode`              | Processing mode: `full`, `incremental`, or `route`.                       | No       | `full`  |
| `-inbox-folder`                 | The source folder from which to process messages.                         | No       | `Inbox` |
| `-state`                        | Path to the state file for incremental processing.                        | No       |         |
| `-processed-folder`             | Destination folder for successful messages in `route` mode.               | No       | `Processed`|
| `-error-folder`                 | Destination folder for failed messages in `route` mode.                   | No       | `Error` |
| **Email Body Conversion**       |                                                                           |          |         |
| `-convert-body`                 | Conversion mode for email bodies: `none`, `text`, or `pdf`.               | No       | `none`  |
| `-chromium-path`                | Absolute path to the headless Chromium/Chrome binary (required for `pdf`).| No       |         |
| `-msg-handler`                  | Handler for `.msg`/`.eml` attachments: `raw` or `extractor`.              | No       | `raw`   |
| `-attachment-extraction-l1`    | Level 1 extraction for `.msg`/`.eml`: `default` (attachments only) or `inlines` (attachments + inlines). | No       | `default`|
| **Performance & Limits**        |                                                                           |          |         |
| `-parallel`                     | Maximum number of parallel workers.                                       | No       | `10`    |
| `-timeout`                      | HTTP client timeout in seconds.                                           | No       | `120`   |
| `-api-rate`                     | API calls per second for client-side rate limiting.                       | No       | `5.0`   |
| `-api-burst`                    | API burst capacity for client-side rate limiting.                         | No       | `10`    |
| `-max-retries`                  | Maximum number of retries for failed API calls.                           | No       | `2`     |
| `-initial-backoff-seconds`      | Initial backoff in seconds for retries.                                   | No       | `5`     |
| `-max-execution-time-msg`       | Maximum time in seconds to spend on one email message.                    | No       | `120`   |
| `-bandwidth-limit-mbs`          | Bandwidth limit in MB/s for downloads (0 for disabled).                   | No       | `0.0`   |
| `-large-attachment-threshold-mb` | Threshold in MB for what is considered a large attachment.                | No       | `20`    |
| `-chunk-size-mb`                | Chunk size in MB for downloading large attachments.                       | No       | `8`     |

## Configuration File

For a more permanent setup, you can use a JSON file (e.g., `config.json`) and pass its path via the `-config` flag. All options available as flags can be set in the config file.

### Example `config.json`

```json
{
  "mailboxName": "user@example.com",
  "workspacePath": "/path/to/your/output",
  "tokenString": "your-jwt-token-here",
  "debugLogging": false,
  "healthcheck": false,
  "messageDetailsFolder": "",
  "processingMode": "route",
  "stateFilePath": "/path/to/your/state.json",
  "inboxFolder": "Inbox",
  "processedFolder": "Processed-Archive",
  "errorFolder": "Error-Items",
  "httpClientTimeoutSeconds": 120,
  "maxRetries": 2,
  "initialBackoffSeconds": 5,
  "maxExecutionTimeMsg": 120,
  "maxParallelDownloads": 10,
  "apiCallsPerSecond": 5.0,
  "apiBurst": 10,
  "bandwidthLimitMBs": 0.0,
  "largeAttachmentThresholdMB": 20,
  "chunkSizeMB": 8,
  "convertBody": "none",
  "chromiumPath": "",
  "msgHandler": "extractor",
  "attachmentExtractionL1": "default"
}
```

### Configuration Directives

*   **Required**:
    *   `mailboxName`: (String) The email address of the mailbox to download.
    *   `workspacePath`: (String) The absolute path to a unique folder for storing downloaded artifacts.
*   **Token Management**:
    *   `tokenString`: (String) The JWT token.
    *   `tokenFile`: (String) Path to the token file.
    *   `tokenEnv`: (Boolean) Set to `true` to use the `JWT_TOKEN` environment variable.
    *   `removeTokenFile`: (Boolean) Set to `true` to delete the token file after use.
*   **General**:
    *   `debugLogging`: (Boolean) Enables debug-level logging.
    *   `healthcheck`: (Boolean) Set to `true` to perform a health check and exit.
    *   `messageDetailsFolder`: (String) When `healthcheck` is `true`, displays message details for the specified folder.
*   **Processing & State**:
    *   `processingMode`: (String) `full`, `incremental`, or `route`. In `route` mode, messages are moved after processing.
    *   `inboxFolder`: (String) The source folder to process messages from. Defaults to the main `Inbox`.
    *   `stateFilePath`: (String) Absolute path to the state file for incremental mode.
    *   `processedFolder`: (String) The destination folder for successfully processed messages in `route` mode.
    *   `errorFolder`: (String) The destination folder for messages that failed processing in `route` mode.
*   **Performance & Limits**:
    *   `httpClientTimeoutSeconds`: (Integer) Timeout in seconds for HTTP requests.
    *   `maxRetries`: (Integer) The maximum number of retries for failed API calls.
    *   `initialBackoffSeconds`: (Integer) The initial backoff in seconds for retries.
    *   `maxExecutionTimeMsg`: (Integer) The maximum time in seconds to spend processing a single email message before timing out and moving it to the error folder. Defaults to `120`.
    *   `maxParallelDownloads`: (Integer) The maximum number of concurrent workers.
    *   `apiCallsPerSecond`: (Float) The number of API calls allowed per second.
    *   `apiBurst`: (Integer) The burst capacity for the API rate limiter.
    *   `bandwidthLimitMBs`: (Float) The download speed limit in megabytes per second. `0` means disabled.
*   **Attachments**:
    *   `largeAttachmentThresholdMB`: (Integer) Threshold in MB for what is considered a large attachment.
    *   `chunkSizeMB`: (Integer) Chunk size in MB for downloading large attachments.
*   **Email Body Conversion**:
    *   `convertBody`: (String) The conversion mode for email bodies. Can be `none` (no conversion, saves `.html`), `text` (converts to plain text, saves `.txt`), or `pdf` (converts to PDF, saves `.pdf`). Defaults to `none`.
    *   `chromiumPath`: (String) The absolute path to a headless Chromium/Chrome binary (e.g., `d:\inet\www\chromium\bin\chrome.exe` on Windows or `/u01/chromium/chrome` on Linux) OR a DevTools WebSocket control URL to an **always-running Chrome daemon** (e.g., `ws://127.0.0.1:9222`). Required if `convertBody` is set to `pdf`. Connecting to a pre-launched Chrome daemon eliminates process spawning overhead and speeds up PDF rendering.
*   **ItemAttachment Handling**:
    *   `msgHandler`: (String) Determines how `.msg` and `.eml` attachments (ItemAttachments) are processed.
        *   `raw` (Default): Downloads the attachment as-is (MIME/EML format).
        *   `extractor`: Downloads the attachment, extracts the message body (HTML/Text), and extracts exactly one level of nested attachments.
    *   `attachmentExtractionL1`: (String) Determines which parts are extracted when `msgHandler` is set to `extractor`.
        *   `default` (Default): Only extracts standard attachments.
        *   `inlines`: Extracts both standard attachments and inline parts (e.g., inline images).

### A Note on API Permissions

For maximum security, it is recommended to use an Azure App Registration with the principle of least privilege.
*   For download-only modes (`full`, `incremental`), the `Mail.Read` permission is sufficient.
*   For `route` mode, which moves emails, the `Mail.ReadWrite` permission is required.

## Examples

### 0. Embedding as a Go Library (e.g., in `service-gateway`)

`o365mbx` can be embedded directly into other Go applications (such as microservices or daemon gateways) via the high-level `downloader` package with Client Credentials Flow (automatic token refresh):

```go
package main

import (
	"context"
	"log"

	"o365mbx/downloader"
	"o365mbx/engine"
)

func main() {
	ctx := context.Background()

	cfg := &engine.Config{
		MailboxName:          "user@example.com",
		WorkspacePath:        "/data/mailboxes/user1",
		TenantID:             "00000000-0000-0000-0000-000000000000",
		ClientID:             "11111111-1111-1111-1111-111111111111",
		ClientSecret:         "YOUR_AZURE_CLIENT_SECRET", // Auto-refreshed via Entra ID
		ProcessingMode:       "full",
		ConvertBody:          "text",
		MaxParallelDownloads: 5,
	}

	// Option A: Convenient one-liner execution
	if err := downloader.Run(ctx, cfg); err != nil {
		log.Fatalf("Mailbox download failed: %v", err)
	}

	// Option B: Advanced instance control
	// dl, err := downloader.New(cfg, customLogrusLogger)
	// err = dl.Execute(ctx, os.Stdout)
}
```

---

### 1. Client Credentials Mode with Automatic Token Refresh (CLI)

Use an Azure App Registration with automatic background token acquisition and refresh:

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -tenant-id "00000000-0000-0000-0000-000000000000" \
    -client-id "11111111-1111-1111-1111-111111111111" \
    -client-secret "YOUR_AZURE_CLIENT_SECRET"
```

### 2. Client Credentials with Encrypted Secret File (`secretprotector`)

Store the client secret encrypted at rest and supply a master key file:

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -tenant-id "00000000-0000-0000-0000-000000000000" \
    -client-id "11111111-1111-1111-1111-111111111111" \
    -client-secret-file "/secrets/client_secret.enc" \
    -secret-master-key-file "/secrets/master.key"
```

### 3. Client Credentials with Encrypted Environment Secret (`CLIENT_SECRET`)

Read the encrypted client secret from environment variables:

```shell
export CLIENT_SECRET="AES256-GCM:base64..."
export SECRETPROTECTOR_MASTER_KEY="64_CHAR_HEX_MASTER_KEY"

./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -tenant-id "00000000-0000-0000-0000-000000000000" \
    -client-id "11111111-1111-1111-1111-111111111111" \
    -client-secret-env
```

### 4. Basic Run with Static Token String

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-string "YOUR_ACCESS_TOKEN"
```

### 5. Incremental Run Using a Static Encrypted Token File

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-file "/path/to/token.enc" \
    -secret-master-key-file "/secrets/master.key" \
    -processing-mode incremental \
    -state "/path/to/state.json"
```

### 6. High-Performance Run Using a Config File and Overrides

This example uses a `config.json` file but overrides the parallelism and rate limits with command-line flags.

```shell
./o365mbx \
    -config "/path/to/your/config.json" \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -parallel 20 \
    -api-rate 10.0 \
    -api-burst 20
```

### 7. Route Mode with Default Folders

This example processes all messages from the "Inbox", saves the artifacts, and moves the original messages to either a "Processed" or "Error" folder in the mailbox.

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -tenant-id "00000000-0000-0000-0000-000000000000" \
    -client-id "11111111-1111-1111-1111-111111111111" \
    -client-secret-env \
    -processing-mode route
```

### 5. Route Mode with Custom Folders

This example does the same as above, but moves the messages to custom-named folders.

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-env \
    -processing-mode route \
    -processed-folder "Archive/Succeeded" \
    -error-folder "Archive/Failed"
```

### 6. High-Throughput Download

This example configures the application for maximum download speed by increasing the number of parallel workers and raising the API rate limits.

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-file "/path/to/token.txt" \
    -parallel 30 \
    -api-rate 15.0 \
    -api-burst 30
```

### 7. Bandwidth-Limited Download

This example throttles the total download speed to 50 MB/s to avoid hitting API data egress limits during a very large-scale download.

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-file "/path/to/token.txt" \
    -bandwidth-limit-mbs 50.0
```

### 8. Custom Message Timeout

This example sets a shorter timeout of 30 seconds for processing each message, which is useful for rapid processing of small emails or when running in environments with strict time constraints.

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-env \
    -max-execution-time-msg 30
```

### 9. Body Conversion to Plain Text

This example downloads all emails and converts their bodies from HTML to plain text.

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-file "/path/to/token.txt" \
    -convert-body text
```

### 11. Multi-Tenant Enterprise Security & Permission Guidance (Linux / Windows)

When deploying `o365mbx` in enterprise multi-tenant server environments:
* **Linux Umask Recommendation**: Run the binary under a restricted service account with `umask 0077` (or `umask 0027`) to restrict directory (`0700`) and artifact file (`0600`) access strictly to the owner.
* **Workspace Isolation**: Always specify an absolute, dedicated workspace directory per mailbox (`-workspace /data/mailboxes/user1`) to prevent workspace cross-talk or unauthorized file reads via `os.OpenRoot`.
* **Chrome Daemon Isolation**: When running PDF conversion (`-convert-body pdf`) against a shared Chrome daemon (`ws://127.0.0.1:9222`), ensure the daemon process runs under isolated non-privileged process credentials.

---

### 10. Health Check Examples

The `-healthcheck` flag provides powerful, read-only tools to inspect the mailbox without downloading any items.

#### Basic Health Check

This example runs a general health check on the mailbox, showing overall stats and a list of all folders, sorted by name.

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -token-env \
    -healthcheck
```

**Example Output:**

```text
--- Mailbox Health Check ---
Mailbox: user@example.com
------------------------------
Total Messages: 1573
Total Folders: 14
Total Mailbox Size: 250.75 MB
------------------------------

--- Folder Statistics ---
Folder                 Items   Size (KB)
-------                -----   ---------
Archive                50      102400.00
Conversation History   2       50.20
Deleted Items          10      5120.00
Drafts                 1       10.50
Inbox                  25      81920.00
Junk Email             5       1024.00
Outbox                 0       0.00
... (and so on)
-------------------------
```

#### Message Details Health Check

This example extends the health check to show detailed information for all messages within a specific folder, such as the `Inbox`.

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -token-env \
    -healthcheck \
    -message-details "Inbox"
```

**Example Output:**

```text
--- Message Details for Folder: Inbox ---
From                  To                      Date                Subject                                                                 Attachments  Total Size (KB)
----                  --                      ----                -------                                                                 -----------  ----------------
sender@corp.com       user@example.com        2024-08-01 10:30    Q3 Financial Report and Project Updates                                 2            1205.63
another@sender.org    user@example.com;...    2024-08-01 09:15    Important: Action Required for your account                             0            5.12
marketing@company.com user@example.com        2024-07-31 16:00    Weekly Newsletter - Check out our new features and updates for this week! 0            15.30
... (and so on)
-------------------------------------------------
```

### 11. Message Processing Timeout

This example sets a custom timeout of 60 seconds for processing each individual email message. If a message (including its body conversion and all attachments) takes longer than this, it is cancelled and moved to the error folder.

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-env \
    -max-execution-time-msg 60
```

### 12. Complex Email Extraction

This example downloads all emails and, for any attached `.msg` or `.eml` files, extracts their body and immediate attachments.

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-env \
    -msg-handler extractor
```

### 13. Raw Email Download

This example ensures that attached emails are saved as-is without any extraction (default behavior).

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-env \
    -msg-handler raw
```

## License

This project is licensed under the MIT License. See the [LICENSE](LICENSE) file for details.
