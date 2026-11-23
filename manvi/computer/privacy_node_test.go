package computer

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/workflow"
)

const sensitiveEntryJSON = `{
  "schema_version":1,"id":"pay.card","revision":"1","application":"pay",
  "parameters":[{"name":"card","type":"string","sensitive":true}],
  "targets":{
    "card":{"strategies":[{"role":"text_field","name":"Card"},{"role":"text_field","identifier":"card-number"}],"rationale":"card field","stability":"semantic"}
  },
  "outcomes":[{"id":"done","kind":"success","description":"entered","outputs":[]}],
  "steps":[
    {"id":"card","kind":"set_value","target":"card","effect":"read","input":{"source":"input","key":"card"}},
    {"id":"done","kind":"conclude","outcome":"done"}
  ],
  "limits":{"max_actions":40,"active_seconds":300,"observation_attempts":3,"intervention_seconds":600}
}`

func compileSensitiveEntry(t *testing.T) *workflow.Program {
	t.Helper()
	p, err := workflow.Compile([]byte(sensitiveEntryJSON))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// maskedPixelRed is the 16-bit red sample at (x, y) of a recorded frame.
func maskedPixelRed(t *testing.T, o Observation, x, y int) uint32 {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(o.Screenshot.Base64)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	r, _, _, _ := img.At(x, y).RGBA()
	return r
}

func TestSensitiveInputTargetsNamesTheWrittenControlOnly(t *testing.T) {
	got := sensitiveInputTargets(compileSensitiveEntry(t).Capability())
	want := []Selector{{Role: "text_field", Name: "Card"}, {Role: "text_field", Identifier: "card-number"}}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rung %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
	// A purely visual rung has no node to name and must not become an empty
	// selector, which would match (and mask) every node.
	c := compileSensitiveEntry(t).Capability()
	c.Targets["card"] = workflow.Selector{Strategies: []workflow.Selector{{Visual: &workflow.VisualAnchor{}}, {Role: "text_field", Name: "Card"}}}
	if n := sensitiveInputTargets(c); len(n) != 1 || n[0] != (Selector{Role: "text_field", Name: "Card"}) {
		t.Fatalf("visual rung handling: %+v", n)
	}
	// A non-sensitive parameter names nothing.
	c.Parameters[0].Sensitive = false
	if n := sensitiveInputTargets(c); len(n) != 0 {
		t.Fatalf("non-sensitive input produced targets %+v", n)
	}
}

// An application may reformat what it is given, so the literal string the host
// watches never appears in what it echoes back. The node the secret was typed
// into must be masked by identity, not by finding the string.
func TestSensitiveInputNodeIsMaskedWhenTheApplicationReformatsTheSecret(t *testing.T) {
	const secret = "4111111111111111"
	const echoed = "4111 1111 1111 1111"
	o := observationFixture(t)
	value := echoed
	identifier := "card-number"
	o.Nodes = []Node{{ID: "card", Role: "text_field", Name: "Card", Identifier: &identifier, Value: &value, Enabled: true, Editable: true, Actions: []string{"set_value"}, Bounds: &Bounds{10, 10, 40, 20}}}
	d := &testDesktop{observation: o}
	var records []Record
	opts := RunOptions{
		Program: compileSensitiveEntry(t), Desktop: d,
		Inputs:   map[string]workflow.Value{"card": {Type: "string", Text: secret}},
		Session:  Session{ID: "session", RunID: "run", Epoch: 1, Window: o.Window},
		OnRecord: func(r Record) error { records = append(records, r); return nil },
	}
	ctx := safetyContext(t)
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	result, err := run.Wait(ctx)
	if err != nil || result.State.Phase != workflow.Completed {
		t.Fatalf("%+v %v", result, err)
	}
	if len(records) == 0 {
		t.Fatal("no records observed")
	}
	sawFrame := false
	for _, r := range records {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "4111") {
			t.Fatalf("record %s/%s exposes the reformatted secret: %s", r.Kind, r.Stage, b)
		}
		if r.Observation != nil && r.Observation.Screenshot.Base64 != "" {
			sawFrame = true
			if px := maskedPixelRed(t, *r.Observation, 20, 20); px > 10000 {
				t.Fatalf("record %s/%s left the sensitive control's pixels unmasked", r.Kind, r.Stage)
			}
		}
	}
	if !sawFrame {
		t.Fatal("no recorded frame to check")
	}
}
