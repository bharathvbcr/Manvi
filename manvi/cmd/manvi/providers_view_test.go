package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/credentials"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
	"github.com/bharathvbcr/Manvi/manvi/ui/tui"
)

// providersEnv sets anthropic's key and leaves every other provider without
// one, so the report does not depend on the machine running the test.
func providersEnv(t *testing.T, key, workspace string) {
	t.Helper()
	t.Setenv("ANTHROPIC_API_KEY", key)
	t.Setenv(anthropicWorkspaceEnv, workspace)
	for _, v := range []string{"XAI_API_KEY", "GROK_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY"} {
		t.Setenv(v, "")
	}
}

// TestProvidersIsATableWithTheRemedyBeneath is the layout: a count, ready rows
// first, the state in one column, and for a missing key the variables to set
// with where to get one on the line beneath, under the same column.
func TestProvidersIsATableWithTheRemedyBeneath(t *testing.T) {
	const key = "sk-ant-fixture-providers-view"
	providersEnv(t, key, "  wrkspc_fixture01  ")

	var out bytes.Buffer
	if err := showProviders(&out, pinnedTo(t, "http://127.0.0.1:1/v1")); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if strings.Contains(text, "\x1b") {
		t.Fatalf("a buffer is not a terminal, and was sent escapes:\n%q", text)
	}
	if strings.Contains(text, key) {
		t.Fatalf("the report disclosed the key:\n%s", text)
	}

	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if lines[0] != "providers  1 of 4 ready" {
		t.Errorf("header = %q, want the count of usable providers", lines[0])
	}
	row := func(name string) (int, string) {
		for i, l := range lines {
			if f := strings.Fields(l); len(f) > 1 && f[1] == name {
				return i, l
			}
		}
		t.Fatalf("no row for %s in:\n%s", name, text)
		return 0, ""
	}

	first, anth := row("anthropic")
	if first != 2 {
		t.Errorf("anthropic is row %d; the ready provider belongs first:\n%s", first, text)
	}
	want := "ready" + strings.Repeat(" ", len("unavailable")-len("ready")) + "  ANTHROPIC_API_KEY · " +
		plural(len(key), "char") + " · workspace wrkspc_fixture01"
	if !strings.HasSuffix(anth, want) {
		t.Errorf("anthropic row = %q, want it to end %q", anth, want)
	}

	at, gem := row("gemini")
	if !strings.HasSuffix(gem, "set GEMINI_API_KEY or GOOGLE_API_KEY") {
		t.Errorf("gemini row = %q, want the variables that would supply it", gem)
	}
	doc := lines[at+1]
	column := columnOf(gem, "set GEMINI")
	if strings.TrimSpace(doc) != "https://console.cloud.google.com/apis/credentials (a Cloud console API key for the Generative Language API)" ||
		columnOf(doc, "https://") != column {
		t.Errorf("doc line = %q, want the key's source aligned under column %d", doc, column)
	}

	_, loc := row("local")
	stateAt := columnOf(anth, "ready")
	if columnOf(loc, "unavailable") != stateAt || columnOf(gem, "not set") != stateAt {
		t.Errorf("states are not one column:\n%s", text)
	}
}

// columnOf is the screen column sub starts at: the status marks are
// multi-byte, so a byte offset is not a column.
func columnOf(line, sub string) int {
	i := strings.Index(line, sub)
	if i < 0 {
		return -1
	}
	return render.StringWidth(line[:i])
}

// TestABadWorkspaceIsShownAndRefused: the report says why the key will not
// work, and a run refuses to build the adapter rather than send the header.
func TestABadWorkspaceIsShownAndRefused(t *testing.T) {
	providersEnv(t, "sk-ant-fixture-bad-workspace", "wrkspc 1")
	reg := pinnedTo(t, "http://127.0.0.1:1/v1")

	var out bytes.Buffer
	if err := showProviders(&out, reg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "anthropic  misconfigured  ANTHROPIC_API_KEY · ANTHROPIC_WORKSPACE_ID: workspace id contains ' '") {
		t.Errorf("report does not name the bad workspace:\n%s", out.String())
	}
	if _, err := buildProvider(context.Background(), "anthropic", reg, credentials.NewResolver(), io.Discard, ""); err == nil ||
		!strings.Contains(err.Error(), anthropicWorkspaceEnv) {
		t.Errorf("buildProvider = %v, want a refusal naming %s", err, anthropicWorkspaceEnv)
	}
}

// TestProviderColoursFollowTheStatusVocabulary: on a colour terminal the mark
// is the theme's pass colour, and body text keeps the terminal's own
// foreground — the theme's is chosen for a painted background and would
// vanish on a light terminal.
func TestProviderColoursFollowTheStatusVocabulary(t *testing.T) {
	th := tui.PickThemeByNameUnchecked("dark", render.TrueColor, true)
	lines := providerLines([]providerRow{
		{name: "anthropic", state: stateReady, source: "ANTHROPIC_API_KEY", facts: []string{"9 chars"}},
	}, th)

	spans := map[string]render.Style{}
	for _, span := range lines[2] {
		spans[span.Text] = span.Style
	}
	if got, want := spans[th.Glyphs().Pass].Fg, th.Status(tui.StatusPass).Fg; got != want {
		t.Errorf("ready mark fg = %v, want the pass colour %v", got, want)
	}
	name := spans["anthropic"]
	if !name.Fg.IsDefault() || !name.Has(render.Bold) {
		t.Errorf("provider name = %+v, want bold in the terminal's own colour", name)
	}
	if !spans["ANTHROPIC_API_KEY"].Fg.IsDefault() {
		t.Errorf("variable name carries a fixed foreground: %+v", spans["ANTHROPIC_API_KEY"])
	}
	for text, s := range spans {
		if !s.Bg.IsDefault() {
			t.Errorf("span %q paints a background into a terminal it does not own", text)
		}
	}
}
