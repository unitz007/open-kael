package domain

import (
	"context"
	"errors"
)

// ErrPermanent is wrapped by a Listener's error return to signal that the
// failure is not transient — retrying will not help (e.g. invalid or revoked
// bot token, deleted resource). The runtime's restart loop checks for it with
// errors.Is and stops rather than backing off and retrying.
var ErrPermanent = errors.New("permanent")

// Listener is implemented by a provider's Executor when it can also receive
// inbound messages — the inbound half of ActionSendMessage's outbound half.
// Not every Executor is a messenger, and not every messenger can listen
// (e.g. a send-only relay), so this is a separate interface.
type Listener interface {
	// Listen blocks, delivering each inbound message to onMessage, until ctx
	// is cancelled or a transport error occurs. identity carries the bot/app
	// credential (bot token, webhook secret, etc.) for the specific registered
	// Identity to poll or subscribe to.
	//
	// Return fmt.Errorf("%w: ...", domain.ErrPermanent) for non-transient
	// failures (invalid credential, deleted bot) so the runtime stops retrying.
	Listen(ctx context.Context, identity *Identity, onMessage func(InboundMessage)) error
}
