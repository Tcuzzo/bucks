package channel

import "context"

// Approver is the seam that decouples the operator channel from HOW an above-band
// approval is actually obtained. It exists so there is ONE owner of Telegram
// getUpdates in the whole program: the always-on gateway long-poll loop. The channel
// asks for an approval through this seam; the gateway's approval registry implements
// it by posting an inline Approve/Deny keyboard and resolving the operator's tap from
// the SAME stream of updates the gateway already polls — the channel never polls
// getUpdates itself (the 409 tap-drop lesson: one agent, one bot, one poller).
//
// This interface lives in package channel (not gateway) so that gateway may import
// channel — which it already does for Decision/Report — while channel never imports
// gateway. That asymmetry is what keeps the two packages free of an import cycle: the
// abstraction (Approver, returning a channel.Decision) is defined where it is
// consumed, and the concrete registry in gateway satisfies it.
//
// FAIL-SAFE CONTRACT (unchanged): an implementation MUST return DecisionDenied on a
// timeout, a cancellation, or any transport/post error. A trade above the band is
// placed ONLY on an explicit DecisionApproved — silence is always a no.
type Approver interface {
	RequestApproval(ctx context.Context, r ApprovalRequest) (Decision, error)
}
