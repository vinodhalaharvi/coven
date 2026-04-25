package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStatic(t *testing.T) {
	l := Static("hello")
	got, err := l(context.Background(), "anything")
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello" {
		t.Fatalf("got %q", got)
	}
}

func TestScripted(t *testing.T) {
	l := Scripted("a", "b")
	got, _ := l(context.Background(), "p1")
	if got != "a" {
		t.Fatalf("first = %q", got)
	}
	got, _ = l(context.Background(), "p2")
	if got != "b" {
		t.Fatalf("second = %q", got)
	}
	if _, err := l(context.Background(), "p3"); err == nil {
		t.Fatal("expected error after exhaustion")
	}
}

func TestExtractJSON_Plain(t *testing.T) {
	got := extractJSON(`{"a": 1, "b": "x"}`)
	if got != `{"a": 1, "b": "x"}` {
		t.Fatalf("got %q", got)
	}
}

func TestExtractJSON_WithPreamble(t *testing.T) {
	got := extractJSON(`Here is the JSON: {"a": 1}  thanks`)
	if got != `{"a": 1}` {
		t.Fatalf("got %q", got)
	}
}

func TestExtractJSON_CodeFence(t *testing.T) {
	got := extractJSON("```json\n{\"a\": 1}\n```")
	if got != `{"a": 1}` {
		t.Fatalf("got %q", got)
	}
}

func TestExtractJSON_Array(t *testing.T) {
	got := extractJSON(`Result: [1, 2, 3]`)
	if got != `[1, 2, 3]` {
		t.Fatalf("got %q", got)
	}
}

func TestExtractJSON_Nested(t *testing.T) {
	got := extractJSON(`{"a": {"b": [1, 2]}, "c": 3}`)
	if got != `{"a": {"b": [1, 2]}, "c": 3}` {
		t.Fatalf("got %q", got)
	}
}

func TestExtractJSON_StringWithBraces(t *testing.T) {
	// Ensure brace-counter respects string literals.
	got := extractJSON(`{"msg": "has } brace"}`)
	if got != `{"msg": "has } brace"}` {
		t.Fatalf("got %q", got)
	}
}

func TestExtractJSON_None(t *testing.T) {
	if got := extractJSON("no json here"); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

func TestStructured(t *testing.T) {
	type Out struct {
		Score int    `json:"score"`
		Label string `json:"label"`
	}
	raw := Static(`{"score": 7, "label": "good"}`)
	parse := Structured[Out](raw, `{score:int, label:str}`)
	got, err := parse(context.Background(), "rate this")
	if err != nil {
		t.Fatal(err)
	}
	if got.Score != 7 || got.Label != "good" {
		t.Fatalf("got %+v", got)
	}
}

func TestStructured_InvalidJSON(t *testing.T) {
	type Out struct{ X int `json:"x"` }
	raw := Static("this is not json at all")
	parse := Structured[Out](raw, `{x:int}`)
	_, err := parse(context.Background(), "")
	if err == nil {
		t.Fatal("expected error on non-JSON response")
	}
}

func TestClaude_HTTPRoundtrip(t *testing.T) {
	// Simulate the Anthropic API.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			w.WriteHeader(401)
			return
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		resp := map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "hello world"},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	l := Claude(ClaudeConfig{
		Model:    ClaudeHaiku,
		APIKey:   "test-key",
		Endpoint: srv.URL,
	})
	got, err := l(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello world" {
		t.Fatalf("got %q", got)
	}
}

func TestClaude_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"error": "rate limited"}`))
	}))
	defer srv.Close()

	l := Claude(ClaudeConfig{Model: "x", APIKey: "k", Endpoint: srv.URL})
	_, err := l(context.Background(), "hi")
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("want 429 error, got %v", err)
	}
}
