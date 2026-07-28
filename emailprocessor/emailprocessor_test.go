// Package emailprocessor provides utilities for transforming and cleaning email
// body content, including HTML-to-Text and HTML-to-PDF conversion.
//
// This file contains unit tests for the emailprocessor package.
package emailprocessor

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmailProcessor_IsHTML(t *testing.T) {
	logger := logrus.New()
	ep := NewEmailProcessor(logger)

	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"Plain text", "Hello world", false},
		{"Simple HTML", "<html><body>Hello</body></html>", true},
		{"HTML tag", "Check this <br> tag", true},
		{"Self-closing tag", "Link <img src='foo.png' />", true},
		{"Uppercase tag", "<P>Hello</P>", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ep.IsHTML(tt.content))
		})
	}
}

func TestEmailProcessor_CleanHTML(t *testing.T) {
	logger := logrus.New()
	ep := NewEmailProcessor(logger)

	htmlContent := `
		<html>
			<head><style>body { color: red; }</style></head>
			<body>
				<h1>Title</h1>
				<p>This is a paragraph with a <a href="https://example.com">link</a>.</p>
				<ul>
					<li>Item 1</li>
					<li>Item 2</li>
				</ul>
				<img src="img.png" alt="An image">
				<script>alert('hidden');</script>
			</body>
		</html>
	`

	clean, err := ep.CleanHTML(htmlContent)
	assert.NoError(t, err)
	assert.Contains(t, clean, "Title")
	assert.Contains(t, clean, "This is a paragraph")
	assert.Contains(t, clean, "[link](https://example.com)")
	assert.Contains(t, clean, "Item 1")
	assert.Contains(t, clean, "[Image: An image]")
	assert.NotContains(t, clean, "alert('hidden')")
	assert.NotContains(t, clean, "color: red")
}

func TestEmailProcessor_ProcessBody(t *testing.T) {
	logger := logrus.New()
	ep := NewEmailProcessor(logger)

	html := "<b>Bold</b>"

	// Test "none"
	res, err := ep.ProcessBody(context.Background(), html, "none")
	assert.NoError(t, err)
	assert.Equal(t, html, res)

	// Test "text"
	res, err = ep.ProcessBody(context.Background(), html, "text")
	assert.NoError(t, err)
	assert.Equal(t, "Bold", res)

	// Test invalid
	res, err = ep.ProcessBody(context.Background(), html, "invalid")
	assert.Error(t, err)
	assert.Nil(t, res)

	// Test "pdf" without initialization
	res, err = ep.ProcessBody(context.Background(), html, "pdf")
	assert.Error(t, err)
	assert.Nil(t, res)
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

func TestEmailProcessor_ConvertToPDF(t *testing.T) {
	chromiumPath := findSystemChromiumPath()
	if chromiumPath == "" {
		t.Skip("Skipping PDF conversion test: No Chromium/Edge browser found on system")
	}

	logger := logrus.New()
	ep := NewEmailProcessor(logger)

	ctx := context.Background()
	err := ep.Initialize(ctx, chromiumPath, 2)
	require.NoError(t, err)
	defer func() {
		_ = ep.Close()
	}()

	html := "<html><body><h1>Test PDF Generation</h1><p>High-fidelity rendering test.</p></body></html>"
	pdfBytes, err := ep.ConvertToPDF(ctx, html)

	assert.NoError(t, err)
	assert.NotNil(t, pdfBytes)
	assert.True(t, len(pdfBytes) > 100)

	// Verify PDF header magic bytes (%PDF)
	if len(pdfBytes) >= 4 {
		assert.Equal(t, "%PDF", string(pdfBytes[:4]))
	}
}

func TestEmailProcessor_ConvertToPDF_ComplexLayout(t *testing.T) {
	chromiumPath := findSystemChromiumPath()
	if chromiumPath == "" {
		t.Skip("Skipping PDF conversion test: No Chromium/Edge browser found on system")
	}

	logger := logrus.New()
	ep := NewEmailProcessor(logger)

	ctx := context.Background()
	err := ep.Initialize(ctx, chromiumPath, 2)
	require.NoError(t, err)
	defer func() {
		_ = ep.Close()
	}()

	complexHTML := `
	<!DOCTYPE html>
	<html>
	<head>
		<style>
			body { font-family: sans-serif; background-color: #f4f4f4; margin: 20px; }
			.header { background: #0078d4; color: white; padding: 20px; border-radius: 8px; }
			.content { display: flex; gap: 10px; margin-top: 20px; }
			.card { background: white; padding: 15px; border-radius: 5px; box-shadow: 0 2px 5px rgba(0,0,0,0.1); }
			table { width: 100%; border-collapse: collapse; margin-top: 20px; }
			th, td { border: 1px solid #ddd; padding: 8px; text-align: left; }
			th { background-color: #f2f2f2; }
		</style>
	</head>
	<body>
		<div class="header">
			<h1>Monthly Invoice & Operational Summary 🚀</h1>
			<p>Unicode Test: 日本語, 繁體中文, العربية, 🚀 Mailbox Analytics</p>
		</div>
		<div class="content">
			<div class="card">
				<h3>Total Processed</h3>
				<p>1,250 Messages</p>
			</div>
			<div class="card">
				<h3>Status</h3>
				<p style="color: green; font-weight: bold;">SUCCESS</p>
			</div>
		</div>
		<table>
			<tr><th>Item</th><th>Quantity</th><th>Price</th></tr>
			<tr><td>PDF Engine License</td><td>1</td><td>$0.00</td></tr>
			<tr><td>Graph API Extraction</td><td>1000</td><td>$0.00</td></tr>
		</table>
		<svg height="100" width="100">
			<circle cx="50" cy="50" r="40" stroke="black" stroke-width="3" fill="red" />
		</svg>
	</body>
	</html>
	`

	pdfBytes, err := ep.ConvertToPDF(ctx, complexHTML)
	assert.NoError(t, err)
	assert.NotNil(t, pdfBytes)
	assert.True(t, len(pdfBytes) > 500)
	assert.Equal(t, "%PDF", string(pdfBytes[:4]))
}

func TestEmailProcessor_PoolConcurrency(t *testing.T) {
	chromiumPath := findSystemChromiumPath()
	if chromiumPath == "" {
		t.Skip("Skipping PDF conversion test: No Chromium/Edge browser found on system")
	}

	logger := logrus.New()
	ep := NewEmailProcessor(logger)

	ctx := context.Background()
	// Small pool to test blocking/availability
	err := ep.Initialize(ctx, chromiumPath, 2)
	require.NoError(t, err)
	defer func() {
		_ = ep.Close()
	}()

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			html := fmt.Sprintf("<html><body><h1>Concurrent %d</h1></body></html>", id)
			pdfBytes, err := ep.ConvertToPDF(ctx, html)
			assert.NoError(t, err)
			assert.NotNil(t, pdfBytes)
		}(i)
	}
	wg.Wait()
}

func TestEmailProcessor_Recycling(t *testing.T) {
	chromiumPath := findSystemChromiumPath()
	if chromiumPath == "" {
		t.Skip("Skipping PDF conversion test: No Chromium/Edge browser found on system")
	}

	logger := logrus.New()
	ep := NewEmailProcessor(logger)

	ctx := context.Background()
	err := ep.Initialize(ctx, chromiumPath, 1)
	require.NoError(t, err)
	defer func() {
		_ = ep.Close()
	}()

	// Manually set conversions close to threshold
	atomic.StoreUint64(&ep.conversions, 1000)

	// This call should trigger recycling
	html := "<html><body><h1>Recycled</h1></body></html>"
	pdfBytes, err := ep.ConvertToPDF(ctx, html)
	assert.NoError(t, err)
	assert.NotNil(t, pdfBytes)

	// Counter should be 1 after recycling (it was reset to 0 in startBrowser, then incremented by the call)
	assert.Equal(t, uint64(1), atomic.LoadUint64(&ep.conversions))
}

func TestEmailProcessor_CleanHTML_EdgeCases(t *testing.T) {
	logger := logrus.New()
	ep := NewEmailProcessor(logger)

	// Test empty content
	res, err := ep.CleanHTML("")
	assert.NoError(t, err)
	assert.Empty(t, res)

	// Test link with no text
	res, err = ep.CleanHTML("<a href='http://foo.com'></a>")
	assert.NoError(t, err)
	assert.Contains(t, res, "[http://foo.com](http://foo.com)")

	// Test link with only nested image (current behavior: uses alt text as text if no TextNode present)
	res, err = ep.CleanHTML("<a href='http://foo.com'><img src='bar.png' alt='alt text'></a>")
	assert.NoError(t, err)
	assert.Contains(t, res, "[alt text](http://foo.com)")

	// Test link with text but no href
	res, err = ep.CleanHTML("<a>Just text</a>")
	assert.NoError(t, err)
	assert.Contains(t, res, "Just text")

	// Test link with no text and no href
	res, err = ep.CleanHTML("<a></a>")
	assert.NoError(t, err)
	assert.Empty(t, res)

	// Test image with no alt text
	res, err = ep.CleanHTML("<img src='foo.png'>")
	assert.NoError(t, err)
	assert.Empty(t, res)

	// Test block-level spacing
	res, err = ep.CleanHTML("<p>Para 1</p><p>Para 2</p>")
	assert.NoError(t, err)
	assert.Contains(t, res, "Para 1")
	assert.Contains(t, res, "Para 2")

	res, err = ep.CleanHTML("&lt;tag&gt; &amp; &quot;quote&quot;")
	assert.NoError(t, err)
	assert.Equal(t, "<tag> & \"quote\"", res)

	// Test nested elements with specific text/block combinations
	nestedHTML := "<div><span>Text 1</span> <span>Text 2</span><p>Para</p></div>"
	res, err = ep.CleanHTML(nestedHTML)
	assert.NoError(t, err)
	assert.Contains(t, res, "Text 1 Text 2")
	assert.Contains(t, res, "Para")
}

func TestEmailProcessor_CleanHTML_BlockEdgeCases(t *testing.T) {
	logger := logrus.New()
	ep := NewEmailProcessor(logger)

	// Test multiple block elements
	html := "<div>Block 1</div><div>Block 2</div>"
	res, err := ep.CleanHTML(html)
	assert.NoError(t, err)
	assert.Contains(t, res, "Block 1")
	assert.Contains(t, res, "Block 2")

	// Test mixed text and block
	html = "Text before<div>Block</div>Text after"
	res, err = ep.CleanHTML(html)
	assert.NoError(t, err)
	assert.Contains(t, res, "Text before")
	assert.Contains(t, res, "Block")
	assert.Contains(t, res, "Text after")

	// Test nested blocks (to trigger nested isBlock logic)
	html = "<div>Outer<div>Inner</div></div>"
	res, err = ep.CleanHTML(html)
	assert.NoError(t, err)
	assert.Contains(t, res, "Outer")
	assert.Contains(t, res, "Inner")

	// Test table elements specifically (added to isBlock)
	html = "<table><tr><th>Header</th></tr><tr><td>Data</td></tr></table>"
	res, err = ep.CleanHTML(html)
	assert.NoError(t, err)
	assert.Contains(t, res, "Header")
	assert.Contains(t, res, "Data")
}

func TestEmailProcessor_CleanHTML_Malformed(t *testing.T) {
	logger := logrus.New()
	ep := NewEmailProcessor(logger)

	// Test malformed tags
	res, err := ep.CleanHTML("<div class='foo' unmatched>Content")
	assert.NoError(t, err)
	assert.Contains(t, res, "Content")

	// Test nested links (should handle gracefully)
	_, err = ep.CleanHTML("<a href='1'><a href='2'>Link</a></a>")
	assert.NoError(t, err)
}

func TestEmailProcessor_ProcessBody_ContextCancelled(t *testing.T) {
	logger := logrus.New()
	ep := NewEmailProcessor(logger)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	// Without initialization, it should still fail with "not initialized" error
	_, err := ep.ProcessBody(ctx, "<html></html>", "pdf")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not initialized")
}

func TestEmailProcessor_Initialize_Errors(t *testing.T) {
	logger := logrus.New()
	ep := NewEmailProcessor(logger)

	// Test non-existent chromium path
	err := ep.Initialize(context.Background(), "C:\\non-existent-path\\chrome.exe", 1)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "chromiumPath does not exist")

	// Test empty path (should return nil as per implementation)
	err = ep.Initialize(context.Background(), "", 1)
	assert.NoError(t, err)
}

func TestEmailProcessor_Initialize_DaemonURL(t *testing.T) {
	logger := logrus.New()
	ep := NewEmailProcessor(logger)

	// Test invalid websocket URL connection (should attempt WebSocket connection without file stat error)
	err := ep.Initialize(context.Background(), "ws://127.0.0.1:59999/devtools/browser/fake", 1)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to initialize browser instance")
}

func TestEmailProcessor_Close_Nil(t *testing.T) {
	logger := logrus.New()
	ep := NewEmailProcessor(logger)
	// Should not panic and return nil if nothing to close
	err := ep.Close()
	assert.NoError(t, err)
}

func TestEmailProcessor_ConvertToPDF_ContextCancelled(t *testing.T) {
	chromiumPath := findSystemChromiumPath()
	if chromiumPath == "" {
		t.Skip("Skipping PDF conversion test: No Chromium/Edge browser found on system")
	}

	logger := logrus.New()
	ep := NewEmailProcessor(logger)

	ctx, cancel := context.WithCancel(context.Background())
	err := ep.Initialize(ctx, chromiumPath, 1)
	require.NoError(t, err)
	defer func() {
		_ = ep.Close()
	}()

	cancel() // Cancel context before call
	_, err = ep.ConvertToPDF(ctx, "<html></html>")
	assert.Error(t, err)
	assert.Equal(t, context.Canceled, err)
}

func TestEmailProcessor_CleanHTML_NestedAndStyles(t *testing.T) {
	logger := logrus.New()
	ep := NewEmailProcessor(logger)

	// Test nested lists and blocks
	html := `<ul><li>Item 1<ul><li>Sub 1</li></ul></li><li>Item 2</li></ul>`
	res, err := ep.CleanHTML(html)
	assert.NoError(t, err)
	assert.Contains(t, res, "Item 1")
	assert.Contains(t, res, "Sub 1")
	assert.Contains(t, res, "Item 2")

	// Test script/style exclusion in different locations
	html = `<div><style>p {color: red;}</style><p>Visible</p><script>console.log(1);</script></div>`
	res, err = ep.CleanHTML(html)
	assert.NoError(t, err)
	assert.Equal(t, "Visible", res)
}

func TestEmailProcessor_ConvertToPDF_InvalidContent(t *testing.T) {
	chromiumPath := findSystemChromiumPath()
	if chromiumPath == "" {
		t.Skip("Skipping PDF conversion test: No Chromium/Edge browser found on system")
	}

	logger := logrus.New()
	ep := NewEmailProcessor(logger)
	ctx := context.Background()
	err := ep.Initialize(ctx, chromiumPath, 1)
	require.NoError(t, err)
	defer func() {
		_ = ep.Close()
	}()

	// rod handles invalid HTML gracefully by wrapping it, so we test something that might cause issues if any
	// but mostly we want to see if we can trigger any of the internal error checks
	res, err := ep.ConvertToPDF(ctx, "")
	assert.NoError(t, err) // Empty content is still valid for PDF generation in rod
	assert.NotNil(t, res)
}
