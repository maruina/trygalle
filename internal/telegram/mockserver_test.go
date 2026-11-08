package telegram

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot"
)

// pollServer is the mock Bot API server for the poll loop: a scripted
// sequence of getUpdates batches plus a captured sendMessage log. Each
// batch is a list of one update's JSON; the server wraps the batch in the
// Bot API envelope. Once exhausted, getUpdates answers an empty result, so
// the loop keeps long-polling until the test cancels it.
// pollEntry is one scripted getUpdates call: either a batch of updates or
// one raw Bot API error body.
type pollEntry struct {
	batch []string
	err   string
}

type pollServer struct {
	entries []pollEntry
	// blocks, when set for an index, delays that getUpdates response until
	// the channel closes (or the request context ends), so a test can
	// observe the loop's state between two polls deterministically.
	blocks map[int]<-chan struct{}

	mu      sync.Mutex
	offsets []string
	sends   []map[string][]string

	server *httptest.Server
}

func newPollServer(t testing.TB, entries []pollEntry, blocks map[int]<-chan struct{}) *pollServer {
	t.Helper()
	s := &pollServer{entries: entries, blocks: blocks}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)
	return s
}

func (s *pollServer) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/getUpdates"):
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		i := len(s.offsets)
		s.offsets = append(s.offsets, r.FormValue("offset"))
		var body string
		if i < len(s.entries) {
			if s.entries[i].err != "" {
				body = s.entries[i].err
			} else {
				body = `{"ok":true,"result":[` + strings.Join(s.entries[i].batch, ",") + `]}`
			}
		} else {
			body = `{"ok":true,"result":[]}`
		}
		block := s.blocks[i]
		s.mu.Unlock()
		if block != nil {
			select {
			case <-block:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	case strings.HasSuffix(r.URL.Path, "/sendMessage"):
		s.recordSend(w, r)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (s *pollServer) recordSend(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	fields := map[string][]string{}
	for name, vals := range r.MultipartForm.Value {
		fields[name] = slices.Clone(vals)
	}
	s.mu.Lock()
	s.sends = append(s.sends, fields)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":7,"date":1,"chat":{"id":42,"type":"private"},"text":"sent"}}`))
}

// offsetsRecorded returns the requested offset form values in order.
func (s *pollServer) offsetsRecorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.offsets)
}

// sendCount counts captured sendMessage requests.
func (s *pollServer) sendCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sends)
}

// sendText returns the text field of the nth captured sendMessage.
func (s *pollServer) sendText(n int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sends[n]["text"][0]
}

// sendChatID returns the chat_id field of the nth captured sendMessage.
func (s *pollServer) sendChatID(n int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sends[n]["chat_id"][0]
}

// botErrorBody is one Bot API error body without the 429 parameters object.
func botErrorBody(code int, desc string) string {
	return fmt.Sprintf(`{"ok":false,"error_code":%d,"description":"%s"}`, code, desc)
}

// botFloodBody is the 429 shape the transport reads retry_after from.
func botFloodBody(retryAfter int) string {
	return fmt.Sprintf(`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":%d}}`, retryAfter)
}

// newBotAPI builds the reply-path sender against one server URL, matching
// how production New wires the library client's base URL.
func newBotAPI(t testing.TB, serverURL string) *bot.Bot {
	t.Helper()
	b, err := bot.New("1:test-token",
		bot.WithSkipGetMe(),
		bot.WithServerURL(serverURL),
	)
	if err != nil {
		t.Fatalf("bot.New: %v", err)
	}
	return b
}

// textUpdateJSON builds one text-message update as raw Bot API JSON.
func textUpdateJSON(id, from, chatID int64, chatType, text string) string {
	return messageJSON(id, from, chatID, chatType, map[string]any{"text": text})
}

// voiceUpdateJSON builds one voice-note update: media metadata only, no
// text field, so the classifier answers the canned reply.
func voiceUpdateJSON(id, from, chatID int64, chatType string) string {
	return messageJSON(id, from, chatID, chatType, map[string]any{
		"voice": map[string]any{"file_id": "voice-1", "duration": 1},
	})
}

// callbackUpdateJSON builds one non-message update kind with no chat shape.
func callbackUpdateJSON(id int64, from int64) string {
	body, _ := json.Marshal(map[string]any{
		"update_id": id,
		"callback_query": map[string]any{
			"id":   "cb-1",
			"from": map[string]any{"id": from},
			"data": " ignored",
		},
	})
	return string(body)
}

func messageJSON(id, from, chatID int64, chatType string, extra map[string]any) string {
	m := map[string]any{
		"message_id": id,
		"from":       map[string]any{"id": from},
		"chat":       map[string]any{"id": chatID, "type": chatType},
		"date":       1,
	}
	maps.Copy(m, extra)
	body, _ := json.Marshal(map[string]any{"update_id": id, "message": m})
	return string(body)
}

// validatedConfig is a valid test configuration with a fast poll timeout so
// canceled long polls return promptly.
func validatedConfig(t testing.TB) Config {
	t.Helper()
	cfg, err := (&Config{Token: "1:test-token", AllowedUser: 42, PollTimeout: 50 * time.Millisecond}).Validate()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
