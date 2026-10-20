package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/maruina/trygalle/internal/rpc"
)

// postChat sends one chat-completions request to the mock model and returns
// the raw response body.
func postChat(t *testing.T, url, body string) []byte {
	t.Helper()
	resp, err := http.Post(url+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post chat/completions: %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return got
}

func decodeCompletion(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode completion %q: %v", body, err)
	}
	return v
}

// sseFrames splits an SSE body into its JSON frames, skipping the [DONE] line.
func sseFrames(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var frames []map[string]any
	for line := range bytes.SplitSeq(body, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload := bytes.TrimPrefix(line, []byte("data: "))
		if string(payload) == "[DONE]" {
			continue
		}
		frames = append(frames, decodeCompletion(t, payload))
	}
	if len(frames) == 0 {
		t.Fatalf("no SSE frames in %q", body)
	}
	return frames
}

func TestMockModelStreamingAndPlain(t *testing.T) {
	m := StartMockModel(t)

	// Pi always streams; the SSE path is the contract path.
	streaming := postChat(t, m.URL(), `{"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	frames := sseFrames(t, streaming)
	last := frames[len(frames)-1]
	choices := last["choices"].([]any)
	if choices[0].(map[string]any)["finish_reason"] != "stop" {
		t.Errorf("final frame finish_reason = %v, want stop", last)
	}
	if !bytes.Contains(streaming, []byte("data: [DONE]")) {
		t.Errorf("streaming response lacks the [DONE] frame: %s", streaming)
	}

	// The plain JSON path is exercised for non-streaming producers.
	plain := postChat(t, m.URL(), `{"messages":[{"role":"user","content":"hi"}],"stream":false}`)
	v := decodeCompletion(t, plain)
	message := v["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "hello from the mock model" {
		t.Errorf("plain content = %v, want the default scripted text", message["content"])
	}
}

// streamTextContent extracts the cumulative assistant content from an SSE
// stream, mirroring how Pi reconstructs the message.
func streamTextContent(frames []map[string]any) string {
	var text strings.Builder
	for _, f := range frames {
		delta := f["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		if content, ok := delta["content"].(string); ok {
			text.WriteString(content)
		}
	}
	return text.String()
}

func TestMockModelScriptedBehaviors(t *testing.T) {
	t.Run("empty text", func(t *testing.T) {
		m := StartMockModel(t)
		m.RespondEmptyText()
		frames := sseFrames(t, postChat(t, m.URL(), `{"messages":[],"stream":true}`))
		if got := streamTextContent(frames); got != "" {
			t.Errorf("empty behavior content = %q, want empty string", got)
		}
	})

	t.Run("fail once then succeed", func(t *testing.T) {
		m := StartMockModel(t)
		m.FailOnce()
		resp, err := http.Post(m.URL()+"/v1/chat/completions", "application/json", strings.NewReader(`{"messages":[],"stream":false}`))
		if err != nil {
			t.Fatalf("first post: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("first status = %d, want 500", resp.StatusCode)
		}
		if got := postChat(t, m.URL(), `{"messages":[],"stream":false}`); !bytes.Contains(got, []byte("mock model")) {
			t.Errorf("second response = %s, want the scripted text", got)
		}
	})

	t.Run("fail always", func(t *testing.T) {
		m := StartMockModel(t)
		m.FailAlways()
		for i := range 2 {
			resp, err := http.Post(m.URL()+"/v1/chat/completions", "application/json", strings.NewReader(`{"messages":[],"stream":false}`))
			if err != nil {
				t.Fatalf("post %d: %v", i, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusInternalServerError {
				t.Errorf("attempt %d status = %d, want 500", i, resp.StatusCode)
			}
		}
	})
}

func TestMockModelDelay(t *testing.T) {
	m := StartMockModel(t)
	m.Delay(150 * time.Millisecond)
	start := time.Now()
	postChat(t, m.URL(), `{"messages":[],"stream":false}`)
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Errorf("delay elapsed = %v, want >= 150ms", elapsed)
	}
}

func TestMockModelCapturesMetadataOnly(t *testing.T) {
	m := StartMockModel(t)
	postChat(t, m.URL(), `{"messages":[{"role":"user","content":"supersecret prompt"}],"stream":true,"model":"mock-model"}`)

	captures := m.Requests()
	if len(captures) != 1 {
		t.Fatalf("captured %d requests, want 1", len(captures))
	}
	c := captures[0]
	if c.Path != "/v1/chat/completions" {
		t.Errorf("path = %q, want %q", c.Path, mockRequestPath)
	}
	if !c.Stream {
		t.Error("stream = false, want true")
	}
	// The Go test client sends no credentials; the live Pi request asserts
	// the authorization header instead.
	if !contains(c.HeaderNames, "content-type") {
		t.Errorf("headers %v lack content-type", c.HeaderNames)
	}
	for _, want := range []string{"messages", "model", "stream"} {
		if !contains(c.BodyFields, want) {
			t.Errorf("body fields %v lack %q", c.BodyFields, want)
		}
	}
	// The capture must never contain content or credential values.
	joined := strings.Join(c.HeaderNames, " ") + " " + strings.Join(c.BodyFields, " ")
	if strings.Contains(joined, "supersecret") || strings.Contains(joined, "Bearer") {
		t.Errorf("capture leaked content or credentials: %q", joined)
	}
}

func contains(list []string, want string) bool {
	return slices.Contains(list, want)
}

func TestWriteModelsJSON(t *testing.T) {
	dir := t.TempDir()
	writeModelsJSON(t, dir, "http://127.0.0.1:1234")
	var got map[string]any
	b, err := os.ReadFile(filepath.Join(dir, "models.json"))
	if err != nil {
		t.Fatalf("read models.json: %v", err)
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("decode models.json: %v", err)
	}
	providers := got["providers"].(map[string]any)
	provider, ok := providers["mock"].(map[string]any)
	if !ok {
		t.Fatalf("models.json lacks the mock provider: %v", got)
	}
	if provider["api"] != "openai-completions" {
		t.Errorf("api = %v, want openai-completions", provider["api"])
	}
	if provider["apiKey"] != "mock" {
		t.Errorf("apiKey = %v, want mock", provider["apiKey"])
	}
	if provider["baseUrl"] != "http://127.0.0.1:1234/v1" {
		t.Errorf("baseUrl = %v, want the mock origin with /v1", provider["baseUrl"])
	}
	models := provider["models"].([]any)
	if models[0].(map[string]any)["id"] != "mock-model" {
		t.Errorf("model id = %v, want mock-model", models[0])
	}
}

// TestMockModelLiveWireShape proves the harness wire contract against real Pi:
// a scripted prompt round-trip completes and Pi's request has the pinned shape.
func TestMockModelLiveWireShape(t *testing.T) {
	bin := Pi(t)
	model := StartMockModel(t)
	client := Start(t, bin, WithModel(model))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, err := client.Send(ctx, rpc.PromptCommand("wire shape probe", "steer"))
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if !resp.Success {
		t.Fatalf("prompt failed: %s", resp.Error)
	}
	var data rpc.PromptData
	if err := resp.Decode(&data); err != nil {
		t.Fatalf("decode prompt data: %v", err)
	}
	if data.Disposition != rpc.DispositionStarted {
		t.Fatalf("disposition = %q, want started", data.Disposition)
	}
	for {
		select {
		case event := <-client.Events():
			if event.Type == rpc.EventAgentSettled {
				goto settled
			}
		case <-ctx.Done():
			t.Fatal("timeout waiting for agent_settled")
		}
	}
settled:

	captures := model.Requests()
	if len(captures) == 0 {
		t.Fatal("mock model captured no requests")
	}
	c := captures[0]
	if c.Path != mockRequestPath {
		t.Errorf("request path = %q, want %q", c.Path, mockRequestPath)
	}
	if !c.Stream {
		t.Error("request stream = false, want true (Pi always streams)")
	}
	for _, want := range []string{"authorization", "content-type"} {
		if !contains(c.HeaderNames, want) {
			t.Errorf("request headers %v lack %q", c.HeaderNames, want)
		}
	}
	for _, want := range []string{"messages", "model", "stream"} {
		if !contains(c.BodyFields, want) {
			t.Errorf("request body fields %v lack %q", c.BodyFields, want)
		}
	}

	// The scripted round-trip completes: the last assistant text is the
	// default mock reply.
	resp, err = client.Send(ctx, rpc.GetLastAssistantTextCommand())
	if err != nil {
		t.Fatalf("get_last_assistant_text: %v", err)
	}
	var text rpc.LastAssistantTextData
	if err := resp.Decode(&text); err != nil {
		t.Fatalf("decode last assistant text: %v", err)
	}
	if text.Text == nil || *text.Text != "hello from the mock model" {
		t.Errorf("last assistant text = %v, want the default mock reply", text.Text)
	}
}
