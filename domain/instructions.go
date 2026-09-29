package domain

import "context"

// InstructionsPromptProvider is implemented by messenger executors that can
// prompt a user to enter or edit their personal agent instructions.
//
// triggerID is the Slack trigger_id from a block_actions interaction — only
// valid for ~3 seconds and used to open a modal. Pass empty string when
// called from a text command (/instructions) rather than a button tap.
//
// Returns waitForTextReply=true when the executor sent a text-based prompt
// (Telegram ForceReply, or Slack text fallback) and the host must track the
// next incoming message as the instructions reply. Returns false when a modal
// was opened and the result arrives via InstructionsSubmission instead.
type InstructionsPromptProvider interface {
	SendInstructionsPrompt(ctx context.Context, channelRef, current, triggerID string) (waitForTextReply bool, err error)
}
