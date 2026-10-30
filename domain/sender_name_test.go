package domain

import (
	"context"
	"testing"
)

func TestSenderName_CarriedOnTheContext(t *testing.T) {
	ctx := WithSenderName(context.Background(), "Ada", "Obi")
	if first, last := SenderNameFromContext(ctx); first != "Ada" || last != "Obi" {
		t.Errorf("%q %q", first, last)
	}
	if first, last := SenderNameFromContext(context.Background()); first != "" || last != "" {
		t.Errorf("none: %q %q", first, last)
	}
}
