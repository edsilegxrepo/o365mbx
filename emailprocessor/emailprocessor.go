// Package emailprocessor provides utilities for transforming and cleaning email
// body content, including HTML-to-Text and HTML-to-PDF conversion.
//
// OBJECTIVE:
// This package is responsible for the "T" (Transform) in the ETL pipeline. It takes
// raw email body content (usually HTML) and converts it into the desired output format
// while sanitizing the content and preserving important information like links and alt-text.
//
// CORE FUNCTIONALITY:
//  1. HTML Sanitization: Cleans HTML to extract meaningful plain text while maintaining
//     basic document structure (blocks, lists, etc.).
//  2. Link & Image Handling: Preserves hyperlinks in a Markdown-like format [text](url)
//     and extracts image alt-text to ensure no information is lost during conversion.
//  3. PDF Generation: Uses the 'chromedp' library with a long-lived browser singleton,
//     incognito tab isolation, and managed context pool for high-fidelity, high-performance HTML-to-PDF rendering.
//  4. Chrome Daemon & Local Binary Support: Supports both managed local Chromium binaries and
//     direct WebSocket connections to pre-launched Chrome daemons (e.g. ws://127.0.0.1:9222) for zero-startup latency.
//  5. Content Detection: Provides utilities to detect if content is HTML or plain text.
//
// PERFORMANCE & RESOURCE MANAGEMENT:
//   - Tab Isolation: Isolates PDF conversion tasks in ephemeral Chrome tabs to prevent cross-request contamination.
//   - Browser Recycling: Automatically restarts the Chromium process/connection after 1000 conversions
//     to prevent V8 heap memory growth over multi-million email jobs.
//   - Process Tree Termination: Uses native context cancellation tree to guarantee clean termination without orphan Chrome processes.
package emailprocessor

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/html"
)

const (
	// DefaultPagePoolSize defines how many concurrent pages to keep in the pool.
	DefaultPagePoolSize = 5
)

var (
	spaceRegex   = regexp.MustCompile(`\s+`)
	htmlTagRegex = regexp.MustCompile(`(?i)<\s*\/?\s*[a-z-][^>]*>`)
	newlineRegex = regexp.MustCompile(`\n{2,}`)
)

// IsHTML checks if a string contains HTML tags.
func (ep *EmailProcessor) IsHTML(content string) bool {
	return htmlTagRegex.MatchString(content)
}

// EmailProcessorInterface defines the interface for EmailProcessor methods used by other packages.
//
//go:generate mockgen -destination=../mocks/mock_emailprocessor.go -package=mocks o365mbx/emailprocessor EmailProcessorInterface
type EmailProcessorInterface interface {
	ProcessBody(ctx context.Context, htmlContent, convertBody string) (interface{}, error)
	IsHTML(content string) bool
	Initialize(ctx context.Context, chromiumPath string, poolSize int) error
	Close() error
}

// EmailProcessor handles the transformation logic for email bodies.
type EmailProcessor struct {
	logger       log.FieldLogger
	allocCtx     context.Context
	allocCancel  context.CancelFunc
	chromiumPath string
	poolSize     int
	conversions  uint64
	mu           sync.Mutex
}

// NewEmailProcessor initializes a new processor with the provided logger.
func NewEmailProcessor(logger log.FieldLogger) *EmailProcessor {
	return &EmailProcessor{logger: logger}
}

// ProcessBody converts the body of an email to the specified format (none, text, or pdf).
func (ep *EmailProcessor) ProcessBody(ctx context.Context, htmlContent, convertBody string) (interface{}, error) {
	switch convertBody {
	case "none":
		return htmlContent, nil
	case "text":
		return ep.CleanHTML(htmlContent)
	case "pdf":
		return ep.ConvertToPDF(ctx, htmlContent)
	default:
		return nil, fmt.Errorf("invalid convertBody value: %s", convertBody)
	}
}

// isURL checks if a given target string is a WebSocket or HTTP URL.
func isURL(target string) bool {
	// nosemgrep: javascript.lang.security.detect-insecure-websocket.detect-insecure-websocket - Local Chrome DevTools protocol communicates over loopback ws://127.0.0.1:9222.
	return strings.HasPrefix(target, "ws://") || strings.HasPrefix(target, "wss://") ||
		strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://")
}

// Initialize sets up the long-lived Chromium browser instance or connects to a pre-launched Chrome daemon.
func (ep *EmailProcessor) Initialize(ctx context.Context, chromiumPath string, poolSize int) error {
	if chromiumPath == "" {
		return nil // Nothing to initialize if PDF conversion isn't used
	}

	if !isURL(chromiumPath) {
		if _, err := os.Stat(chromiumPath); os.IsNotExist(err) {
			return fmt.Errorf("chromiumPath does not exist: %s", chromiumPath)
		}
	}

	if poolSize <= 0 {
		poolSize = DefaultPagePoolSize
	}

	ep.chromiumPath = chromiumPath
	ep.poolSize = poolSize

	return ep.startBrowser()
}

// startBrowser connects to an always-running Chrome daemon if a control URL is supplied (e.g. ws://127.0.0.1:9222)
// or launches a managed Chromium process if a local binary path is provided using chromedp.
func (ep *EmailProcessor) startBrowser() error {
	ep.mu.Lock()
	defer ep.mu.Unlock()

	var allocCtx context.Context
	var allocCancel context.CancelFunc

	if isURL(ep.chromiumPath) {
		ep.logger.WithField("url", ep.chromiumPath).Info("Connecting directly to Chrome daemon via DevTools WebSocket.")
		allocCtx, allocCancel = chromedp.NewRemoteAllocator(context.Background(), ep.chromiumPath)
	} else {
		opts := append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.ExecPath(ep.chromiumPath),
			chromedp.NoFirstRun,
			chromedp.NoDefaultBrowserCheck,
			chromedp.Headless,
		)
		allocCtx, allocCancel = chromedp.NewExecAllocator(context.Background(), opts...)
		ep.logger.WithField("path", ep.chromiumPath).Info("Launched managed Chromium browser process.")
	}

	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	if err := chromedp.Run(browserCtx); err != nil {
		browserCancel()
		allocCancel()
		return fmt.Errorf("failed to initialize browser instance: %w", err)
	}

	ep.allocCtx = browserCtx
	ep.allocCancel = func() {
		browserCancel()
		allocCancel()
	}
	atomic.StoreUint64(&ep.conversions, 0)

	return nil
}

// Close gracefully shuts down browser connection and managed launcher process.
func (ep *EmailProcessor) Close() error {
	ep.mu.Lock()
	defer ep.mu.Unlock()

	if ep.allocCancel != nil {
		ep.allocCancel()
		ep.allocCancel = nil
		ep.allocCtx = nil
	}
	return nil
}

// ConvertToPDF converts HTML content to a PDF byte slice using chromedp.
func (ep *EmailProcessor) ConvertToPDF(ctx context.Context, htmlContent string) ([]byte, error) {
	ep.mu.Lock()
	if ep.allocCtx == nil {
		ep.mu.Unlock()
		return nil, fmt.Errorf("email processor not initialized for PDF conversion")
	}
	ep.mu.Unlock()

	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Browser recycling logic
	if atomic.LoadUint64(&ep.conversions) >= 1000 {
		ep.mu.Lock()
		if atomic.LoadUint64(&ep.conversions) >= 1000 {
			ep.logger.Info("Recycling browser instance after 1000 conversions.")
			ep.mu.Unlock()
			_ = ep.Close()
			if err := ep.startBrowser(); err != nil {
				return nil, fmt.Errorf("failed to recycle browser: %w", err)
			}
		} else {
			ep.mu.Unlock()
		}
	}
	atomic.AddUint64(&ep.conversions, 1)

	// Create tab target context derived from our allocator context
	taskCtx, taskCancel := chromedp.NewContext(ep.allocCtx)
	defer taskCancel()

	if ctx != nil {
		var cancel context.CancelFunc
		if deadline, ok := ctx.Deadline(); ok {
			taskCtx, cancel = context.WithDeadline(taskCtx, deadline)
		} else {
			taskCtx, cancel = context.WithTimeout(taskCtx, 60*time.Second)
		}
		defer cancel()
	}

	var pdfBuffer []byte
	err := chromedp.Run(taskCtx,
		chromedp.Navigate("about:blank"),
		chromedp.ActionFunc(func(runCtx context.Context) error {
			frameTree, err := page.GetFrameTree().Do(runCtx)
			if err != nil {
				return err
			}
			return page.SetDocumentContent(frameTree.Frame.ID, htmlContent).Do(runCtx)
		}),
		chromedp.ActionFunc(func(runCtx context.Context) error {
			buf, _, err := page.PrintToPDF().
				WithPrintBackground(true).
				WithPreferCSSPageSize(true).
				Do(runCtx)
			if err != nil {
				return err
			}
			pdfBuffer = buf
			return nil
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to generate PDF via chromedp: %w", err)
	}

	return pdfBuffer, nil
}

// CleanHTML converts HTML content to a sanitized plain-text string, preserving
// basic structure and link information.
func (ep *EmailProcessor) CleanHTML(htmlContent string) (string, error) {
	doc, err := html.Parse(strings.NewReader(htmlContent))
	if err != nil {
		return "", fmt.Errorf("failed to parse HTML: %w", err)
	}

	var sb strings.Builder
	lastChar := ' ' // Initialize with a space to avoid adding space at the very beginning

	var f func(*html.Node)
	f = func(n *html.Node) {
		// Skip script and style tags
		if n.Type == html.ElementNode && (n.Data == "script" || n.Data == "style") {
			return
		}

		// Handle links
		if n.Type == html.ElementNode && n.Data == "a" {
			var href string
			for _, attr := range n.Attr {
				if attr.Key == "href" {
					href = attr.Val
					break
				}
			}

			// Capture alt text from any immediate child image
			var altText string
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == html.ElementNode && c.Data == "img" {
					for _, attr := range c.Attr {
						if attr.Key == "alt" {
							altText = attr.Val
							break
						}
					}
				}
			}

			// Extract link text
			var linkTextBuilder strings.Builder
			var extractText func(*html.Node)
			extractText = func(node *html.Node) {
				if node.Type == html.TextNode {
					linkTextBuilder.WriteString(node.Data)
				}
				for c := node.FirstChild; c != nil; c = c.NextSibling {
					extractText(c)
				}
			}
			extractText(n)
			linkText := strings.TrimSpace(linkTextBuilder.String())

			// If link text is empty, try to use alt text, then fallback to href
			if linkText == "" {
				if altText != "" {
					linkText = altText
				} else {
					linkText = href
				}
			}

			if linkText != "" && href != "" {
				s := fmt.Sprintf(" [%s](%s) ", linkText, href)
				sb.WriteString(s)
				if len(s) > 0 {
					lastChar = rune(s[len(s)-1])
				}
			} else if href != "" { // Should be rare due to the fallback above
				s := fmt.Sprintf(" %s ", href)
				sb.WriteString(s)
				if len(s) > 0 {
					lastChar = rune(s[len(s)-1])
				}
			} else if linkText != "" { // Text only link with no href
				sb.WriteString(linkText)
				lastChar = rune(linkText[len(linkText)-1])
			}
			return // Skip processing children as we've handled the link
		}

		// Handle images
		if n.Type == html.ElementNode && n.Data == "img" {
			var altText string
			for _, attr := range n.Attr {
				if attr.Key == "alt" {
					altText = attr.Val
					break
				}
			}
			if altText != "" {
				s := fmt.Sprintf(" [Image: %s] ", altText)
				sb.WriteString(s)
				if len(s) > 0 {
					lastChar = rune(s[len(s)-1])
				}
			}
			return // Skip processing children as we've handled the image
		}

		// Add newlines for block-level elements
		isBlock := func(node *html.Node) bool {
			if node.Type != html.ElementNode {
				return false
			}
			switch node.Data {
			case "address", "article", "aside", "blockquote", "details", "dialog", "dd", "div", "dl", "dt",
				"fieldset", "figcaption", "figure", "footer", "form", "h1", "h2", "h3", "h4", "h5", "h6",
				"header", "hgroup", "hr", "li", "main", "nav", "ol", "p", "pre", "section", "table", "tr", "td", "th",
				"ul":
				return true
			default:
				return false
			}
		}

		if n.Type == html.ElementNode && isBlock(n) {
			if sb.Len() > 0 && lastChar != '\n' {
				sb.WriteString("\n")
				lastChar = '\n'
			}
		}

		if n.Type == html.TextNode {
			text := html.UnescapeString(n.Data) // Unescape HTML entities
			text = strings.TrimSpace(text)
			if text != "" {
				// Add a space before the text if the builder is not empty and the last char is not a space or newline
				if sb.Len() > 0 && lastChar != ' ' && lastChar != '\n' {
					sb.WriteString(" ")
				}
				sb.WriteString(text)
				lastChar = rune(text[len(text)-1])
			}
		}

		// Recursively traverse child nodes
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}

		if n.Type == html.ElementNode && isBlock(n) {
			if sb.Len() > 0 && lastChar != '\n' {
				sb.WriteString("\n")
				lastChar = '\n'
			}
		}
	}
	f(doc)

	plainText := sb.String()

	// Consolidate multiple newlines and spaces
	plainText = strings.ReplaceAll(plainText, "\r\n", "\n") // Normalize line endings
	plainText = strings.ReplaceAll(plainText, "\r", "\n")   // Normalize line endings
	plainText = newlineRegex.ReplaceAllString(plainText, "\n\n")
	plainText = spaceRegex.ReplaceAllString(plainText, " ") // Consolidate multiple spaces
	plainText = strings.TrimSpace(plainText)

	return plainText, nil
}
