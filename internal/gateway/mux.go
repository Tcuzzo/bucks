package gateway

import "context"

// mux.go — the Mux fans the single gateway's update stream out to the two handlers
// that own different kinds of update: the CommandRouter (text commands) and the
// ApprovalRegistry (inline-button taps). It is the join that lets ONE poller feed
// BOTH surfaces without either one running its own getUpdates loop.
//
// Routing is by update kind, mirroring the contract each sub-handler already asserts:
// a callback_query (an inline-button tap) goes ONLY to the Callback handler; a text
// message goes ONLY to the Text handler. This is the structural guarantee that the
// command router never steals an approval tap and the approval registry never sees a
// command — each sees exactly its own kind.

// Mux is a Handler that dispatches each update to the sub-handler that owns its kind.
// Either sub-handler may be nil; a nil handler for an arriving kind means that kind is
// simply skipped (no panic), which is convenient before both surfaces are wired.
type Mux struct {
	// Text handles text messages (e.g. the CommandRouter).
	Text Handler
	// Callback handles inline-button taps (e.g. the ApprovalRegistry).
	Callback Handler
}

// Handle routes one update. A callback_query goes to Callback; otherwise a message
// goes to Text. An update that is neither (or whose owning handler is nil) is ignored.
// The callback_query check comes FIRST because that is the discriminating field — a
// real Telegram update sets exactly one of the two — so order is unambiguous.
func (m *Mux) Handle(ctx context.Context, u Update) {
	switch {
	case u.CallbackQuery != nil:
		if m.Callback != nil {
			m.Callback.Handle(ctx, u)
		}
	case u.Message != nil:
		if m.Text != nil {
			m.Text.Handle(ctx, u)
		}
	}
}

// compile-time assertion that Mux is a Handler.
var _ Handler = (*Mux)(nil)
