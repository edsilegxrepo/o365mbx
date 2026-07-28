// Package o365client handles all interactions with the Microsoft Graph API,
// including message retrieval, attachment streaming, and mailbox management.
//
// OBJECTIVE:
// Provide authentication providers for Microsoft Graph API via Kiota AuthenticationProvider interface.
//
// CORE COMPONENTS:
// 1. StaticTokenAuthenticationProvider: Authenticates Kiota Graph requests using a static JWT bearer token.
// 2. ClientCredentialsAuthenticationProvider: Manages OAuth2 Client Credentials flow with automatic token refresh.
//
// CORE FUNCTIONALITY:
// 1. Token Injection: Adds the "Authorization: Bearer <token>" header to all requests.
// 2. OAuth2 Client Credentials: Obtains tokens from Microsoft Entra ID (`login.microsoftonline.com`) and refreshes prior to expiration.
// 3. Provider Interface: Implements Kiota's absauth.AuthenticationProvider interface.
//
// DATA FLOW:
// Kiota RequestInformation -> AuthenticateRequest -> Read/Refresh Token -> Header Bearer Injection -> HTTP Transport.
package o365client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	abstractions "github.com/microsoft/kiota-abstractions-go"
	absauth "github.com/microsoft/kiota-abstractions-go/authentication"
)

// AuthenticationProvider is an alias for kiota-abstractions-go/authentication.AuthenticationProvider interface.
type AuthenticationProvider = absauth.AuthenticationProvider

// StaticTokenAuthenticationProvider authenticates requests with a static access token.
type StaticTokenAuthenticationProvider struct {
	accessToken string
}

// NewStaticTokenAuthenticationProvider creates a new StaticTokenAuthenticationProvider.
func NewStaticTokenAuthenticationProvider(accessToken string) (*StaticTokenAuthenticationProvider, error) {
	if accessToken == "" {
		return nil, errors.New("access token cannot be empty")
	}
	return &StaticTokenAuthenticationProvider{
		accessToken: accessToken,
	}, nil
}

// AuthenticateRequest adds the Authorization header to the request.
func (s *StaticTokenAuthenticationProvider) AuthenticateRequest(ctx context.Context, request *abstractions.RequestInformation, additionalAuthenticationContext map[string]interface{}) error {
	if request == nil {
		return errors.New("request cannot be nil")
	}
	if request.Headers == nil {
		request.Headers = abstractions.NewRequestHeaders()
	}
	request.Headers.Add("Authorization", "Bearer "+s.accessToken)
	return nil
}

// GetAuthorizationToken is required by the AccessTokenProvider interface
func (s *StaticTokenAuthenticationProvider) GetAuthorizationToken(ctx context.Context, request *url.URL, additionalAuthenticationContext map[string]interface{}) (string, error) {
	return s.accessToken, nil
}

// ClientCredentialsAuthenticationProvider handles OAuth2 Client Credentials grant
// and automatic background token refresh for Microsoft Graph API requests.
type ClientCredentialsAuthenticationProvider struct {
	tenantID      string
	clientID      string
	clientSecret  string
	tokenEndpoint string
	httpClient    *http.Client

	mu          sync.RWMutex
	accessToken string
	expiresAt   time.Time
}

// NewClientCredentialsAuthenticationProvider creates a new provider given Azure tenantID, clientID, and clientSecret.
func NewClientCredentialsAuthenticationProvider(tenantID, clientID, clientSecret string) (*ClientCredentialsAuthenticationProvider, error) {
	if tenantID == "" {
		return nil, errors.New("tenantID cannot be empty")
	}
	if clientID == "" {
		return nil, errors.New("clientID cannot be empty")
	}
	if clientSecret == "" {
		return nil, errors.New("clientSecret cannot be empty")
	}
	return &ClientCredentialsAuthenticationProvider{
		tenantID:      tenantID,
		clientID:      clientID,
		clientSecret:  clientSecret,
		tokenEndpoint: fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", tenantID),
		httpClient:    &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// SetTokenEndpoint allows overriding the token endpoint (useful for testing).
func (c *ClientCredentialsAuthenticationProvider) SetTokenEndpoint(endpoint string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokenEndpoint = endpoint
}

// SetHTTPClient allows overriding the internal HTTP client (useful for mock testing).
func (c *ClientCredentialsAuthenticationProvider) SetHTTPClient(client *http.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.httpClient = client
}

// GetToken returns a valid cached OAuth access token, automatically refreshing if expired or near expiration.
func (c *ClientCredentialsAuthenticationProvider) GetToken(ctx context.Context) (string, error) {
	c.mu.RLock()
	if c.accessToken != "" && time.Now().Add(5*time.Minute).Before(c.expiresAt) {
		token := c.accessToken
		c.mu.RUnlock()
		return token, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	// Double check after acquiring write lock
	if c.accessToken != "" && time.Now().Add(5*time.Minute).Before(c.expiresAt) {
		return c.accessToken, nil
	}

	data := url.Values{}
	data.Set("grant_type", "client_credentials")
	data.Set("client_id", c.clientID)
	data.Set("client_secret", c.clientSecret)
	data.Set("scope", "https://graph.microsoft.com/.default")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenEndpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return "", fmt.Errorf("failed to create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("token request returned HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", fmt.Errorf("failed to decode token response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		return "", errors.New("token response missing access_token")
	}

	c.accessToken = tokenResp.AccessToken
	expiresInSec := tokenResp.ExpiresIn
	if expiresInSec <= 0 {
		expiresInSec = 3600
	}
	c.expiresAt = time.Now().Add(time.Duration(expiresInSec-60) * time.Second)

	return c.accessToken, nil
}

// AuthenticateRequest adds the Authorization header to the Kiota request.
func (c *ClientCredentialsAuthenticationProvider) AuthenticateRequest(ctx context.Context, request *abstractions.RequestInformation, additionalAuthenticationContext map[string]interface{}) error {
	if request == nil {
		return errors.New("request cannot be nil")
	}
	token, err := c.GetToken(ctx)
	if err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}
	if request.Headers == nil {
		request.Headers = abstractions.NewRequestHeaders()
	}
	request.Headers.Add("Authorization", "Bearer "+token)
	return nil
}

// GetAuthorizationToken is required by Kiota's AccessTokenProvider interface.
func (c *ClientCredentialsAuthenticationProvider) GetAuthorizationToken(ctx context.Context, request *url.URL, additionalAuthenticationContext map[string]interface{}) (string, error) {
	return c.GetToken(ctx)
}
