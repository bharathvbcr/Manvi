package llm

import "encoding/json"

// CloneMessages returns an owned snapshot, including nested image bytes,
// tool arguments and provider-private continuation state. Unknown block types
// are rejected by the same codec used for persisted conversations.
func CloneMessages(messages []Message) ([]Message, error) {
	if messages == nil {
		return nil, nil
	}
	raw, err := json.Marshal(messages)
	if err != nil {
		return nil, err
	}
	var copied []Message
	if err := json.Unmarshal(raw, &copied); err != nil {
		return nil, err
	}
	return copied, nil
}
