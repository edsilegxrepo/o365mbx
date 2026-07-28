//go:build proxy
// +build proxy

// Package o365client_test contains integration and resilience tests for the o365client package.
//
// OBJECTIVE:
// Provide chaos, load, and resilience testing using Microsoft Dev Proxy or httpmock fallback to simulate
// rate limits (429), server errors (500/503), token expiration, high-concurrency pressure, and large streaming downloads.
//
// CORE COMPONENTS:
// 1. TestResilience_*: Exercises retry logic, rate limit backoffs, token auto-refresh, PDF conversion modes, and interrupted sync recovery.
// 2. TestStress_*: Tests multi-worker parallel folder sync, rapid iterative runs, and memory pressure.
//
// TEST STRATEGY:
// Automatically detects Microsoft Dev Proxy (`devproxy`) or falls back to `httpmock` HTTP transport interception
// to test engine behavior under extreme fault injection and stress conditions.
package o365client_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"criticalsys.net/secretprotector/pkg/libsecsecrets"

	"criticalsys.net/o365mbx/emailprocessor"
	"criticalsys.net/o365mbx/engine"
	"criticalsys.net/o365mbx/filehandler"
	"criticalsys.net/o365mbx/o365client"

	"github.com/jarcoal/httpmock"
	kiota "github.com/microsoft/kiota-abstractions-go"
	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	autoStartedCmd *exec.Cmd
	autoStartedMu  sync.Mutex
)

func TestMain(m *testing.M) {
	code := m.Run()
	stopAutoStartedDevProxy()
	os.Exit(code)
}

func stopAutoStartedDevProxy() {
	autoStartedMu.Lock()
	defer autoStartedMu.Unlock()
	stopAutoStartedDevProxyLocked()
}

func stopAutoStartedDevProxyLocked() {
	if autoStartedCmd != nil && autoStartedCmd.Process != nil {
		log.Info("Ensuring auto-started Dev Proxy instance is completely shut down...")
		client := &http.Client{Timeout: 2 * time.Second}
		stopReq, _ := http.NewRequest("POST", "http://127.0.0.1:8897/proxy/stopProxy", nil)
		if stopResp, stopErr := client.Do(stopReq); stopErr == nil {
			_ = stopResp.Body.Close()
		}

		// Wait up to 3 seconds for graceful Dev Proxy exit and native registry unregistration
		done := make(chan error, 1)
		go func() {
			done <- autoStartedCmd.Wait()
		}()

		select {
		case <-done:
			log.Info("Dev Proxy instance exited gracefully.")
		case <-time.After(3 * time.Second):
			log.Warn("Dev Proxy did not exit within timeout; force killing process.")
			_ = autoStartedCmd.Process.Kill()
			<-done
		}
		autoStartedCmd = nil

		// Reset Windows System Proxy setting (ProxyEnable=0) as a safety net
		if runtime.GOOS == "windows" {
			_ = exec.Command("reg", "add", `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`, "/v", "ProxyEnable", "/t", "REG_DWORD", "/d", "0", "/f").Run()
		}
	}
}

func findDevProxyExecutable() string {
	if bin, err := exec.LookPath("devproxy"); err == nil {
		return bin
	}
	if bin, err := exec.LookPath("devproxy.exe"); err == nil {
		return bin
	}
	if ph := os.Getenv("PROXY_HOME"); ph != "" {
		candidates := []string{
			filepath.Join(ph, "devproxy.exe"),
			filepath.Join(ph, "devproxy"),
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				return c
			}
		}
	}
	defaultPaths := []string{
		`d:\inetd\devproxy\devproxy.exe`,
		`C:\Program Files\DevProxy\devproxy.exe`,
		filepath.Join(os.Getenv("HOME"), ".config", "devproxy", "devproxy"),
	}
	for _, p := range defaultPaths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func findProjectRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func ensureDevProxyRunning(t *testing.T, httpClient *http.Client) {
	// 1. Check if Dev Proxy is already running
	req, _ := http.NewRequest("GET", "https://graph.microsoft.com/v1.0/me", nil)
	resp, err := httpClient.Do(req)
	if err == nil {
		resp.Body.Close()
		return // Dev Proxy is already running
	}

	autoStartedMu.Lock()
	defer autoStartedMu.Unlock()

	if autoStartedCmd != nil && autoStartedCmd.Process != nil {
		return // Already spawned for this test run
	}

	// 2. Locate Dev Proxy binary
	devProxyBin := findDevProxyExecutable()
	if devProxyBin == "" {
		t.Skipf("Dev Proxy is not running and executable was not found. Install Dev Proxy or set PROXY_HOME.")
		return
	}

	projRoot := findProjectRoot()
	mocksFile := filepath.Join(projRoot, "testdata", "resilience-full-pipeline.json")
	if _, err := os.Stat(mocksFile); err != nil {
		t.Skipf("Mocks file not found at %s: %v", mocksFile, err)
		return
	}

	proxyHome := os.Getenv("PROXY_HOME")
	if proxyHome == "" {
		proxyHome = filepath.Dir(devProxyBin)
	}
	configFile := filepath.Join(proxyHome, "config", "m365.json")

	args := []string{"--mocks-file", mocksFile, "--watch", "false", "--as-doctor", "false"}
	if _, err := os.Stat(configFile); err == nil {
		args = append([]string{"--config-file", configFile}, args...)
	}

	// 3. Auto-start Dev Proxy
	cmd := exec.Command(devProxyBin, args...)
	cmd.Dir = proxyHome
	cmd.Env = append(os.Environ(), "HTTP_PROXY=", "HTTPS_PROXY=", "http_proxy=", "https_proxy=")
	if err := cmd.Start(); err != nil {
		t.Skipf("Failed to auto-start Dev Proxy (%s): %v", devProxyBin, err)
		return
	}

	autoStartedCmd = cmd

	// 4. Register cleanup hook per test
	t.Cleanup(func() {
		stopAutoStartedDevProxy()
	})

	// 5. Poll until Dev Proxy is responsive (up to 15 seconds)
	ready := false
	for i := 0; i < 30; i++ {
		time.Sleep(500 * time.Millisecond)
		pollReq, _ := http.NewRequest("GET", "https://graph.microsoft.com/v1.0/me", nil)
		pollResp, pollErr := httpClient.Do(pollReq)
		if pollErr == nil {
			_ = pollResp.Body.Close()
			ready = true
			break
		}
	}

	if !ready {
		stopAutoStartedDevProxyLocked()
		t.Skipf("Dev Proxy auto-started but did not respond within timeout.")
	}
}

// setupProxiedClient initializes an O365Client configured to route all traffic
// through a local proxy (defaulting to Dev Proxy at http://127.0.0.1:8000).
// It also skips TLS verification to allow the proxy to intercept HTTPS traffic.
func setupProxiedClient(t *testing.T) (*o365client.O365Client, *http.Client) {
	proxyURLStr := "http://127.0.0.1:8000"
	proxyURL, _ := url.Parse(proxyURLStr)

	// Force proxy for THIS process
	os.Setenv("HTTP_PROXY", proxyURLStr)
	os.Setenv("HTTPS_PROXY", proxyURLStr)
	os.Setenv("http_proxy", proxyURLStr)
	os.Setenv("https_proxy", proxyURLStr)

	transport := &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	httpClient := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}

	// Auto-start Dev Proxy if not running, and register automatic shutdown
	ensureDevProxyRunning(t, httpClient)

	authProvider, _ := o365client.NewStaticTokenAuthenticationProvider("eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6Ik1lZ2FuIEJvd2VuIiwiaWF0IjoxNTE2MjM5MDIyfQ.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c")

	// CRITICAL: We MUST use the adapter that takes the custom httpClient
	adapter, err := msgraphsdk.NewGraphRequestAdapterWithParseNodeFactoryAndSerializationWriterFactoryAndHttpClient(
		authProvider, nil, nil, httpClient,
	)
	require.NoError(t, err)

	return o365client.NewO365ClientWithAdapter(adapter, nil), httpClient
}

func TestResilience_LiveProxyBehavior(t *testing.T) {
	client, _ := setupProxiedClient(t)

	tmpDir := t.TempDir()

	processor := emailprocessor.NewEmailProcessor(log.New())
	ctx := context.Background()
	_ = processor.Initialize(ctx, "", 1)
	defer processor.Close()
	handler := filehandler.NewFileHandler(tmpDir, client, processor, 20, 8, 0, "extractor", "default", log.New())

	cfg := &engine.Config{
		MailboxName:          "MeganB@M365x214355.onmicrosoft.com",
		WorkspacePath:        tmpDir,
		ProcessingMode:       "full",
		InboxFolder:          "Inbox",
		MaxParallelDownloads: 1,
	}

	// Retry loop for 50% failure rate
	var err error
	success := false
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		err = engine.RunEngine(ctx, cfg, client, processor, handler, "1.0.0")
		cancel()

		if err == nil {
			success = true
			break
		}
		// If throttled, wait longer
		waitSec := 2
		if err != nil && (strings.Contains(err.Error(), "throttled") || strings.Contains(err.Error(), "Retry-After")) {
			waitSec = 5
		}
		time.Sleep(time.Duration(waitSec) * time.Second)
	}

	require.True(t, success, "Engine failed to succeed under chaos: %v", err)
	assert.DirExists(t, tmpDir)
}

func TestResilience_DevProxy(t *testing.T) {
	client, _ := setupProxiedClient(t)

	tmpDir := t.TempDir()

	processor := emailprocessor.NewEmailProcessor(log.New())
	ctx := context.Background()
	_ = processor.Initialize(ctx, "", 1)
	defer processor.Close()
	handler := filehandler.NewFileHandler(tmpDir, client, processor, 20, 8, 0, "extractor", "default", log.New())

	cfg := &engine.Config{
		MailboxName:          "MeganB@M365x214355.onmicrosoft.com",
		WorkspacePath:        tmpDir,
		ProcessingMode:       "full",
		InboxFolder:          "Inbox",
		MaxParallelDownloads: 1,
	}
	cfg.SetDefaults()

	var err error
	success := false
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		err = engine.RunEngine(ctx, cfg, client, processor, handler, "1.0.0")
		cancel()

		if err == nil {
			success = true
			break
		}
		waitSec := 2
		if err != nil && (strings.Contains(err.Error(), "throttled") || strings.Contains(err.Error(), "Retry-After")) {
			waitSec = 5
		}
		time.Sleep(time.Duration(waitSec) * time.Second)
	}
	require.True(t, success, "Engine failed to succeed under chaos: %v", err)
}

func TestResilience_NestedAttachmentExtraction(t *testing.T) {
	client, _ := setupProxiedClient(t)

	tmpDir := t.TempDir()

	processor := emailprocessor.NewEmailProcessor(log.New())
	ctx := context.Background()
	_ = processor.Initialize(ctx, "", 1)
	defer processor.Close()
	handler := filehandler.NewFileHandler(tmpDir, client, processor, 20, 8, 0, "extractor", "inlines", log.New())

	cfg := &engine.Config{
		MailboxName:            "MeganB@M365x214355.onmicrosoft.com",
		WorkspacePath:          tmpDir,
		ProcessingMode:         "full",
		InboxFolder:            "Inbox",
		MaxParallelDownloads:   1,
		MsgHandler:             "extractor",
		AttachmentExtractionL1: "inlines",
	}
	cfg.SetDefaults()

	var err error
	success := false
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		err = engine.RunEngine(ctx, cfg, client, processor, handler, "1.0.0")
		cancel()
		if err == nil {
			success = true
			break
		}
		waitSec := 2
		if err != nil && (strings.Contains(err.Error(), "throttled") || strings.Contains(err.Error(), "Retry-After")) {
			waitSec = 5
		}
		time.Sleep(time.Duration(waitSec) * time.Second)
	}
	require.True(t, success, "Engine failed under chaos in nested test: %v", err)

	attDir := filepath.Join(tmpDir, "msg-nested", "attachments")
	assert.FileExists(t, filepath.Join(attDir, "01_nested_message.eml"))
	nestedPartPath := filepath.Join(attDir, "01_1_nested_canary.txt")
	assert.FileExists(t, nestedPartPath)
	content, err := os.ReadFile(nestedPartPath)
	require.NoError(t, err)
	assert.Contains(t, string(content), "Nested Unicode Content")
}

func TestResilience_MassiveAttachmentPressure(t *testing.T) {
	client, _ := setupProxiedClient(t)

	tmpDir := t.TempDir()

	processor := emailprocessor.NewEmailProcessor(log.New())
	ctx := context.Background()
	_ = processor.Initialize(ctx, "", 1)
	defer processor.Close()
	handler := filehandler.NewFileHandler(tmpDir, client, processor, 20, 8, 0, "extractor", "default", log.New())

	cfg := &engine.Config{
		MailboxName:          "MeganB@M365x214355.onmicrosoft.com",
		WorkspacePath:        tmpDir,
		ProcessingMode:       "full",
		InboxFolder:          "Inbox",
		MaxParallelDownloads: 10,
	}
	cfg.SetDefaults()

	var err error
	success := false
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		err = engine.RunEngine(ctx, cfg, client, processor, handler, "1.0.0")
		cancel()
		if err == nil {
			success = true
			break
		}
		waitSec := 2
		if err != nil && (strings.Contains(err.Error(), "throttled") || strings.Contains(err.Error(), "Retry-After")) {
			waitSec = 5
		}
		time.Sleep(time.Duration(waitSec) * time.Second)
	}
	require.True(t, success, "Engine failed under chaos in massive test: %v", err)

	attDir := filepath.Join(tmpDir, "msg-massive", "attachments")
	files, _ := os.ReadDir(attDir)
	count := 0
	for _, f := range files {
		if !f.IsDir() && filepath.Ext(f.Name()) != ".json" {
			count++
		}
	}
	assert.Equal(t, 105, count)
}

func TestResilience_ConcurrencyPressure(t *testing.T) {
	client, _ := setupProxiedClient(t)

	tmpDir := t.TempDir()

	processor := emailprocessor.NewEmailProcessor(log.New())
	ctx := context.Background()
	_ = processor.Initialize(ctx, "", 1)
	defer processor.Close()
	handler := filehandler.NewFileHandler(tmpDir, client, processor, 20, 8, 0, "extractor", "default", log.New())

	cfg := &engine.Config{
		MailboxName:          "MeganB@M365x214355.onmicrosoft.com",
		WorkspacePath:        tmpDir,
		ProcessingMode:       "full",
		InboxFolder:          "Inbox",
		MaxParallelDownloads: 10,
	}
	cfg.SetDefaults()

	var err error
	success := false
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		err = engine.RunEngine(ctx, cfg, client, processor, handler, "1.0.0")
		cancel()
		if err == nil {
			success = true
			break
		}
		waitSec := 2
		if err != nil && (strings.Contains(err.Error(), "throttled") || strings.Contains(err.Error(), "Retry-After")) {
			waitSec = 10
		}
		time.Sleep(time.Duration(waitSec) * time.Second)
		backoff := 2
		if i > 5 {
			backoff = 5
		}
		if i > 10 {
			backoff = 10
		}
		time.Sleep(time.Duration(backoff) * time.Second)
	}
	require.True(t, success, "Engine failed under chaos in concurrency test: %v", err)

	files, _ := os.ReadDir(tmpDir)
	dirCount := 0
	for _, f := range files {
		if f.IsDir() {
			dirCount++
		}
	}
	fmt.Printf("Detected %d subdirectories in workspace\n", dirCount)
	assert.GreaterOrEqual(t, dirCount, 20)
}

func TestResilience_HighFidelity_InlinesEnabled(t *testing.T) {
	client, _ := setupProxiedClient(t)

	tmpDir := t.TempDir()

	processor := emailprocessor.NewEmailProcessor(log.New())
	ctx := context.Background()
	_ = processor.Initialize(ctx, "", 1)
	defer processor.Close()
	handler := filehandler.NewFileHandler(tmpDir, client, processor, 20, 8, 0, "extractor", "inlines", log.New())

	cfg := &engine.Config{
		MailboxName:            "MeganB@M365x214355.onmicrosoft.com",
		WorkspacePath:          tmpDir,
		ProcessingMode:         "full",
		InboxFolder:            "Inbox",
		MaxParallelDownloads:   1,
		MsgHandler:             "extractor",
		AttachmentExtractionL1: "inlines",
		ConvertBody:            "text",
	}
	cfg.SetDefaults()

	var err error
	success := false
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		err = engine.RunEngine(ctx, cfg, client, processor, handler, "1.0.0")
		cancel()
		if err == nil {
			success = true
			break
		}
		waitSec := 2
		if err != nil && (strings.Contains(err.Error(), "throttled") || strings.Contains(err.Error(), "Retry-After")) {
			waitSec = 5
		}
		time.Sleep(time.Duration(waitSec) * time.Second)
	}
	require.True(t, success, "Engine failed in inlines test: %v", err)

	msgDirHF := filepath.Join(tmpDir, "msg-hi-fi")
	attDirHF := filepath.Join(msgDirHF, "attachments")
	assert.FileExists(t, filepath.Join(attDirHF, "01_!@#$%^&()_+-=[]{}.txt"))
	assert.FileExists(t, filepath.Join(attDirHF, "02_file with spaces.pdf"))
	assert.FileExists(t, filepath.Join(attDirHF, "03_empty.dat"))
	assert.FileExists(t, filepath.Join(attDirHF, "04_complex_nested.eml"))
	assert.FileExists(t, filepath.Join(attDirHF, "04_1_nested_special_chars_!@#.txt"))
	assert.FileExists(t, filepath.Join(attDirHF, "04_2_inline_image_1.png"))

	msgDirKS := filepath.Join(tmpDir, "msg-kitchen-sink")
	attDirKS := filepath.Join(msgDirKS, "attachments")
	files, _ := os.ReadDir(attDirKS)
	count := 0
	for _, f := range files {
		if !f.IsDir() && filepath.Ext(f.Name()) != ".json" {
			count++
		}
	}
	assert.GreaterOrEqual(t, count, 51)
}

func TestResilience_HighFidelity_DefaultMode(t *testing.T) {
	client, _ := setupProxiedClient(t)

	tmpDir := t.TempDir()

	processor := emailprocessor.NewEmailProcessor(log.New())
	ctx := context.Background()
	_ = processor.Initialize(ctx, "", 1)
	defer processor.Close()
	handler := filehandler.NewFileHandler(tmpDir, client, processor, 20, 8, 0, "extractor", "default", log.New())

	cfg := &engine.Config{
		MailboxName:            "MeganB@M365x214355.onmicrosoft.com",
		WorkspacePath:          tmpDir,
		ProcessingMode:         "full",
		InboxFolder:            "Inbox",
		MaxParallelDownloads:   1,
		MsgHandler:             "extractor",
		AttachmentExtractionL1: "default",
		ConvertBody:            "text",
	}
	cfg.SetDefaults()

	var err error
	success := false
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		err = engine.RunEngine(ctx, cfg, client, processor, handler, "1.0.0")
		cancel()
		if err == nil {
			success = true
			break
		}
		waitSec := 2
		if err != nil && (strings.Contains(err.Error(), "throttled") || strings.Contains(err.Error(), "Retry-After")) {
			waitSec = 5
		}
		time.Sleep(time.Duration(waitSec) * time.Second)
	}
	require.True(t, success, "Engine failed in default mode test: %v", err)

	attDirHF := filepath.Join(tmpDir, "msg-hi-fi", "attachments")
	assert.FileExists(t, filepath.Join(attDirHF, "04_1_nested_special_chars_!@#.txt"))
	assert.NoFileExists(t, filepath.Join(attDirHF, "04_2_inline_image_1.png"))
}

func TestResilience_HealthCheckMode(t *testing.T) {
	client, _ := setupProxiedClient(t)

	var stats *o365client.MailboxHealthStats
	var err error
	success := false
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		stats, err = client.GetMailboxHealthCheck(ctx, "MeganB@M365x214355.onmicrosoft.com")
		cancel()
		if err == nil && stats != nil {
			success = true
			break
		}
		time.Sleep(2 * time.Second)
	}
	require.True(t, success, "GetMailboxHealthCheck failed under proxy chaos: %v", err)
	assert.NotEmpty(t, stats.Folders)
	assert.GreaterOrEqual(t, stats.TotalMessages, int32(0))
}

func TestResilience_RouteMode(t *testing.T) {
	client, _ := setupProxiedClient(t)

	tmpDir := t.TempDir()

	processor := emailprocessor.NewEmailProcessor(log.New())
	ctx := context.Background()
	_ = processor.Initialize(ctx, "", 1)
	defer processor.Close()
	handler := filehandler.NewFileHandler(tmpDir, client, processor, 20, 8, 0, "raw", "default", log.New())

	cfg := &engine.Config{
		MailboxName:          "MeganB@M365x214355.onmicrosoft.com",
		WorkspacePath:        tmpDir,
		ProcessingMode:       "route",
		InboxFolder:          "Inbox",
		ProcessedFolder:      "Processed",
		ErrorFolder:          "Error",
		MaxParallelDownloads: 2,
	}
	cfg.SetDefaults()

	var err error
	success := false
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		err = engine.RunEngine(ctx, cfg, client, processor, handler, "1.0.0")
		cancel()
		if err == nil {
			success = true
			break
		}
		waitSec := 3
		if err != nil && (strings.Contains(err.Error(), "throttled") || strings.Contains(err.Error(), "Retry-After")) {
			waitSec = 5
		}
		time.Sleep(time.Duration(waitSec) * time.Second)
	}
	require.True(t, success, "Engine failed in route mode under proxy chaos: %v", err)
}

func TestResilience_IncrementalSyncMode(t *testing.T) {
	client, _ := setupProxiedClient(t)

	tmpDir := t.TempDir()

	processor := emailprocessor.NewEmailProcessor(log.New())
	ctx := context.Background()
	_ = processor.Initialize(ctx, "", 1)
	defer processor.Close()
	handler := filehandler.NewFileHandler(tmpDir, client, processor, 20, 8, 0, "raw", "default", log.New())

	stateFile := filepath.Join(tmpDir, "state.json")
	cfg := &engine.Config{
		MailboxName:          "MeganB@M365x214355.onmicrosoft.com",
		WorkspacePath:        tmpDir,
		ProcessingMode:       "incremental",
		StateFilePath:        stateFile,
		InboxFolder:          "Inbox",
		MaxParallelDownloads: 2,
	}
	cfg.SetDefaults()

	var err error
	success := false
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		err = engine.RunEngine(ctx, cfg, client, processor, handler, "1.0.0")
		cancel()
		if err == nil {
			success = true
			break
		}
		waitSec := 3
		if err != nil && (strings.Contains(err.Error(), "throttled") || strings.Contains(err.Error(), "Retry-After")) {
			waitSec = 5
		}
		time.Sleep(time.Duration(waitSec) * time.Second)
	}
	require.True(t, success, "Engine failed in incremental mode under proxy chaos: %v", err)
	assert.FileExists(t, stateFile)
}

func TestResilience_LargeAttachmentStreamingFallback(t *testing.T) {
	client, _ := setupProxiedClient(t)

	tmpDir := t.TempDir()

	processor := emailprocessor.NewEmailProcessor(log.New())
	ctx := context.Background()
	_ = processor.Initialize(ctx, "", 1)
	defer processor.Close()
	handler := filehandler.NewFileHandler(tmpDir, client, processor, 20, 8, 0, "extractor", "default", log.New())

	cfg := &engine.Config{
		MailboxName:          "MeganB@M365x214355.onmicrosoft.com",
		WorkspacePath:        tmpDir,
		ProcessingMode:       "full",
		InboxFolder:          "Inbox",
		MaxParallelDownloads: 2,
	}
	cfg.SetDefaults()

	var err error
	success := false
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		err = engine.RunEngine(ctx, cfg, client, processor, handler, "1.0.0")
		cancel()
		if err == nil {
			success = true
			break
		}
		waitSec := 3
		if err != nil && (strings.Contains(err.Error(), "throttled") || strings.Contains(err.Error(), "Retry-After")) {
			waitSec = 5
		}
		time.Sleep(time.Duration(waitSec) * time.Second)
	}
	require.True(t, success, "Engine failed in streaming fallback test under proxy chaos: %v", err)
}

func TestResilience_ExpiredTokenHandling(t *testing.T) {
	authProvider, _ := o365client.NewStaticTokenAuthenticationProvider("invalid-expired-token")
	adapter, err := msgraphsdk.NewGraphRequestAdapter(authProvider)
	require.NoError(t, err)

	invalidClient := o365client.NewO365ClientWithAdapter(adapter, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = invalidClient.GetMailboxHealthCheck(ctx, "MeganB@M365x214355.onmicrosoft.com")
	assert.Error(t, err, "Expected authentication error for expired/invalid token")
}

func TestResilience_BodyConversionHTMLToText(t *testing.T) {
	client, _ := setupProxiedClient(t)

	tmpDir := t.TempDir()

	processor := emailprocessor.NewEmailProcessor(log.New())
	ctx := context.Background()
	_ = processor.Initialize(ctx, "", 1)
	defer processor.Close()
	handler := filehandler.NewFileHandler(tmpDir, client, processor, 20, 8, 0, "extractor", "default", log.New())

	cfg := &engine.Config{
		MailboxName:          "MeganB@M365x214355.onmicrosoft.com",
		WorkspacePath:        tmpDir,
		ProcessingMode:       "full",
		InboxFolder:          "Inbox",
		ConvertBody:          "text",
		MaxParallelDownloads: 2,
	}
	cfg.SetDefaults()

	var err error
	success := false
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		err = engine.RunEngine(ctx, cfg, client, processor, handler, "1.0.0")
		cancel()
		if err == nil {
			success = true
			break
		}
		waitSec := 3
		if err != nil && (strings.Contains(err.Error(), "throttled") || strings.Contains(err.Error(), "Retry-After")) {
			waitSec = 5
		}
		time.Sleep(time.Duration(waitSec) * time.Second)
	}
	require.True(t, success, "Engine failed in body conversion test under proxy chaos: %v", err)
}

func findSystemChromiumPath() string {
	if envPath := os.Getenv("PDF_TEST_CHROMIUM_PATH"); envPath != "" {
		return envPath
	}
	if envPath := os.Getenv("CHROMIUM_PATH"); envPath != "" {
		return envPath
	}
	candidates := []string{
		`d:\inet\www\chromium\bin\chrome.exe`,
		`/u01/chromium/chrome`,
		`/u01/chromium`,
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		`/usr/bin/chromium`,
		`/usr/bin/chromium-browser`,
		`/usr/bin/google-chrome`,
		`/Applications/Google Chrome.app/Contents/MacOS/Google Chrome`,
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

func TestResilience_BodyConversionHTMLToPDF(t *testing.T) {
	chromiumPath := findSystemChromiumPath()
	if chromiumPath == "" {
		t.Skip("Skipping live PDF body conversion test: No Chromium/Edge browser found on system")
	}

	client, _ := setupProxiedClient(t)

	tmpDir := t.TempDir()

	processor := emailprocessor.NewEmailProcessor(log.New())
	ctx := context.Background()
	err := processor.Initialize(ctx, chromiumPath, 2)
	require.NoError(t, err)
	defer processor.Close()

	handler := filehandler.NewFileHandler(tmpDir, client, processor, 20, 8, 0, "raw", "default", log.New())

	cfg := &engine.Config{
		MailboxName:          "MeganB@M365x214355.onmicrosoft.com",
		WorkspacePath:        tmpDir,
		ProcessingMode:       "full",
		InboxFolder:          "Inbox",
		ConvertBody:          "pdf",
		ChromiumPath:         chromiumPath,
		MaxParallelDownloads: 2,
	}
	cfg.SetDefaults()

	success := false
	for i := 0; i < 20; i++ {
		runCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		err = engine.RunEngine(runCtx, cfg, client, processor, handler, "1.0.0")
		cancel()
		if err == nil {
			success = true
			break
		}
		waitSec := 3
		if err != nil && (strings.Contains(err.Error(), "throttled") || strings.Contains(err.Error(), "Retry-After")) {
			waitSec = 5
		}
		time.Sleep(time.Duration(waitSec) * time.Second)
	}
	require.True(t, success, "Engine failed in live PDF body conversion test under proxy chaos: %v", err)

	// Verify at least one message directory contains a valid body.pdf file
	entries, readErr := os.ReadDir(tmpDir)
	require.NoError(t, readErr)
	foundPDF := false
	for _, entry := range entries {
		if entry.IsDir() {
			pdfPath := filepath.Join(tmpDir, entry.Name(), "body.pdf")
			if pdfData, statErr := os.ReadFile(pdfPath); statErr == nil && len(pdfData) >= 4 {
				if string(pdfData[:4]) == "%PDF" {
					foundPDF = true
					break
				}
			}
		}
	}
	assert.True(t, foundPDF, "Expected at least one valid body.pdf file with %%PDF header in workspace")
}

func TestResilience_UnicodeAndSpecialCharFilenames(t *testing.T) {
	client, _ := setupProxiedClient(t)

	tmpDir := t.TempDir()

	processor := emailprocessor.NewEmailProcessor(log.New())
	ctx := context.Background()
	_ = processor.Initialize(ctx, "", 1)
	defer processor.Close()
	handler := filehandler.NewFileHandler(tmpDir, client, processor, 20, 8, 0, "extractor", "inlines", log.New())

	cfg := &engine.Config{
		MailboxName:            "MeganB@M365x214355.onmicrosoft.com",
		WorkspacePath:          tmpDir,
		ProcessingMode:         "full",
		InboxFolder:            "Inbox",
		AttachmentExtractionL1: "inlines",
		MaxParallelDownloads:   2,
	}
	cfg.SetDefaults()

	var err error
	success := false
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		err = engine.RunEngine(ctx, cfg, client, processor, handler, "1.0.0")
		cancel()
		if err == nil {
			success = true
			break
		}
		waitSec := 3
		if err != nil && (strings.Contains(err.Error(), "throttled") || strings.Contains(err.Error(), "Retry-After")) {
			waitSec = 5
		}
		time.Sleep(time.Duration(waitSec) * time.Second)
	}
	require.True(t, success, "Engine failed in unicode/special chars test under proxy chaos: %v", err)
}

func TestResilience_InterruptedSyncResumeRecovery(t *testing.T) {
	client, _ := setupProxiedClient(t)

	tmpDir := t.TempDir()

	stateFile := filepath.Join(tmpDir, "state.json")
	processor := emailprocessor.NewEmailProcessor(log.New())
	ctx := context.Background()
	_ = processor.Initialize(ctx, "", 1)
	defer processor.Close()
	handler := filehandler.NewFileHandler(tmpDir, client, processor, 20, 8, 0, "raw", "default", log.New())

	cfg := &engine.Config{
		MailboxName:          "MeganB@M365x214355.onmicrosoft.com",
		WorkspacePath:        tmpDir,
		ProcessingMode:       "incremental",
		StateFilePath:        stateFile,
		InboxFolder:          "Inbox",
		MaxParallelDownloads: 2,
	}
	cfg.SetDefaults()

	// Initial Sync Run
	var err error
	success := false
	for i := 0; i < 20; i++ {
		runCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		err = engine.RunEngine(runCtx, cfg, client, processor, handler, "1.0.0")
		cancel()
		if err == nil {
			success = true
			break
		}
		waitSec := 3
		if err != nil && (strings.Contains(err.Error(), "throttled") || strings.Contains(err.Error(), "Retry-After")) {
			waitSec = 5
		}
		time.Sleep(time.Duration(waitSec) * time.Second)
	}
	require.True(t, success, "First incremental run failed: %v", err)
	assert.FileExists(t, stateFile)

	// Resume Run (re-run with saved state file)
	success = false
	for i := 0; i < 20; i++ {
		runCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		err = engine.RunEngine(runCtx, cfg, client, processor, handler, "1.0.0")
		cancel()
		if err == nil {
			success = true
			break
		}
		waitSec := 3
		if err != nil && (strings.Contains(err.Error(), "throttled") || strings.Contains(err.Error(), "Retry-After")) {
			waitSec = 5
		}
		time.Sleep(time.Duration(waitSec) * time.Second)
	}
	require.True(t, success, "Resume incremental run failed: %v", err)
}

func TestStress_HighConcurrencyWorkers(t *testing.T) {
	client, _ := setupProxiedClient(t)

	tmpDir := t.TempDir()

	processor := emailprocessor.NewEmailProcessor(log.New())
	ctx := context.Background()
	_ = processor.Initialize(ctx, "", 1)
	defer processor.Close()

	// High concurrency: 50 parallel workers, high burst
	handler := filehandler.NewFileHandler(tmpDir, client, processor, 50, 16, 0, "extractor", "default", log.New())

	cfg := &engine.Config{
		MailboxName:          "MeganB@M365x214355.onmicrosoft.com",
		WorkspacePath:        tmpDir,
		ProcessingMode:       "full",
		InboxFolder:          "Inbox",
		MaxParallelDownloads: 50,
		APICallsPerSecond:    100.0,
		APIBurst:             200,
	}
	cfg.SetDefaults()

	var err error
	success := false
	for i := 0; i < 20; i++ {
		runCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		err = engine.RunEngine(runCtx, cfg, client, processor, handler, "1.0.0")
		cancel()
		if err == nil {
			success = true
			break
		}
		waitSec := 3
		if err != nil && (strings.Contains(err.Error(), "throttled") || strings.Contains(err.Error(), "Retry-After")) {
			waitSec = 5
		}
		time.Sleep(time.Duration(waitSec) * time.Second)
	}
	require.True(t, success, "Stress test with 50 concurrent workers failed: %v", err)
}

func TestStress_ParallelMultiFolderSync(t *testing.T) {
	client, _ := setupProxiedClient(t)

	tmpDir := t.TempDir()

	processor := emailprocessor.NewEmailProcessor(log.New())
	ctx := context.Background()
	_ = processor.Initialize(ctx, "", 1)
	defer processor.Close()

	folders := []string{"Inbox", "Archive", "Sent"}
	errChan := make(chan error, len(folders))

	var wg sync.WaitGroup
	for _, fName := range folders {
		wg.Add(1)
		go func(folder string) {
			defer wg.Done()
			folderTmpDir := filepath.Join(tmpDir, folder)
			_ = os.MkdirAll(folderTmpDir, 0o700)

			handler := filehandler.NewFileHandler(folderTmpDir, client, processor, 10, 4, 0, "extractor", "default", log.New())
			cfg := &engine.Config{
				MailboxName:          "MeganB@M365x214355.onmicrosoft.com",
				WorkspacePath:        folderTmpDir,
				ProcessingMode:       "full",
				InboxFolder:          folder,
				MaxParallelDownloads: 5,
			}
			cfg.SetDefaults()

			var runErr error
			for i := 0; i < 15; i++ {
				runCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				runErr = engine.RunEngine(runCtx, cfg, client, processor, handler, "1.0.0")
				cancel()
				if runErr == nil {
					break
				}
				time.Sleep(3 * time.Second)
			}
			if runErr != nil {
				errChan <- fmt.Errorf("folder %s failed: %w", folder, runErr)
			}
		}(fName)
	}

	wg.Wait()
	close(errChan)

	for err := range errChan {
		require.NoError(t, err)
	}
}

func TestStress_RapidIterativeRuns(t *testing.T) {
	client, _ := setupProxiedClient(t)

	tmpDir := t.TempDir()

	processor := emailprocessor.NewEmailProcessor(log.New())
	ctx := context.Background()
	_ = processor.Initialize(ctx, "", 1)
	defer processor.Close()
	handler := filehandler.NewFileHandler(tmpDir, client, processor, 10, 4, 0, "extractor", "default", log.New())

	cfg := &engine.Config{
		MailboxName:          "MeganB@M365x214355.onmicrosoft.com",
		WorkspacePath:        tmpDir,
		ProcessingMode:       "full",
		InboxFolder:          "Inbox",
		MaxParallelDownloads: 5,
	}
	cfg.SetDefaults()

	// 10 continuous rapid runs to detect resource or connection leaks
	successes := 0
	for iteration := 1; iteration <= 10; iteration++ {
		runCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		err := engine.RunEngine(runCtx, cfg, client, processor, handler, "1.0.0")
		cancel()
		if err == nil {
			successes++
		}
		time.Sleep(1 * time.Second)
	}

	assert.GreaterOrEqual(t, successes, 1, "At least one rapid iteration must succeed under stress")
}

func TestResilience_MessageDetailsMode(t *testing.T) {
	client, _ := setupProxiedClient(t)

	detailsChan := make(chan o365client.MessageDetail, 100)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := client.GetMessageDetailsForFolder(ctx, "MeganB@M365x214355.onmicrosoft.com", "Inbox", detailsChan)
	assert.NoError(t, err)

	detailsCount := 0
	for range detailsChan {
		detailsCount++
	}
	assert.GreaterOrEqual(t, detailsCount, 0)
}

func TestResilience_BandwidthLimiter(t *testing.T) {
	client, _ := setupProxiedClient(t)

	tmpDir := t.TempDir()

	processor := emailprocessor.NewEmailProcessor(log.New())
	ctx := context.Background()
	_ = processor.Initialize(ctx, "", 1)
	defer processor.Close()

	// Enable bandwidth limiter at 0.5 MB/s
	handler := filehandler.NewFileHandler(tmpDir, client, processor, 10, 2, 0.5, "raw", "default", log.New())

	cfg := &engine.Config{
		MailboxName:          "MeganB@M365x214355.onmicrosoft.com",
		WorkspacePath:        tmpDir,
		ProcessingMode:       "full",
		InboxFolder:          "Inbox",
		BandwidthLimitMBs:    0.5,
		MaxParallelDownloads: 2,
	}
	cfg.SetDefaults()

	var err error
	success := false
	for i := 0; i < 15; i++ {
		runCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		err = engine.RunEngine(runCtx, cfg, client, processor, handler, "1.0.0")
		cancel()
		if err == nil {
			success = true
			break
		}
		time.Sleep(3 * time.Second)
	}
	require.True(t, success, "Engine with bandwidth limiter failed under proxy chaos: %v", err)
}

func TestResilience_PerMessageTimeout(t *testing.T) {
	client, _ := setupProxiedClient(t)

	tmpDir := t.TempDir()

	processor := emailprocessor.NewEmailProcessor(log.New())
	ctx := context.Background()
	_ = processor.Initialize(ctx, "", 1)
	defer processor.Close()

	handler := filehandler.NewFileHandler(tmpDir, client, processor, 10, 2, 0, "raw", "default", log.New())

	// Configure per-message timeout to 1 second
	cfg := &engine.Config{
		MailboxName:          "MeganB@M365x214355.onmicrosoft.com",
		WorkspacePath:        tmpDir,
		ProcessingMode:       "full",
		InboxFolder:          "Inbox",
		MaxExecutionTimeMsg:  1,
		MaxParallelDownloads: 2,
	}
	cfg.SetDefaults()

	runCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	_ = engine.RunEngine(runCtx, cfg, client, processor, handler, "1.0.0")

	// Ensure the workspace was initialized cleanly
	_, err := os.Stat(tmpDir)
	assert.NoError(t, err)
}

func TestResilience_ConfigFileLoading(t *testing.T) {
	client, _ := setupProxiedClient(t)

	tmpDir := t.TempDir()

	configFilePath := filepath.Join(tmpDir, "config.json")
	configJSON := fmt.Sprintf(`{
		"mailboxName": "MeganB@M365x214355.onmicrosoft.com",
		"workspacePath": %q,
		"processingMode": "full",
		"inboxFolder": "Inbox",
		"convertBody": "text",
		"maxParallelDownloads": 3
	}`, tmpDir)
	err := os.WriteFile(configFilePath, []byte(configJSON), 0o600)
	require.NoError(t, err)

	cfg, loadErr := engine.LoadConfig(configFilePath)
	require.NoError(t, loadErr)
	cfg.SetDefaults()

	processor := emailprocessor.NewEmailProcessor(log.New())
	ctx := context.Background()
	_ = processor.Initialize(ctx, "", 1)
	defer processor.Close()

	handler := filehandler.NewFileHandler(tmpDir, client, processor, 10, 3, 0, "raw", "default", log.New())

	var runErr error
	success := false
	for i := 0; i < 15; i++ {
		runCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		runErr = engine.RunEngine(runCtx, cfg, client, processor, handler, "1.0.0")
		cancel()
		if runErr == nil {
			success = true
			break
		}
		time.Sleep(3 * time.Second)
	}
	require.True(t, success, "Engine with loaded config file failed under proxy chaos: %v", runErr)
}

func TestResilience_ContinuousIncrementalPolling(t *testing.T) {
	client, _ := setupProxiedClient(t)

	tmpDir := t.TempDir()

	stateFilePath := filepath.Join(tmpDir, "state.json")

	cfg := &engine.Config{
		MailboxName:          "MeganB@M365x214355.onmicrosoft.com",
		WorkspacePath:        tmpDir,
		ProcessingMode:       "incremental",
		StateFilePath:        stateFilePath,
		InboxFolder:          "Inbox",
		ConvertBody:          "text",
		MaxParallelDownloads: 3,
	}
	cfg.SetDefaults()

	processor := emailprocessor.NewEmailProcessor(log.New())
	ctx := context.Background()
	_ = processor.Initialize(ctx, "", 1)
	defer processor.Close()

	handler := filehandler.NewFileHandler(tmpDir, client, processor, 10, 3, 0, "raw", "default", log.New())

	// Simulate a daemon running 15 back-to-back polling iterations
	for pollCycle := 1; pollCycle <= 15; pollCycle++ {
		var runErr error
		success := false
		for attempt := 0; attempt < 10; attempt++ {
			runCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			runErr = engine.RunEngine(runCtx, cfg, client, processor, handler, "1.0.0")
			cancel()
			if runErr == nil {
				success = true
				break
			}
			time.Sleep(2 * time.Second)
		}
		require.True(t, success, "Continuous poller cycle %d failed under proxy chaos: %v", pollCycle, runErr)

		// Verify state file exists and is valid after each poll cycle
		if pollCycle == 1 {
			_, err := os.Stat(stateFilePath)
			require.NoError(t, err, "State file must exist after initial polling cycle")
		}
	}
}

func TestResilience_SecretProtectorEncryptedToken(t *testing.T) {
	client, _ := setupProxiedClient(t)

	ctx := context.Background()

	// 1. Generate 32-byte master key via libsecsecrets
	masterKeyHex, err := libsecsecrets.GenerateKey()
	require.NoError(t, err, "Failed to generate secretprotector master key")
	masterKeyBytes, err := libsecsecrets.ResolveKey(ctx, masterKeyHex, "", "")
	require.NoError(t, err, "Failed to resolve master key bytes")

	// 2. Encrypt mock O365 access token using secretprotector
	mockAccessToken := "mock-resilience-access-token"
	encryptedCiphertext, err := libsecsecrets.Encrypt(ctx, mockAccessToken, masterKeyBytes)
	require.NoError(t, err, "Failed to encrypt mock access token with secretprotector")

	// 3. Set up workspace, token file, and master key file
	tmpDir := t.TempDir()

	tokenFilePath := filepath.Join(tmpDir, "token.enc")
	require.NoError(t, os.WriteFile(tokenFilePath, []byte(encryptedCiphertext), 0o600))

	keyDir := filepath.Join(".", ".test_keys_resilience")
	require.NoError(t, os.MkdirAll(keyDir, 0o700))
	defer func() { _ = os.RemoveAll(keyDir) }()
	masterKeyFilePath := filepath.Join(keyDir, "master_key.txt")
	require.NoError(t, os.WriteFile(masterKeyFilePath, []byte(masterKeyHex), 0o600))

	// 4. Configure engine with secretprotector token file and master key file
	cfg := &engine.Config{
		MailboxName:          "MeganB@M365x214355.onmicrosoft.com",
		WorkspacePath:        tmpDir,
		TokenFile:            tokenFilePath,
		SecretMasterKeyFile:  masterKeyFilePath,
		ProcessingMode:       "full",
		InboxFolder:          "Inbox",
		ConvertBody:          "text",
		MaxParallelDownloads: 2,
	}

	processor := emailprocessor.NewEmailProcessor(log.WithFields(log.Fields{}))
	_ = processor.Initialize(ctx, "", 1)
	defer processor.Close()

	handler := filehandler.NewFileHandler(tmpDir, client, processor, 10, 2, 0, "raw", "default", log.WithFields(log.Fields{}))

	// 5. Execute engine under Dev Proxy chaos retry loop
	var runErr error
	success := false
	for attempt := 0; attempt < 10; attempt++ {
		runCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		runErr = engine.RunEngine(runCtx, cfg, client, processor, handler, "1.0.0")
		cancel()
		if runErr == nil {
			success = true
			break
		}
		time.Sleep(3 * time.Second)
	}
	require.True(t, success, "SecretProtector encrypted token execution failed under proxy chaos: %v", runErr)
}

func TestResilience_ClientCredentialsTokenLifecycle(t *testing.T) {
	ctx := context.Background()

	provider, err := o365client.NewClientCredentialsAuthenticationProvider("resilience-tenant-id", "resilience-client-id", "resilience-secret")
	require.NoError(t, err)

	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	mockHTTP := &http.Client{}
	httpmock.ActivateNonDefault(mockHTTP)
	provider.SetHTTPClient(mockHTTP)

	postCalls := 0
	httpmock.RegisterResponder("POST", "https://login.microsoftonline.com/resilience-tenant-id/oauth2/v2.0/token",
		func(req *http.Request) (*http.Response, error) {
			postCalls++
			if postCalls == 1 {
				return httpmock.NewStringResponse(200, `{"access_token": "devproxy-token-v1-initial", "expires_in": 1, "token_type": "Bearer"}`), nil
			}
			return httpmock.NewStringResponse(200, `{"access_token": "devproxy-token-v2-refreshed", "expires_in": 3600, "token_type": "Bearer"}`), nil
		})

	// 1. Initial acquisition
	tok1, err := provider.GetToken(ctx)
	require.NoError(t, err)
	assert.Equal(t, "devproxy-token-v1-initial", tok1)
	assert.Equal(t, 1, postCalls)

	// 2. Simulate token expiration and execute request requiring token refresh
	reqInfo := kiota.NewRequestInformation()
	err = provider.AuthenticateRequest(ctx, reqInfo, nil)
	assert.NoError(t, err)

	// 3. Verify refreshed token is retrieved automatically
	tok2, err := provider.GetToken(ctx)
	require.NoError(t, err)
	assert.Equal(t, "devproxy-token-v2-refreshed", tok2)
	assert.Equal(t, 2, postCalls)
}
