package domain

// Earlier is what was said just before a person's latest message: their own previous
// message and the agent's reply to it. It lets a short follow-up ("pull up the full
// context", "do it", "and gold?") be understood next to what it follows.
type Earlier struct {
	User      string // the person's previous message ("" if none)
	Assistant string // the agent's reply to it, in full ("" if none)
}

// IsZero reports whether there is nothing earlier to show.
func (e Earlier) IsZero() bool { return e.User == "" && e.Assistant == "" }
