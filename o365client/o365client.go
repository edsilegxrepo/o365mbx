// Package o365client handles all interactions with the Microsoft Graph API,
// including message retrieval, attachment streaming, and mailbox management.
//
// OBJECTIVE:
// Served as the primary Graph API communication client for mailbox inspection, message streaming, and folder management.
//
// CORE COMPONENTS:
// 1. O365Client: High-level Graph API client wrapper.
// 2. GetMessages: Delta sync & pagination query handler.
// 3. GetAttachmentRawStream: Raw binary `$value` stream downloader for attachments and MIME envelopes.
// 4. MoveMessage & GetOrCreateFolderIDByName: Mailbox routing and folder management operations.
//
// CORE FUNCTIONALITY:
//  1. Authentication: Manages static token-based authentication for Graph API requests.
//  2. Message Retrieval: Fetches messages using delta queries for efficient incremental sync.
//  3. Attachment Streaming: Provides methods to download attachments, including raw
//     MIME streams for item attachments (.msg/.eml).
//  4. Mailbox Management: Retrieves folder structures, item counts, and storage statistics.
//  5. Resilience: Implements API rate limiting and basic error mapping for Graph API responses.
//
// DATA FLOW:
// Graph Request -> Kiota Request Adapter / HTTP Client -> OData Graph API Response -> Domain Models / AppErrors.
package o365client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	// #nosec G404 - math/rand is used exclusively for exponential backoff jitter and retry timing, not cryptographic security.
	// nosemgrep: go.lang.security.audit.crypto.math_random.math-random-used
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"criticalsys.net/o365mbx/apperrors"
	"criticalsys.net/o365mbx/utils"

	kiota "github.com/microsoft/kiota-abstractions-go"
	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	"github.com/microsoftgraph/msgraph-sdk-go/models"
	"github.com/microsoftgraph/msgraph-sdk-go/models/odataerrors"
	"github.com/microsoftgraph/msgraph-sdk-go/users"
	log "github.com/sirupsen/logrus"
)

// FolderStats encapsulates basic statistics for a specific mail folder.
type FolderStats struct {
	Name         string
	TotalItems   int32
	Size         int64
	LastItemDate *time.Time
}

// MailboxHealthStats provides an overview of the mailbox's health and structure.
type MailboxHealthStats struct {
	TotalMessages    int32
	TotalMailboxSize int64
	Folders          []FolderStats
}

// MessageDetail contains granular information about a specific email message.
type MessageDetail struct {
	From                 string
	To                   string
	Date                 time.Time
	Subject              string
	AttachmentCount      int
	AttachmentsTotalSize int64
}

// O365ClientInterface defines the interface for O365Client methods used by other packages.
//
//go:generate mockgen -destination=../mocks/mock_o365client.go -package=mocks o365mbx/o365client O365ClientInterface
type O365ClientInterface interface {
	GetMailboxStats(ctx context.Context, mailboxName string) (map[string]int32, error)
	GetMessages(ctx context.Context, mailboxName, sourceFolderID string, state *RunState, messagesChan chan<- models.Messageable) error
	GetMessageAttachments(ctx context.Context, mailboxName, messageID string) ([]models.Attachmentable, error)
	GetMailboxHealthCheck(ctx context.Context, mailboxName string) (*MailboxHealthStats, error)
	GetMessageDetailsForFolder(ctx context.Context, mailboxName, folderName string, detailsChan chan<- MessageDetail) error
	MoveMessage(ctx context.Context, mailboxName, messageID, destinationFolderID string) error
	GetOrCreateFolderIDByName(ctx context.Context, mailboxName, folderName string) (string, error)
	GetAttachmentRawStream(ctx context.Context, mailboxName, messageID, attachmentID string) (io.ReadCloser, error)
	GetItemAttachment(ctx context.Context, mailboxName, messageID, attachmentID string) (models.Attachmentable, error)
}

// O365Client implements the O365ClientInterface using the Microsoft Graph SDK.
type O365Client struct {
	client       *msgraphsdk.GraphServiceClient
	authProvider AuthenticationProvider
	rng          *rand.Rand
}

// --- Initialization ---

// NewO365Client initializes a new Graph API client with the provided access token and random source.
// It sets up the static authentication provider and request adapter for the Microsoft Graph SDK.
func NewO365Client(accessToken string, rng *rand.Rand) (*O365Client, error) {
	authProvider, err := NewStaticTokenAuthenticationProvider(accessToken)
	if err != nil {
		return nil, fmt.Errorf("failed to create auth provider: %w", err)
	}
	return NewO365ClientWithAuthProvider(authProvider, rng)
}

// NewO365ClientWithAuthProvider initializes a new Graph API client with any Kiota AuthenticationProvider.
func NewO365ClientWithAuthProvider(authProvider AuthenticationProvider, rng *rand.Rand) (*O365Client, error) {
	if authProvider == nil {
		return nil, fmt.Errorf("authProvider cannot be nil")
	}
	adapter, err := msgraphsdk.NewGraphRequestAdapter(authProvider)
	if err != nil {
		return nil, fmt.Errorf("failed to create graph adapter: %w", err)
	}
	client := NewO365ClientWithAdapter(adapter, rng)
	client.authProvider = authProvider
	return client, nil
}

// NewO365ClientWithAdapter allows injecting a custom adapter for testing (e.g., with Microsoft Dev Proxy transport).
func NewO365ClientWithAdapter(adapter kiota.RequestAdapter, rng *rand.Rand) *O365Client {
	return &O365Client{
		client: msgraphsdk.NewGraphServiceClient(adapter),
		rng:    rng,
	}
}

// --- Message Retrieval ---

// GetMailboxStats returns a map of folder names to their total item counts.
func (c *O365Client) GetMailboxStats(ctx context.Context, mailboxName string) (map[string]int32, error) {
	allFolders, err := c.GetAllFolders(ctx, mailboxName)
	if err != nil {
		return nil, fmt.Errorf("failed to get all folders for stats: %w", err)
	}

	stats := make(map[string]int32)
	for _, folder := range allFolders {
		if folder.GetDisplayName() != nil && folder.GetTotalItemCount() != nil {
			stats[*folder.GetDisplayName()] = *folder.GetTotalItemCount()
		}
	}
	return stats, nil
}

// GetMessages fetches a list of messages for a given mailbox using delta query and streams them to a channel.
// It uses Microsoft Graph's delta query capability to efficiently retrieve only changes
// since the last run. It updates the provided RunState with the new delta link.
func (c *O365Client) GetMessages(ctx context.Context, mailboxName, sourceFolderID string, state *RunState, messagesChan chan<- models.Messageable) error {
	defer close(messagesChan)

	isIncrementalRun := state.DeltaLink != ""

	var (
		messagesResponse users.ItemMailFoldersItemMessagesDeltaGetResponseable
		err              error
	)

	if !isIncrementalRun {
		log.Info("No delta link found. Starting initial synchronization.")
		// We no longer expand attachments here to reduce memory usage.
		// Attachments will be fetched on a per-message basis.
		requestConfiguration := &users.ItemMailFoldersItemMessagesDeltaRequestBuilderGetRequestConfiguration{
			QueryParameters: &users.ItemMailFoldersItemMessagesDeltaRequestBuilderGetQueryParameters{
				Select: []string{"id", "subject", "receivedDateTime", "body", "hasAttachments", "from", "toRecipients", "ccRecipients"},
			},
		}
		log.WithFields(log.Fields{
			"mailboxName":    mailboxName,
			"sourceFolderID": sourceFolderID,
		}).Debug("Requesting message delta from Graph API.")
		for attempt := 0; attempt < 5; attempt++ {
			messagesResponse, err = c.client.Users().ByUserId(mailboxName).MailFolders().ByMailFolderId(sourceFolderID).Messages().Delta().Get(ctx, requestConfiguration)
			if err == nil {
				break
			}
			sleepSec := 2
			if strings.Contains(strings.ToLower(err.Error()), "throttled") || strings.Contains(strings.ToLower(err.Error()), "retry-after") {
				sleepSec = 7
			}
			time.Sleep(time.Duration(sleepSec) * time.Second)
		}
	} else {
		log.WithField("deltaLink", state.DeltaLink).Info("Found delta link. Fetching incremental changes.")
		builder := users.NewItemMailFoldersItemMessagesDeltaRequestBuilder(state.DeltaLink, c.client.GetAdapter())
		for attempt := 0; attempt < 5; attempt++ {
			messagesResponse, err = builder.Get(ctx, nil)
			if err == nil {
				break
			}
			sleepSec := 2
			if strings.Contains(strings.ToLower(err.Error()), "throttled") || strings.Contains(strings.ToLower(err.Error()), "retry-after") {
				sleepSec = 7
			}
			time.Sleep(time.Duration(sleepSec) * time.Second)
		}
	}

	if err != nil {
		return handleError(err)
	}

	for {
		if messagesResponse == nil || messagesResponse.GetValue() == nil {
			log.Warn("Received nil response or empty messages list from Graph API.")
			break
		}

		messages := messagesResponse.GetValue()
		log.WithField("count", len(messages)).Info("Fetched page of messages.")

		for i, message := range messages {
			var id string
			if message.GetId() != nil {
				id = *message.GetId()
			}
			log.WithFields(log.Fields{"index": i, "id": id}).Debug("Streaming message to channel.")
			select {
			case <-ctx.Done():
				log.Warn("Context cancelled during message streaming.")
				return ctx.Err()
			case messagesChan <- message:
			}
		}

		nextLink := messagesResponse.GetOdataNextLink()
		if nextLink == nil || *nextLink == "" {
			deltaLink := messagesResponse.GetOdataDeltaLink()
			if deltaLink != nil && *deltaLink != "" {
				log.WithField("deltaLink", *deltaLink).Info("Captured new delta link for next run.")
				state.DeltaLink = *deltaLink
			} else {
				if isIncrementalRun {
					log.Error("Critical error: A delta link was expected on the final page of an incremental sync, but was not provided by the API.")
					return apperrors.ErrMissingDeltaLink
				}
				log.Warn("Expected a delta link on the final page, but found none. This is not critical for a full sync, but state for the next incremental run cannot be saved.")
			}
			break
		}

		log.Debug("Fetching next page of messages.")
		builder := users.NewItemMailFoldersItemMessagesDeltaRequestBuilder(*nextLink, c.client.GetAdapter())
		for attempt := 0; attempt < 5; attempt++ {
			messagesResponse, err = builder.Get(ctx, nil)
			if err == nil {
				break
			}
			sleepSec := 2
			if strings.Contains(strings.ToLower(err.Error()), "throttled") || strings.Contains(strings.ToLower(err.Error()), "retry-after") {
				sleepSec = 7
			}
			time.Sleep(time.Duration(sleepSec) * time.Second)
		}
		if err != nil {
			return handleError(err)
		}
	}

	log.Info("Finished processing all message pages.")
	return nil
}

// GetMessageAttachments fetches all attachments for a specific message, expanding item attachments.
func (c *O365Client) GetMessageAttachments(ctx context.Context, mailboxName, messageID string) ([]models.Attachmentable, error) {
	log.WithFields(log.Fields{"messageID": messageID}).Debug("Fetching attachments for message.")

	expand := "microsoft.graph.itemAttachment/item"
	requestConfiguration := &users.ItemMessagesItemAttachmentsRequestBuilderGetRequestConfiguration{
		QueryParameters: &users.ItemMessagesItemAttachmentsRequestBuilderGetQueryParameters{
			Expand: []string{expand},
		},
	}

	response, err := c.client.Users().ByUserId(mailboxName).Messages().ByMessageId(messageID).Attachments().Get(ctx, requestConfiguration)
	if err != nil {
		return nil, handleError(err)
	}

	attachments := response.GetValue()
	log.WithFields(log.Fields{"messageID": messageID, "count": len(attachments)}).Info("Successfully fetched attachments.")
	return attachments, nil
}

// GetItemAttachment fetches a single item attachment with expanded item details.
func (c *O365Client) GetItemAttachment(ctx context.Context, mailboxName, messageID, attachmentID string) (models.Attachmentable, error) {
	attachment, err := c.client.Users().ByUserId(mailboxName).Messages().ByMessageId(messageID).Attachments().ByAttachmentId(attachmentID).Get(ctx, nil)
	if err != nil {
		return nil, handleError(err)
	}
	return attachment, nil
}

// --- Folder Management ---

// GetOrCreateFolderIDByName gets the ID of a folder by name, creating it if it doesn't exist.
// It first attempts an exact match using an OData filter. If that fails, it performs
// a case-insensitive search across all folders in the mailbox.
func (c *O365Client) GetOrCreateFolderIDByName(ctx context.Context, mailboxName, folderName string) (string, error) {
	filter := fmt.Sprintf("displayName eq '%s'", folderName)
	requestConfiguration := &users.ItemMailFoldersRequestBuilderGetRequestConfiguration{
		QueryParameters: &users.ItemMailFoldersRequestBuilderGetQueryParameters{
			Filter: &filter,
			Top:    Ptr(int32(1)), // We only need one result
		},
	}

	var folders models.MailFolderCollectionResponseable
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		folders, err = c.client.Users().ByUserId(mailboxName).MailFolders().Get(ctx, requestConfiguration)
		if err == nil {
			break
		}
		sleepSec := 2
		if strings.Contains(strings.ToLower(err.Error()), "throttled") || strings.Contains(strings.ToLower(err.Error()), "retry-after") {
			sleepSec = 7
		}
		time.Sleep(time.Duration(sleepSec) * time.Second)
	}
	if err != nil {
		return "", handleError(err)
	}

	if folders != nil && folders.GetValue() != nil && len(folders.GetValue()) > 0 {
		folderID := *folders.GetValue()[0].GetId()
		log.WithField("folderName", folderName).Info("Found existing folder.")
		return folderID, nil
	}

	// If folder is not found by exact match, try a case-insensitive search on client side
	allFolders, err := c.GetAllFolders(ctx, mailboxName)
	if err != nil {
		return "", fmt.Errorf("could not get all folders for case-insensitive search: %w", err)
	}

	for _, folder := range allFolders {
		if strings.EqualFold(*folder.GetDisplayName(), folderName) {
			folderID := *folder.GetId()
			log.WithField("folderName", folderName).Info("Found existing folder (case-insensitive).")
			return folderID, nil
		}
	}

	log.WithField("folderName", folderName).Info("Folder not found, creating it.")
	newFolder := models.NewMailFolder()
	newFolder.SetDisplayName(&folderName)

	var createdFolder models.MailFolderable
	for attempt := 0; attempt < 5; attempt++ {
		createdFolder, err = c.client.Users().ByUserId(mailboxName).MailFolders().Post(ctx, newFolder, nil)
		if err == nil {
			break
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	if err != nil {
		return "", handleError(err)
	}

	folderID := *createdFolder.GetId()
	log.WithFields(log.Fields{"folderName": folderName, "folderId": folderID}).Info("Successfully created folder.")
	return folderID, nil
}

// GetAllFolders retrieves all mail folders for a mailbox, handling pagination.
func (c *O365Client) GetAllFolders(ctx context.Context, mailboxName string) ([]models.MailFolderable, error) {
	allFolders := make([]models.MailFolderable, 0)
	requestConfiguration := &users.ItemMailFoldersRequestBuilderGetRequestConfiguration{
		QueryParameters: &users.ItemMailFoldersRequestBuilderGetQueryParameters{
			Select: []string{"id", "displayName", "totalItemCount"},
			Expand: []string{"singleValueExtendedProperties($filter=id eq 'Long 0x0E08')"},
		},
	}
	foldersResponse, err := c.client.Users().ByUserId(mailboxName).MailFolders().Get(ctx, requestConfiguration)
	if err != nil {
		return nil, handleError(err)
	}

	if foldersResponse == nil {
		return allFolders, nil
	}

	for {
		pageFolders := foldersResponse.GetValue()
		if pageFolders != nil {
			allFolders = append(allFolders, pageFolders...)
		}

		nextLink := foldersResponse.GetOdataNextLink()
		if nextLink == nil || *nextLink == "" {
			break
		}

		log.Debug("Fetching next page of mail folders.")
		builder := users.NewItemMailFoldersRequestBuilder(*nextLink, c.client.GetAdapter())
		foldersResponse, err = builder.Get(ctx, nil)
		if err != nil {
			return nil, handleError(err)
		}
	}
	return allFolders, nil
}

// --- Health and Diagnostics ---

// GetMailboxHealthCheck retrieves aggregate statistics and folder details for the mailbox.
func (c *O365Client) GetMailboxHealthCheck(ctx context.Context, mailboxName string) (*MailboxHealthStats, error) {
	stats := &MailboxHealthStats{
		Folders: make([]FolderStats, 0),
	}

	allFolders, err := c.GetAllFolders(ctx, mailboxName)
	if err != nil {
		return nil, fmt.Errorf("failed to get all folders: %w", err)
	}

	// Sort folders by name
	sort.Slice(allFolders, func(i, j int) bool {
		return strings.ToLower(*allFolders[i].GetDisplayName()) < strings.ToLower(*allFolders[j].GetDisplayName())
	})

	var totalMailboxSize int64
	for _, folder := range allFolders {
		folderSize := getFolderSizeFromFolder(folder)

		folderStat := FolderStats{
			Name:       *folder.GetDisplayName(),
			TotalItems: *folder.GetTotalItemCount(),
			Size:       folderSize,
		}

		// 2. If it's the Inbox, get the last message date
		if strings.ToLower(*folder.GetDisplayName()) == "inbox" {
			// Query for the most recent message
			lastMessage, err := c.client.Users().ByUserId(mailboxName).MailFolders().ByMailFolderId(*folder.GetId()).Messages().Get(ctx, &users.ItemMailFoldersItemMessagesRequestBuilderGetRequestConfiguration{
				QueryParameters: &users.ItemMailFoldersItemMessagesRequestBuilderGetQueryParameters{
					Top:     Ptr(int32(1)),
					Select:  []string{"receivedDateTime"},
					Orderby: []string{"receivedDateTime desc"},
				},
			})
			if err != nil {
				log.WithField("folder", folderStat.Name).Warnf("Could not fetch last message date: %v", err)
			} else if len(lastMessage.GetValue()) > 0 {
				folderStat.LastItemDate = lastMessage.GetValue()[0].GetReceivedDateTime()
			}
		}

		stats.Folders = append(stats.Folders, folderStat)
		stats.TotalMessages += folderStat.TotalItems
		totalMailboxSize += folderSize
	}

	stats.TotalMailboxSize = totalMailboxSize

	return stats, nil
}

// GetMessageDetailsForFolder streams granular metadata for all messages in a
// specified folder to a channel. It expands attachment information to calculate
// total attachment sizes for each message.
func (c *O365Client) GetMessageDetailsForFolder(ctx context.Context, mailboxName, folderName string, detailsChan chan<- MessageDetail) error {
	defer close(detailsChan)

	folderID, err := c.GetOrCreateFolderIDByName(ctx, mailboxName, folderName)
	if err != nil {
		return fmt.Errorf("could not find or create folder '%s': %w", folderName, err)
	}

	requestConfig := &users.ItemMailFoldersItemMessagesRequestBuilderGetRequestConfiguration{
		QueryParameters: &users.ItemMailFoldersItemMessagesRequestBuilderGetQueryParameters{
			Select: []string{
				"from", "toRecipients", "receivedDateTime", "subject", "hasAttachments",
			},
			Expand: []string{"attachments($select=size)"},
			Top:    Ptr(int32(100)),
		},
	}

	messagesResponse, err := c.client.Users().ByUserId(mailboxName).MailFolders().ByMailFolderId(folderID).Messages().Get(ctx, requestConfig)
	if err != nil {
		return handleError(err)
	}

	for {
		for _, msg := range messagesResponse.GetValue() {
			var totalAttachmentSize int64
			attachmentCount := 0
			if msg.GetHasAttachments() != nil && *msg.GetHasAttachments() {
				attachments := msg.GetAttachments()
				attachmentCount = len(attachments)
				for _, att := range attachments {
					if size := att.GetSize(); size != nil {
						totalAttachmentSize += int64(*size)
					}
				}
			}

			var toString string
			if toRecipients := msg.GetToRecipients(); len(toRecipients) > 0 {
				toEmails := make([]string, len(toRecipients))
				for i, r := range toRecipients {
					if r.GetEmailAddress() != nil && r.GetEmailAddress().GetAddress() != nil {
						toEmails[i] = *r.GetEmailAddress().GetAddress()
					}
				}
				toString = strings.Join(toEmails, ";")
			}

			var fromString string
			if from := msg.GetFrom(); from != nil && from.GetEmailAddress() != nil && from.GetEmailAddress().GetAddress() != nil {
				fromString = *from.GetEmailAddress().GetAddress()
			}

			detail := MessageDetail{
				From:                 fromString,
				To:                   toString,
				Date:                 utils.TimeValue(msg.GetReceivedDateTime(), time.Time{}),
				Subject:              utils.StringValue(msg.GetSubject(), "(no subject)"),
				AttachmentCount:      attachmentCount,
				AttachmentsTotalSize: totalAttachmentSize,
			}

			select {
			case <-ctx.Done():
				log.Warn("Context cancelled during message detail streaming.")
				return ctx.Err()
			case detailsChan <- detail:
			}
		}

		nextLink := messagesResponse.GetOdataNextLink()
		if nextLink == nil || *nextLink == "" {
			break
		}

		log.Debug("Fetching next page of messages for details.")
		builder := users.NewItemMailFoldersItemMessagesRequestBuilder(*nextLink, c.client.GetAdapter())
		messagesResponse, err = builder.Get(ctx, nil)
		if err != nil {
			return handleError(err)
		}
	}

	return nil
}

// MoveMessage relocates a message to a different folder within the same mailbox.
// This is typically used in 'route' mode to move processed messages to
// successful or error folders.
func (c *O365Client) MoveMessage(ctx context.Context, mailboxName, messageID, destinationFolderID string) error {
	requestBody := users.NewItemMessagesItemMovePostRequestBody()
	requestBody.SetDestinationId(&destinationFolderID)

	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		_, err := c.client.Users().ByUserId(mailboxName).Messages().ByMessageId(messageID).Move().Post(ctx, requestBody, nil)
		if err == nil {
			return nil
		}
		lastErr = err
		sleepSec := 2
		if strings.Contains(strings.ToLower(err.Error()), "throttled") || strings.Contains(strings.ToLower(err.Error()), "retry-after") {
			sleepSec = 7
		}
		time.Sleep(time.Duration(sleepSec) * time.Second)
	}
	return handleError(lastErr)
}

// --- Attachment and Stream Handling ---

// GetAttachmentRawStream fetches the raw stream of an attachment using the $value endpoint.
func (c *O365Client) GetAttachmentRawStream(ctx context.Context, mailboxName, messageID, attachmentID string) (io.ReadCloser, error) {
	log.WithFields(log.Fields{
		"messageID":    messageID,
		"attachmentID": attachmentID,
	}).Debug("Fetching raw attachment stream ($value).")

	// 2. Build the URL string
	baseUrl := c.client.GetAdapter().GetBaseUrl()
	urlPathStr := fmt.Sprintf("%s/users/%s/messages/%s/attachments/%s/$value",
		baseUrl, mailboxName, messageID, attachmentID)

	// 3. PARSE the string into a *url.URL object
	parsedUrl, err := url.Parse(urlPathStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse attachment URL: %w", err)
	}

	// 4. Send the request via the Adapter or native HTTP
	// We use native http.Client here because Kiota's SendPrimitive often fails
	// with "no factory registered" for message/rfc822 or other binary streams
	// when using the $value endpoint.
	// Use an optimized transport for connection pooling in production.
	transport := http.DefaultTransport
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		cloned := t.Clone()
		cloned.MaxIdleConnsPerHost = 100 // Allow more concurrent idle connections per host
		transport = cloned
	}

	httpClient := &http.Client{
		Transport: transport,
		Timeout:   120 * time.Second, // Base safety timeout
	}

	req, err := http.NewRequestWithContext(ctx, "GET", parsedUrl.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	if c.authProvider != nil {
		reqInfo := kiota.NewRequestInformation()
		reqInfo.UrlTemplate = parsedUrl.String()
		reqInfo.Method = kiota.GET
		if authErr := c.authProvider.AuthenticateRequest(ctx, reqInfo, nil); authErr == nil {
			if authHeaders := reqInfo.Headers.Get("Authorization"); len(authHeaders) > 0 {
				req.Header.Set("Authorization", authHeaders[0])
			}
		}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, handleError(err)
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("graph api returned status %d for $value endpoint", resp.StatusCode)
	}

	return resp.Body, nil
}

// RunState represents the state of the last successful incremental run.
type RunState struct {
	DeltaLink string `json:"deltaLink"`
}

// handleError converts odataerrors.ODataError to a more specific application error.
// extracting the HTTP status code if available
func handleError(err error) error {
	if err == nil {
		return nil
	}

	var odataErr *odataerrors.ODataError
	if errors.As(err, &odataErr) {
		statusCode := 0
		type responseStatusCode interface {
			GetResponseStatusCode() int
		}
		var rsc responseStatusCode
		if errors.As(err, &rsc) {
			statusCode = rsc.GetResponseStatusCode()
		}

		return &apperrors.APIError{StatusCode: statusCode, Msg: odataErr.Error()}
	}
	return err
}

// Ptr returns a pointer to the given value.
func Ptr[T any](v T) *T {
	return &v
}

// getFolderSizeFromFolder extracts folder size in bytes from MAPI extended properties or additionalData.
func getFolderSizeFromFolder(folder models.MailFolderable) int64 {
	if folder == nil {
		return 0
	}
	// 1. Check singleValueExtendedProperties for MAPI PR_MESSAGE_SIZE_EXTENDED (Long 0x0E08)
	if props := folder.GetSingleValueExtendedProperties(); props != nil {
		for _, prop := range props {
			if prop.GetId() != nil && strings.EqualFold(*prop.GetId(), "Long 0x0E08") && prop.GetValue() != nil {
				if size := parseFolderSize(*prop.GetValue()); size > 0 {
					return size
				}
			}
		}
	}
	// 2. Fall back to additionalData ("sizeInBytes", "size")
	return getFolderSizeFromAdditionalData(folder.GetAdditionalData())
}

// getFolderSizeFromAdditionalData searches additionalData case-insensitively for size keys.
func getFolderSizeFromAdditionalData(additionalData map[string]any) int64 {
	if additionalData == nil {
		return 0
	}
	for k, v := range additionalData {
		if strings.EqualFold(k, "sizeInBytes") || strings.EqualFold(k, "size") {
			if val := parseFolderSize(v); val > 0 {
				return val
			}
		}
	}
	return 0
}

// parseFolderSize extracts an int64 from various types that Kiota, JSON decoders, or Graph API might use for sizeInBytes.
func parseFolderSize(size interface{}) int64 {
	if size == nil {
		return 0
	}

	// 1. Unwrap Kiota UntypedNode or custom wrappers that provide GetValue()
	if un, ok := size.(interface{ GetValue() any }); ok {
		if inner := un.GetValue(); inner != nil {
			return parseFolderSize(inner)
		}
	}

	switch v := size.(type) {
	case *int64:
		if v != nil {
			return *v
		}
	case *int32:
		if v != nil {
			return int64(*v)
		}
	case *int:
		if v != nil {
			return int64(*v)
		}
	case int64:
		return v
	case int32:
		return int64(v)
	case int:
		return int64(v)
	case uint64:
		if v > math.MaxInt64 {
			return math.MaxInt64
		}
		return int64(v)
	case uint32:
		return int64(v)
	case uint:
		if uint64(v) > math.MaxInt64 {
			return math.MaxInt64
		}
		return int64(v)
	case float64:
		return int64(v)
	case *float64:
		if v != nil {
			return int64(*v)
		}
	case float32:
		return int64(v)
	case *float32:
		if v != nil {
			return int64(*v)
		}
	case string:
		if val, err := strconv.ParseInt(v, 10, 64); err == nil {
			return val
		}
		if fval, err := strconv.ParseFloat(v, 64); err == nil {
			return int64(fval)
		}
	case *string:
		if v != nil {
			return parseFolderSize(*v)
		}
	case json.Number:
		if val, err := v.Int64(); err == nil {
			return val
		}
		if fval, err := v.Float64(); err == nil {
			return int64(fval)
		}
	}
	return 0
}

// ExportParseFolderSize is an exported version of parseFolderSize for testing.
func ExportParseFolderSize(size interface{}) int64 {
	return parseFolderSize(size)
}
