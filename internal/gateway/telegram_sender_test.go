package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// recordedPost captures one inbound Telegram sendMessage call so the test can assert
// exactly what the sender posted (the chat id, the text, and any inline keyboard).
type recordedPost struct {
	path string
	body map[string]any
}

// sendMessageRecorder is an httptest handler that records every /sendMessage POST and
// replies with the ok=true envelope a real Telegram would. No network leaves the test.
type sendMessageRecorder struct {
	mu    sync.Mutex
	posts []recordedPost
}

func (r *sendMessageRecorder) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		r.mu.Lock()
		r.posts = append(r.posts, recordedPost{path: req.URL.Path, body: body})
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	})
}

func (r *sendMessageRecorder) last() recordedPost {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.posts) == 0 {
		return recordedPost{}
	}
	return r.posts[len(r.posts)-1]
}

// TestTelegramSenderSendPostsChatAndText proves Send posts the right chat_id + text to
// the /sendMessage method on the configured apiBase, honoring the injected client.
func TestTelegramSenderSendPostsChatAndText(t *testing.T) {
	rec := &sendMessageRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	s := NewTelegramSender(srv.URL, WithSenderHTTPClient(srv.Client()))

	if err := s.Send(context.Background(), 42, "hello operator"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := rec.last()
	if !strings.HasSuffix(got.path, "/sendMessage") {
		t.Errorf("posted to %q, want a /sendMessage path", got.path)
	}
	// chat_id is JSON-decoded as a float64; compare numerically.
	if cid, _ := got.body["chat_id"].(float64); int64(cid) != 42 {
		t.Errorf("chat_id = %v, want 42", got.body["chat_id"])
	}
	if got.body["text"] != "hello operator" {
		t.Errorf("text = %v, want %q", got.body["text"], "hello operator")
	}
	// A plain command reply must NOT carry an inline keyboard.
	if _, ok := got.body["reply_markup"]; ok {
		t.Error("plain Send must not include reply_markup")
	}
}

// TestTelegramSenderApprovalKeyboardCarriesTokens proves SendApprovalKeyboard posts an
// inline Approve/Deny keyboard whose two buttons carry the registry's exact callback_data
// tokens: "bucks:approve:<token>" and "bucks:deny:<token>".
func TestTelegramSenderApprovalKeyboardCarriesTokens(t *testing.T) {
	rec := &sendMessageRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	s := NewTelegramSender(srv.URL, WithSenderHTTPClient(srv.Client()))

	const token = "a7"
	if err := s.SendApprovalKeyboard(context.Background(), 99, "Approve this trade?", token); err != nil {
		t.Fatalf("SendApprovalKeyboard: %v", err)
	}

	got := rec.last()
	if !strings.HasSuffix(got.path, "/sendMessage") {
		t.Errorf("posted to %q, want a /sendMessage path", got.path)
	}
	if cid, _ := got.body["chat_id"].(float64); int64(cid) != 99 {
		t.Errorf("chat_id = %v, want 99", got.body["chat_id"])
	}
	if got.body["text"] != "Approve this trade?" {
		t.Errorf("text = %v", got.body["text"])
	}

	// The two callback_data tokens MUST match the format the ApprovalRegistry parses,
	// so a real tap routes back to the waiting request. We pull them out of the nested
	// reply_markup.inline_keyboard structure and assert both are present.
	datas := extractCallbackData(t, got.body)
	wantApprove := "bucks:approve:" + token
	wantDeny := "bucks:deny:" + token
	if !contains(datas, wantApprove) {
		t.Errorf("approve button callback_data missing %q; got %v", wantApprove, datas)
	}
	if !contains(datas, wantDeny) {
		t.Errorf("deny button callback_data missing %q; got %v", wantDeny, datas)
	}

	// And the tokens the registry would parse back out must round-trip cleanly.
	for _, d := range []string{wantApprove, wantDeny} {
		if _, tok, ok := parseCallbackData(d); !ok || tok != token {
			t.Errorf("callback_data %q does not parse back to token %q (ok=%v tok=%q)", d, token, ok, tok)
		}
	}
}

// TestTelegramSenderNon2xxIsError proves a non-2xx Telegram status surfaces as an error
// (so a failed post fails the approval SAFE rather than silently dropping it).
func TestTelegramSenderNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := NewTelegramSender(srv.URL, WithSenderHTTPClient(srv.Client()))
	if err := s.Send(context.Background(), 1, "x"); err == nil {
		t.Fatal("Send to a 500 must return an error")
	}
}

// TestTelegramSenderSendErrorIsTokenFree is the FINDING-1 regression test for the OUTBOUND
// half: when the sendMessage Do() fails, http.Client.Do wraps the failure in a *url.Error
// carrying the request URL (which embeds the bot token via apiBase). The error the sender
// RETURNS to its callers (commands.go reply log, approval.go returned error) must NOT
// contain the token. Before the pre-scrub fix this FAILS with the token visible. Covers
// both Send and SendApprovalKeyboard.
func TestTelegramSenderSendErrorIsTokenFree(t *testing.T) {
	client := &http.Client{Transport: failingRoundTripper{}}
	s := NewTelegramSender(fakeAPIBase, WithSenderHTTPClient(client))

	err := s.Send(context.Background(), 1, "hi")
	if err == nil {
		t.Fatal("Send against a failing transport must return an error")
	}
	if strings.Contains(err.Error(), fakeToken) {
		t.Fatalf("FINDING 1 LEAK: Send error contains the bot token: %v", err)
	}

	kerr := s.SendApprovalKeyboard(context.Background(), 1, "Approve?", "tok9")
	if kerr == nil {
		t.Fatal("SendApprovalKeyboard against a failing transport must return an error")
	}
	if strings.Contains(kerr.Error(), fakeToken) {
		t.Fatalf("FINDING 1 LEAK: SendApprovalKeyboard error contains the bot token: %v", kerr)
	}
}

// extractCallbackData digs the callback_data strings out of the posted
// reply_markup.inline_keyboard (a [][]button) regardless of row layout.
func extractCallbackData(t *testing.T, body map[string]any) []string {
	t.Helper()
	rm, ok := body["reply_markup"].(map[string]any)
	if !ok {
		t.Fatalf("no reply_markup object in body: %v", body)
	}
	rows, ok := rm["inline_keyboard"].([]any)
	if !ok {
		t.Fatalf("no inline_keyboard array in reply_markup: %v", rm)
	}
	var out []string
	for _, row := range rows {
		btns, ok := row.([]any)
		if !ok {
			continue
		}
		for _, b := range btns {
			btn, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if cd, ok := btn["callback_data"].(string); ok {
				out = append(out, cd)
			}
		}
	}
	return out
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
