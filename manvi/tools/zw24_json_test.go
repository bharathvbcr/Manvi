package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/internal/hygiene"
)

// mcp_list_tools answers a JSON document. A soft hyphen or a bidi override in a
// server's tool description reaches the seam inside a string of it, and what
// leaves must still parse and must not hand the rune back to whoever parses it.
func TestAThirdPartyJSONResultStaysJSONAndStaysInert(t *testing.T) {
	doc, err := json.Marshal(map[string]string{"description": "evil\u202egpj \u00ad \u0085 x"})
	if err != nil {
		t.Fatal(err)
	}
	// Pretty-printed with carriage returns, as a server written for Windows might.
	doc = []byte("{\r\n  " + string(doc[1:len(doc)-1]) + "\r\n}")
	got := w23Registry(t, "mcp_json", true, string(doc)).Run(context.Background(), Call{ID: "1", Name: "mcp_json", Arguments: []byte(`{}`)})
	if strings.ContainsRune(got.Text, '\r') {
		t.Fatalf("a live carriage return reached the result: %q", got.Text)
	}
	var back map[string]string
	if err := json.Unmarshal([]byte(got.Text), &back); err != nil {
		t.Fatalf("the escaped result is no longer JSON: %v\n%q", err, got.Text)
	}
	for _, r := range back["description"] {
		if hygiene.Invisible(r) {
			t.Fatalf("a parser of the result reads %U back out of it: %q", r, back["description"])
		}
	}
}
