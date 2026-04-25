// Package llm provides a function-typed Claude API client and a generic
// Structured[T] helper that turns a raw LLM call into a typed one.
//
// We deliberately avoid interfaces: an LLM is a function value, and typed
// LLMs are typed function values. This preserves types end-to-end.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"time"
)

// LLM is the core function type. Any backend (Claude, Ollama, a fake, a
// replay recorder) is just a value of this type.
type LLM = func(ctx context.Context, prompt string) (string, error)

// Model presets — kept here so magic strings don't leak into agent code.
const (
	ClaudeOpus   = "claude-opus-4-7"
	ClaudeSonnet = "claude-sonnet-4-6"
	ClaudeHaiku  = "claude-haiku-4-5-20251001"
)

// ClaudeConfig configures the Claude backend.
type ClaudeConfig struct {
	Model     string
	APIKey    string        // if empty, read from ANTHROPIC_API_KEY
	MaxTokens int           // default 2048
	Timeout   time.Duration // default 60s
	Endpoint  string        // for testing; defaults to api.anthropic.com
	HTTP      *http.Client  // for testing
}

// Claude returns an LLM backed by the Anthropic API.
func Claude(cfg ClaudeConfig) LLM {
	if cfg.APIKey == "" {
		cfg.APIKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	if cfg.MaxTokens == 0 {
		cfg.MaxTokens = 2048
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = "https://api.anthropic.com/v1/messages"
	}
	client := cfg.HTTP
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}

	return func(ctx context.Context, prompt string) (string, error) {
		body, err := json.Marshal(map[string]any{
			"model":      cfg.Model,
			"max_tokens": cfg.MaxTokens,
			"messages": []map[string]string{
				{"role": "user", "content": prompt},
			},
		})
		if err != nil {
			return "", fmt.Errorf("claude: marshal request: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, "POST", cfg.Endpoint, bytes.NewReader(body))
		if err != nil {
			return "", fmt.Errorf("claude: new request: %w", err)
		}
		req.Header.Set("x-api-key", cfg.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("content-type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("claude: http: %w", err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", fmt.Errorf("claude: read body: %w", err)
		}
		if resp.StatusCode >= 400 {
			return "", fmt.Errorf("claude: status %d: %s", resp.StatusCode, string(raw))
		}

		var out struct {
			Content []struct {
				Text string `json:"text"`
				Type string `json:"type"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return "", fmt.Errorf("claude: decode: %w", err)
		}
		for _, c := range out.Content {
			if c.Type == "text" || c.Type == "" {
				return c.Text, nil
			}
		}
		return "", fmt.Errorf("claude: no text content in response")
	}
}

// Static returns an LLM that always returns the given string. Useful for
// tests and for wiring up agents without burning API credits.
func Static(response string) LLM {
	return func(ctx context.Context, prompt string) (string, error) {
		return response, nil
	}
}

// Scripted returns an LLM that returns responses[i] for the i-th call, and
// an error after exhaustion. Useful for deterministic tests.
func Scripted(responses ...string) LLM {
	var i int
	return func(ctx context.Context, prompt string) (string, error) {
		if i >= len(responses) {
			return "", fmt.Errorf("scripted llm: exhausted after %d calls", i)
		}
		r := responses[i]
		i++
		return r, nil
	}
}

// Structured wraps a raw LLM into one that returns T by asking for JSON and
// decoding it. The schema hint is appended to the prompt.
func Structured[T any](raw LLM, schemaHint string) func(context.Context, string) (T, error) {
	return func(ctx context.Context, prompt string) (T, error) {
		var zero T
		full := prompt + "\n\nRespond ONLY as JSON matching this schema: " + schemaHint +
			"\nDo not include prose, code fences, or explanations."
		resp, err := raw(ctx, full)
		if err != nil {
			return zero, err
		}
		js := extractJSON(resp)
		if js == "" {
			return zero, fmt.Errorf("structured: no JSON found in response: %s", truncate(resp, 200))
		}
		var out T
		if err := json.Unmarshal([]byte(js), &out); err != nil {
			return zero, fmt.Errorf("structured: decode: %w; raw=%s", err, truncate(js, 200))
		}
		return out, nil
	}
}

// extractJSON pulls the first balanced JSON object or array out of a string,
// tolerating ```json fences and leading/trailing prose.
func extractJSON(s string) string {
	// Strip code fences.
	s = codeFenceRE.ReplaceAllString(s, "$1")

	// Find the first '{' or '[' and extract a balanced span.
	start := -1
	var open, close byte
	for i := 0; i < len(s); i++ {
		if s[i] == '{' {
			start = i
			open, close = '{', '}'
			break
		}
		if s[i] == '[' {
			start = i
			open, close = '[', ']'
			break
		}
	}
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if esc {
			esc = false
			continue
		}
		if inStr {
			switch c {
			case '\\':
				esc = true
			case '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

var codeFenceRE = regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)```")

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
