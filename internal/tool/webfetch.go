package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	maxFetchBytes          = 5 * 1024 * 1024
	defaultFetchTimeout    = 30 * time.Second
	maxFetchTimeoutSeconds = 120
	fetchRetryDelay        = 200 * time.Millisecond
)

// WebFetch downloads an HTTP or HTTPS resource for the model.
type WebFetch struct {
	Client *http.Client
}

func (w *WebFetch) Name() string { return "webfetch" }

func (w *WebFetch) Description() string {
	return "Fetch content from an HTTP or HTTPS URL and return it as text, markdown, or HTML. Markdown is the default.\n\nUse a more targeted tool when one is available. This tool is read-only. Large text results may be replaced with a preview while the complete output is retained in managed storage."
}

func (w *WebFetch) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"url": {"type": "string", "description": "The HTTP or HTTPS URL to fetch content from"},
			"format": {"type": "string", "enum": ["text", "markdown", "html"], "description": "The format to return the content in. Defaults to markdown."},
			"timeout": {"type": "integer", "description": "Optional timeout in seconds (maximum: 120)"}
		},
		"required": ["url"]
	}`)
}

func (w *WebFetch) Run(ctx context.Context, call Call) (string, error) {
	var args struct {
		URL     string `json:"url"`
		Format  string `json:"format"`
		Timeout int    `json:"timeout"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("webfetch: %w", err)
	}
	url := strings.TrimSpace(args.URL)
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return "", fmt.Errorf("webfetch: URL must start with http:// or https://")
	}
	if args.Format == "" {
		args.Format = "markdown"
	}
	switch args.Format {
	case "text", "markdown", "html":
	default:
		return "", fmt.Errorf("webfetch: format must be text, markdown or html")
	}
	timeout := defaultFetchTimeout
	if args.Timeout > 0 {
		if args.Timeout > maxFetchTimeoutSeconds {
			return "", fmt.Errorf("webfetch: timeout must be at most %d seconds", maxFetchTimeoutSeconds)
		}
		timeout = time.Duration(args.Timeout) * time.Second
	}
	if err := call.Allow(ctx, "webfetch", []string{url}, ""); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := w.Client
	if client == nil {
		client = http.DefaultClient
	}
	var resp *http.Response
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return "", fmt.Errorf("webfetch: %w", err)
		}
		req.Header.Set("User-Agent", "lingzhu-agent/0.1 (+https://github.com/miebh/lingzhu)")
		req.Header.Set("Accept", "text/markdown, text/html, text/plain;q=0.9, */*;q=0.1")

		resp, err = client.Do(req)
		if err != nil {
			if attempt == 0 && ctx.Err() == nil && retryableFetchError(err) {
				call.SetMetadata("retried", true)
				if !waitFetchRetry(ctx) {
					return "", fmt.Errorf("webfetch: %w", err)
				}
				continue
			}
			return "", fmt.Errorf("webfetch: %w", err)
		}
		// A server error is often transient. Retry it once; a client error is
		// the request's own fault and would fail again unchanged.
		if resp.StatusCode >= 500 && resp.StatusCode <= 599 && attempt == 0 {
			_ = resp.Body.Close()
			call.SetMetadata("retried", true)
			if !waitFetchRetry(ctx) {
				return "", fmt.Errorf("webfetch: %s returned %s", url, resp.Status)
			}
			continue
		}
		break
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("webfetch: %s returned %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes+1))
	if err != nil {
		return "", fmt.Errorf("webfetch: %w", err)
	}
	truncated := len(body) > maxFetchBytes
	if truncated {
		body = body[:maxFetchBytes]
	}
	text := string(body)
	contentType := resp.Header.Get("Content-Type")

	switch args.Format {
	case "html":
		// Keep the source as returned.
	case "text":
		if isHTML(contentType, text) {
			text = htmlToText(text)
		}
	default:
		if isHTML(contentType, text) {
			text = htmlToText(text)
		}
	}
	text = strings.TrimSpace(text)
	if truncated {
		text += "\n\n(result truncated)"
	}
	return text, nil
}

// retryableFetchError reports transport failures that a second attempt can fix.
// A caller deadline or a TLS/certificate failure is not retryable.
func retryableFetchError(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return errors.Is(urlErr.Err, io.EOF) || errors.Is(urlErr.Err, io.ErrUnexpectedEOF)
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// waitFetchRetry waits before the single retry without outliving the call.
func waitFetchRetry(ctx context.Context) bool {
	timer := time.NewTimer(fetchRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func isHTML(contentType, body string) bool {
	if strings.Contains(strings.ToLower(contentType), "html") {
		return true
	}
	head := strings.ToLower(strings.TrimSpace(body))
	return strings.HasPrefix(head, "<!doctype html") || strings.HasPrefix(head, "<html")
}

var (
	scriptStyle = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script>|<style\b[^>]*>.*?</style>|<noscript\b[^>]*>.*?</noscript>`)
	blockTags   = regexp.MustCompile(`(?is)</?(p|div|section|article|header|footer|li|ul|ol|tr|table|h[1-6]|blockquote|pre)\b[^>]*>`)
	brTags      = regexp.MustCompile(`(?is)<br\s*/?>`)
	anchorTags  = regexp.MustCompile(`(?is)<a\b[^>]*href=["']([^"']+)["'][^>]*>(.*?)</a>`)
	anyTag      = regexp.MustCompile(`(?s)<[^>]+>`)
	blankLines  = regexp.MustCompile(`\n{3,}`)
	spaces      = regexp.MustCompile(`[ \t]{2,}`)
)

// htmlToText is a compact HTML-to-readable-text pass. It keeps link text and
// drops tags, scripts and styles without pulling in a full HTML parser.
func htmlToText(source string) string {
	text := scriptStyle.ReplaceAllString(source, " ")
	text = anchorTags.ReplaceAllStringFunc(text, func(match string) string {
		parts := anchorTags.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}
		label := strings.TrimSpace(anyTag.ReplaceAllString(parts[2], ""))
		href := strings.TrimSpace(parts[1])
		if label == "" {
			return href
		}
		if href == "" || strings.HasPrefix(href, "#") {
			return label
		}
		return label + " (" + href + ")"
	})
	text = brTags.ReplaceAllString(text, "\n")
	text = blockTags.ReplaceAllString(text, "\n")
	text = anyTag.ReplaceAllString(text, "")
	text = html.UnescapeString(text)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = spaces.ReplaceAllString(text, " ")
	text = blankLines.ReplaceAllString(text, "\n\n")
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		lines = append(lines, strings.TrimSpace(line))
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
