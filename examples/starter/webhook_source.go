package main

import (
	"encoding/json"
	"net/http"
	"os"

	"github.com/unitz007/kael/webhook"
)

// exampleWebhookSource is a minimal webhook.Source — enough to show the
// shape (Path/Verify/Decode). A real sender (GitHub, Stripe, whatever) has
// its own payload shape and signature header name, but the verification
// itself is almost always this same HMAC-SHA256-over-the-raw-body pattern —
// see webhook.VerifyHMACSHA256's doc comment.
type exampleWebhookSource struct {
	secret []byte
}

func newExampleWebhookSource() webhook.Source {
	return &exampleWebhookSource{secret: []byte(os.Getenv("EXAMPLE_WEBHOOK_SECRET"))}
}

func (s *exampleWebhookSource) Path() string {
	return "/webhooks/example"
}

func (s *exampleWebhookSource) Verify(body []byte, header http.Header) bool {
	return webhook.VerifyHMACSHA256(s.secret, body, header.Get("X-Signature"))
}

// Decode expects a plain {"message": "..."} JSON body. An empty message
// still counts as "verified" but returns ok=false — the caller acks 200
// without running the workflow, same as a bot-loop guard or an event
// subtype a real Source doesn't react to would.
func (s *exampleWebhookSource) Decode(body []byte, header http.Header) (userTrigger string, ok bool, err error) {
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", false, err
	}
	if payload.Message == "" {
		return "", false, nil
	}
	return payload.Message, true, nil
}
