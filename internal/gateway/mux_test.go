package gateway

import (
	"context"
	"testing"
)

// kinds counts how many recorded updates were callbacks vs text vs neither.
func kinds(us []Update) (callbacks, texts, others int) {
	for _, u := range us {
		switch {
		case u.CallbackQuery != nil:
			callbacks++
		case u.Message != nil:
			texts++
		default:
			others++
		}
	}
	return
}

// 6. Mux routes a callback_query update to the Callback handler and a text message to
// the Text handler; each only ever sees its own kind. (Reuses the existing
// recordingHandler from gateway_test.go, which records the full updates it saw.)
func TestMuxRoutesByKind(t *testing.T) {
	text := &recordingHandler{}
	cb := &recordingHandler{}
	mux := &Mux{Text: text, Callback: cb}

	tm := &Message{Text: "/status"}
	tm.Chat.ID = 1
	mux.Handle(context.Background(), Update{UpdateID: 1, Message: tm})
	mux.Handle(context.Background(), callbackUpdate(2, "bucks:approve:t1"))

	if c, txt, o := kinds(text.snapshot()); txt != 1 || c != 0 || o != 0 {
		t.Fatalf("text handler should have seen exactly 1 text and nothing else, got cb=%d text=%d other=%d", c, txt, o)
	}
	if c, txt, o := kinds(cb.snapshot()); c != 1 || txt != 0 || o != 0 {
		t.Fatalf("callback handler should have seen exactly 1 callback and nothing else, got cb=%d text=%d other=%d", c, txt, o)
	}
}

// Mux must tolerate nil sub-handlers (skip, no panic) and ignore an empty update.
func TestMuxToleratesNilHandlers(t *testing.T) {
	mux := &Mux{} // both nil
	// Must not panic on any kind of update.
	mux.Handle(context.Background(), callbackUpdate(1, "bucks:approve:t1"))
	tm := &Message{Text: "/status"}
	mux.Handle(context.Background(), Update{UpdateID: 2, Message: tm})
	mux.Handle(context.Background(), Update{UpdateID: 3}) // neither

	// With only a Text handler, a callback must be skipped (not routed to Text).
	text := &recordingHandler{}
	muxText := &Mux{Text: text}
	muxText.Handle(context.Background(), callbackUpdate(4, "bucks:deny:t1"))
	if c, txt, _ := kinds(text.snapshot()); c != 0 || txt != 0 {
		t.Fatalf("a callback with no Callback handler must be skipped, text handler saw cb=%d text=%d", c, txt)
	}
}
