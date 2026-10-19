package domain

import "errors"

// Why a model call failed, in the two ways that matter to what happens next. A model client wraps its
// errors with these (errors.Is), so the loop, the host and the screens can tell "out of credit" from
// "had a hiccup" without reading error text.
var (
	// ErrLLMOutOfCredit means the provider refused because the account has no credit left. Trying
	// again cannot help until someone tops it up, so it is never retried; another provider may be.
	ErrLLMOutOfCredit = errors.New("the answering service is out of credit")
	// ErrLLMUnavailable means the provider could not answer right now (overloaded, down, rate-limited,
	// or its key was refused): worth trying again, or another provider.
	ErrLLMUnavailable = errors.New("the answering service is unavailable")
)

// LLMDown reports whether err is the model service not being able to answer, for either reason.
// What was asked of it was fine; nothing about the person's request is wrong.
func LLMDown(err error) bool {
	return errors.Is(err, ErrLLMOutOfCredit) || errors.Is(err, ErrLLMUnavailable)
}
