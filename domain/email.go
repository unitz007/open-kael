package domain

import "strings"

// NormalizeEmail is an email address as it is kept and compared: without surrounding spaces and in
// lower case. People type their address in whatever case they like ("Ada@Example.com"), and mail
// providers deliver it the same whichever they use, so it must be the same login either way.
func NormalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
