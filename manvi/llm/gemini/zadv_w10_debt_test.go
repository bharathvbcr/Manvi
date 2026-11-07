package gemini

import (
	"encoding/json"
	"testing"
)

func TestAdvW10DecodeFieldKeepsTheOldValueForAbsentAndNull(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage(""), json.RawMessage("null"), json.RawMessage(`"x"`)} {
		var oldV, newV string
		_ = json.Unmarshal(raw, &oldV)
		decodeField(t, raw, &newV)
		if oldV != newV {
			t.Errorf("raw %q: old %q, new %q", raw, oldV, newV)
		}
	}
}
