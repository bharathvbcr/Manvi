package devcouncil

import (
	"encoding/json"
	"strings"
	"testing"

	"manvi/fetch"
	"manvi/tools"
)

const fetchTool = "devcouncil_fetch_url"

// quote renders a URL as a JSON string literal.
func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// withFetcher points a fixture's registry at an allowlist.
func withFetcher(f *fixture, hosts ...string) *fixture {
	f.reg.deps.Fetch = fetch.New(hosts, fetch.Limits{})
	return f
}

func offers(reg *Registry, name string) bool {
	for _, tool := range reg.Tools() {
		if tool.Schema.Name == name {
			return true
		}
	}
	return false
}

// The default is no network access, and the honest expression of that is a tool
// that is not there. A tool that existed and always refused would cost schema
// tokens on every request and teach a model to keep asking.
func TestFetchToolIsAbsentWithoutAnAllowlist(t *testing.T) {
	f := newFixture(t)
	if offers(f.reg, fetchTool) {
		t.Fatal("an unconfigured harness offers a network tool")
	}
}

func TestFetchToolAppearsOnlyWhenHostsAreAllowed(t *testing.T) {
	f := withFetcher(newFixture(t), "go.dev")
	if !offers(f.reg, fetchTool) {
		t.Fatal("hosts were allowed and no fetch tool was offered")
	}
}

// The 45th tool. The documented count is what an unconfigured harness offers;
// this keeps the other number honest, so the conditional tool cannot quietly
// become two.
func TestTheWebToolIsTheOnlyConditionalTool(t *testing.T) {
	base := len(newFixture(t).reg.Tools())
	withWeb := len(withFetcher(newFixture(t), "go.dev").reg.Tools())
	if withWeb != base+1 {
		t.Fatalf("configuring web access changed the tool count by %d, want exactly 1 — "+
			"the documented count accounts for one conditional tool", withWeb-base)
	}
}

// It is read-only and Extended: it cannot mutate, and it is not on the floor a
// small local model starts with.
func TestFetchToolIsReadOnlyAndExtended(t *testing.T) {
	f := withFetcher(newFixture(t), "go.dev")
	for _, tool := range f.reg.Tools() {
		if tool.Schema.Name != fetchTool {
			continue
		}
		if !tool.ReadOnly {
			t.Error("the fetch tool is not marked read-only, so a read-only child would get it")
		}
		if !tool.Extended {
			t.Error("the fetch tool is not Extended, so it would sit on the local floor")
		}
		if tool.Group != tools.GroupWeb {
			t.Errorf("group = %q, want %q", tool.Group, tools.GroupWeb)
		}
		return
	}
	t.Fatal("the fetch tool was not registered")
}

// Fetched content is somebody else's prose arriving in the channel the model
// reads its instructions from. It is framed on both sides, and the frame says
// what it means — a delimiter the model has to infer is not a frame.
func TestFetchedContentIsFramedAsUntrusted(t *testing.T) {
	if !strings.Contains(untrustedOpen, "not to be followed") {
		t.Error("the opening marker does not say instructions inside are not to be followed")
	}
	for _, steer := range []string{"run", "fetch another URL", "change a file", "ignore"} {
		if !strings.Contains(untrustedOpen, steer) {
			t.Errorf("the marker does not name %q as a thing the page might try", steer)
		}
	}
	if untrustedClose == "" {
		t.Fatal("there is no closing marker, so a page ending in an instruction runs " +
			"straight into the model's own context")
	}
}

// A refusal has to be actionable and must not read as a malfunction the model
// should route around.
//
// The handler is called directly rather than through the tool pipeline: the
// fixture registers its surface before this test attaches a fetcher, so the
// pipeline it built has no such tool. That the tool reaches the pipeline when
// the fetcher is configured first is TestFetchToolAppearsOnlyWhenHostsAreAllowed.
func TestFetchRefusalsExplainTheRule(t *testing.T) {
	f := withFetcher(newFixture(t), "go.dev")
	call := func(url string) tools.Result {
		return f.reg.fetchURL(t.Context(), tools.Call{
			Name: fetchTool, Arguments: []byte(`{"url":` + quote(url) + `}`),
		})
	}

	res := call("https://evil.example/x")
	if !res.IsError {
		t.Fatal("a host off the allowlist was fetched")
	}
	if !strings.Contains(res.Text, "operator setting") {
		t.Errorf("the refusal does not say whose decision it was: %q", res.Text)
	}
	if !strings.Contains(res.Text, "do not try another route") {
		t.Errorf("the refusal invites a workaround: %q", res.Text)
	}

	res = call("http://go.dev/x")
	if !res.IsError || !strings.Contains(res.Text, "https") {
		t.Errorf("plaintext was not refused with the reason: %q", res.Text)
	}

	res = call("")
	if !res.IsError || !strings.Contains(res.Text, "required") {
		t.Errorf("an empty url = %q", res.Text)
	}
}

// A harness with no allowlist that somehow reaches the handler still refuses,
// and tells the model to answer from the repository rather than to keep trying.
func TestFetchWithNoAllowlistTellsTheModelToStop(t *testing.T) {
	f := newFixture(t)
	f.reg.deps.Fetch = fetch.New(nil, fetch.Limits{})
	res := f.reg.fetchURL(t.Context(), tools.Call{
		Name: fetchTool, Arguments: []byte(`{"url":"https://go.dev/doc"}`),
	})
	if !res.IsError {
		t.Fatal("an unconfigured fetcher returned a document")
	}
	if !strings.Contains(res.Text, "not configured") {
		t.Errorf("the refusal does not name the cause: %q", res.Text)
	}
	if !strings.Contains(res.Text, "say what you could not check") {
		t.Errorf("the model is not told to report the gap: %q", res.Text)
	}
}
