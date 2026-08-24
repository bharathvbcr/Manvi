package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"manvi/session"
)

// The credential backstop on a session log is armed at construction, and the
// only thing keeping that true across future edits is that there is one
// construction site. A second `session.NewLog()` somewhere in this package
// would build an unarmed log that looks identical everywhere else — it writes
// the same file and projects the same history, it just carries the credential.
//
// So the guard is on the source rather than on the behaviour: a behavioural
// test can only cover the paths someone remembered to exercise, and the failure
// here is precisely a path nobody remembered.
func TestSessionLogsAreArmedAtOneSite(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]int{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(string(body), "session.NewLog("); n > 0 {
			found[name] = n
		}
	}

	const owner = "sessionlog.go"
	if found[owner] != 1 {
		t.Errorf("%s should hold exactly one session.NewLog() call, found %d — "+
			"it is the one place the scrubber is attached", owner, found[owner])
	}
	delete(found, owner)
	for name, n := range found {
		t.Errorf("%s calls session.NewLog() %d time(s); use newSessionLog() so the "+
			"credential backstop is attached, or a log will write a key to disk and "+
			"send it to the provider on the next turn", name, n)
	}
}

// And the helper really does arm it, so the guard above is guarding something.
func TestNewSessionLogArmsTheScrubber(t *testing.T) {
	const fake = "sk-ant-api03-NOTAREALKEYAAAAAAAAAAAAAAAAAAAA"
	t.Setenv("ANTHROPIC_API_KEY", fake)

	log := newSessionLog()
	if _, err := log.Append(session.SystemPrompt, session.SystemPromptData{
		Text: "the environment holds " + fake,
	}); err != nil {
		t.Fatal(err)
	}
	for _, e := range log.Events() {
		if strings.Contains(string(e.Data), fake) {
			t.Fatalf("a credential from the environment survived into the log: %s", e.Data)
		}
	}
}
