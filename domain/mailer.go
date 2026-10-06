package domain

import "context"

// Mailer sends transactional emails on behalf of the platform.
type Mailer interface {
	Send(ctx context.Context, to, subject, html string) error
}
