package harness

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Constants for the synthetic model registered through the generated
// models.json. The provider key is "mock"; Pi 1.0.1 accepts it and lists the
// model under --provider mock (verified live; the ollama fallback was not
// needed).
const (
	mockProvider    = "mock"
	mockModelID     = "mock-model"
	mockAPIKey      = "mock"
	mockAPI         = "openai-completions"
	mockRequestPath = "/v1/chat/completions" // Pi requests {baseUrl}/chat/completions
)

// mockBehavior selects the scripted response for one MockModel server.
type mockBehavior int

const (
	behaveText mockBehavior = iota
	behaveEmpty
	behaveFailOnce
	behaveFailAlways
)

// RequestCapture is metadata about one model request. It records the path,
// which header and body field names were present, and the stream flag — never
// the request content or credential values, so captures are public-safe.
type RequestCapture struct {
	Path        string
	HeaderNames []string
	BodyFields  []string
	Stream      bool
}

// MockModel is an OpenAI-compatible chat-completions server for live tests.
// Pi routes the mock provider to it through a generated models.json. The
// server records request metadata for wire-shape assertions and answers with
// one scripted behavior (SSE streaming, matching what Pi actually sends).
type MockModel struct {
	ts *httptest.Server

	mu       sync.Mutex
	behavior mockBehavior
	text     string
	delay    time.Duration
	requests []RequestCapture
}

// StartMockModel starts a mock model server with the default scripted text
// and registers its cleanup on t.
func StartMockModel(t *testing.T) *MockModel {
	t.Helper()
	m := &MockModel{text: "hello from the mock model"}
	m.ts = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.ts.Close)
	return m
}

// URL returns the mock server origin.
func (m *MockModel) URL() string { return m.ts.URL }

// RespondText scripts the reply text for every request.
func (m *MockModel) RespondText(text string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.text = text
}

// RespondEmptyText scripts an assistant reply with no text at all.
func (m *MockModel) RespondEmptyText() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.behavior = behaveEmpty
}

// FailOnce scripts one HTTP 500 followed by the default text.
func (m *MockModel) FailOnce() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.behavior = behaveFailOnce
}

// FailAlways scripts an HTTP 500 for every request.
func (m *MockModel) FailAlways() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.behavior = behaveFailAlways
}

// Delay holds every response open for d before answering.
func (m *MockModel) Delay(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.delay = d
}

// Requests returns captured request metadata in arrival order.
func (m *MockModel) Requests() []RequestCapture {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RequestCapture, len(m.requests))
	copy(out, m.requests)
	return out
}

func (m *MockModel) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	m.capture(r, body)
	m.mu.Lock()
	behavior, text, delay := m.behavior, m.text, m.delay
	m.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}

	if behavior == behaveFailAlways || behavior == behaveFailOnce && m.firstRequest() {
		writeFailure(w)
		return
	}
	if behavior == behaveEmpty {
		text = ""
	}
	var req struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &req)
	writeCompletion(w, req.Stream, text)
}

// firstRequest reports whether this is the first request the server has seen.
func (m *MockModel) firstRequest() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests) == 1
}

func (m *MockModel) capture(r *http.Request, body []byte) {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(body, &fields)
	var flag struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &flag)

	c := RequestCapture{
		Path:   r.URL.Path,
		Stream: flag.Stream,
	}
	for name := range r.Header {
		c.HeaderNames = append(c.HeaderNames, strings.ToLower(name))
	}
	for name := range fields {
		c.BodyFields = append(c.BodyFields, name)
	}
	sort.Strings(c.HeaderNames)
	sort.Strings(c.BodyFields)

	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, c)
}

// writeCompletion answers one chat-completions request. Pi always streams
// (stream: true), so the response is SSE; the plain JSON path exists for
// non-streaming producers.
func writeCompletion(w http.ResponseWriter, stream bool, text string) {
	if !stream {
		writePlainCompletion(w, text)
		return
	}
	writeStreamingCompletion(w, text)
}

func writeStreamingCompletion(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	f, _ := w.(http.Flusher)
	chunk := func(delta map[string]any, finish any) {
		event := map[string]any{
			"id":      "cmpl-mock",
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   mockModelID,
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			}},
		}
		b, _ := json.Marshal(event)
		_, _ = w.Write(append(append([]byte("data: "), b...), '\n', '\n'))
		f.Flush()
	}
	chunk(map[string]any{"role": "assistant", "content": ""}, nil)
	chunk(map[string]any{"content": text}, nil)
	chunk(map[string]any{}, "stop")
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	f.Flush()
}

func writePlainCompletion(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "application/json")
	completion := map[string]any{
		"id":      "cmpl-mock",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   mockModelID,
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": text,
			},
			"finish_reason": "stop",
		}},
	}
	_ = json.NewEncoder(w).Encode(completion)
}

func writeFailure(w http.ResponseWriter) {
	// The status matters, not the exact body; Pi reports stopReason "error"
	// with the body JSON as the error message.
	w.WriteHeader(http.StatusInternalServerError)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": "mock provider error",
			"type":    "server_error",
		},
	})
}
