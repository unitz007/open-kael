// Package webhook is the webhook counterpart to messaging.Messenger. Where
// Messenger abstracts "how do I send/receive chat on this platform," Source
// abstracts "how do I verify and decode this platform's webhook payload."
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

// Source is what a webhook-triggered workflow's Trigger.Value holds. This
// module ships no implementations of it — GitHub, StockAI, or whatever else
// sends you a webhook each has its own payload shape and signature scheme,
// the same reasoning messaging.Messenger has no implementations here either.
type Source interface {
	// Path is the HTTP route this source registers on, e.g. "/webhooks/github".
	Path() string
	// Verify checks the raw body against the request's own signature
	// header(s). Called before Decode ever sees the body — different
	// senders sign differently (GitHub: X-Hub-Signature-256; a custom
	// sender: X-Signature; someone else: something else entirely), so this
	// isn't hardcoded to one convention.
	Verify(body []byte, header http.Header) bool
	// Decode turns an already-verified body into the trigger text handed
	// to the matched workflow. header is the original request's headers —
	// some senders (GitHub's X-GitHub-Event) distinguish event types by
	// header, not just body content, so Decode needs both. ok=false means
	// "verified, but nothing to do here" — a bot-loop guard, a trust
	// filter, an event subtype this source doesn't react to — and the
	// caller acks 200 without running anything. err != nil means the body
	// genuinely couldn't be parsed.
	Decode(body []byte, header http.Header) (userTrigger string, ok bool, err error)
}

// VerifyHMACSHA256 is the shared building block most Source implementations
// need — constant-time HMAC-SHA256 comparison of a hex-encoded signature,
// with or without a "sha256=" prefix. Exported so implementations don't
// each reinvent constant-time comparison, security-sensitive code that
// shouldn't be copy-pasted per integration.
func VerifyHMACSHA256(secret, body []byte, signatureHeader string) bool {
	if len(secret) == 0 || signatureHeader == "" {
		return false
	}
	signatureHeader = strings.TrimPrefix(signatureHeader, "sha256=")
	expected, err := hex.DecodeString(signatureHeader)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), expected)
}
