package domain

// RecentWindow is what a minion is given of a conversation before a new message: the latest exchange in full (the
// person's message, the look-ups it made and their results, and its reply, so "yes, do it" still has the ids it found),
// and before it only the words, what the person said and what the minion replied, until said messages of words are
// there. msgs is the conversation's latest stored messages, oldest first.
//
// Until 2026-10-30 the conversation was its last 8 stored messages, and look-up results and the platform's notes for one
// reply are stored messages: across the 8 conversations with a person on production, 7 of the 56 messages in their
// windows were the person's own, so a minion usually remembered less than one thing they had said, and a window could
// start on a look-up's result without its call. Notes from the platform (RoleSystem) are never given again: each was
// for the reply it was added to ("The person wants advice…").
func RecentWindow(msgs []Message, said int) []Message {
	// The latest exchange starts at the last message from the person.
	// With none (an exchange longer than what was read), it is all words.
	last := len(msgs)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == RoleUser {
			last = i
			break
		}
	}
	var latest []Message
	for _, m := range msgs[last:] {
		if m.Role != RoleSystem {
			latest = append(latest, m)
		}
	}
	n := 0
	for _, m := range latest {
		if isWords(m) {
			n++
		}
	}
	// Before it, words only, newest first until there are enough.
	var earlier []Message
	for i := last - 1; i >= 0 && n < said; i-- {
		m := msgs[i]
		if !isWords(m) {
			continue
		}
		earlier = append(earlier, Message{Role: m.Role, Content: m.Content})
		n++
	}
	out := make([]Message, 0, len(earlier)+len(latest))
	for i := len(earlier) - 1; i >= 0; i-- {
		out = append(out, earlier[i])
	}
	return append(out, latest...)
}

// isWords is a message of the conversation itself: something the person said, or the minion's reply in words.
func isWords(m Message) bool {
	switch m.Role {
	case RoleUser:
		return m.Content != ""
	case RoleAssistant:
		return m.Content != ""
	}
	return false
}
