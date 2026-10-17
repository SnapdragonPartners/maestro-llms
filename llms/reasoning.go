package llms

// ReasoningFrom returns msgs with every ContentReasoning part whose
// Reasoning.Provider is not p removed. Every provider adapter calls it before
// translating a request, so reasoning from one provider is never sent to
// another and a transcript can move between providers. It returns msgs itself
// when there is nothing to remove, and otherwise a copy: msgs is not modified.
// A message left with no content is kept, the same as any other empty message.
func ReasoningFrom(p ProviderID, msgs []Message) []Message {
	return filterReasoning(msgs, func(r *Reasoning) bool { return r != nil && r.Provider == p })
}

// WithoutReasoning returns msgs with every ContentReasoning part removed.
// Removing all reasoning is always a valid request; use it before editing
// history (dropping or rewriting earlier turns, client-side compaction),
// since an edited history can invalidate the reasoning that follows it. It
// returns msgs itself when there is nothing to remove, and otherwise a copy:
// msgs is not modified.
func WithoutReasoning(msgs []Message) []Message {
	return filterReasoning(msgs, func(*Reasoning) bool { return false })
}

// filterReasoning drops reasoning parts for which keep is false, copying only
// the messages it changes.
func filterReasoning(msgs []Message, keep func(*Reasoning) bool) []Message {
	first := -1
	for i := range msgs {
		if dropCount(&msgs[i], keep) > 0 {
			first = i
			break
		}
	}
	if first < 0 {
		return msgs
	}
	out := make([]Message, first, len(msgs))
	copy(out, msgs[:first])
	for i := first; i < len(msgs); i++ {
		drop := dropCount(&msgs[i], keep)
		if drop == 0 {
			out = append(out, msgs[i])
			continue
		}
		m := Message{Role: msgs[i].Role, Content: make([]ContentPart, 0, len(msgs[i].Content)-drop)}
		for j := range msgs[i].Content {
			if p := &msgs[i].Content[j]; p.Type != ContentReasoning || keep(p.Reasoning) {
				m.Content = append(m.Content, *p)
			}
		}
		out = append(out, m)
	}
	return out
}

// dropCount reports how many of m's reasoning parts keep rejects.
func dropCount(m *Message, keep func(*Reasoning) bool) int {
	n := 0
	for j := range m.Content {
		if p := &m.Content[j]; p.Type == ContentReasoning && !keep(p.Reasoning) {
			n++
		}
	}
	return n
}
