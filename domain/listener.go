package domain

import "context"

// Listener is implemented by a provider's Executor when it can also receive
// inbound messages — the inbound half of ActionSendMessage's outbound half.
// Not every Executor is a messenger, and not every messenger can listen
// (e.g. a send-only relay), so this is a separate interface.
type Listener interface {
	// Listen blocks, delivering each inbound message to onMessage, until ctx
	// is cancelled or a transport error occurs. identity carries the bot/app
	// credential (bot token, webhook secret, etc.) for the specific registered
	// Identity to poll or subscribe to.
	Listen(ctx context.Context, identity *Identity, onMessage func(InboundMessage)) error
}
