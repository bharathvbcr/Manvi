package serve

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/testsupport"
)

// sqlite runs the sqlite3 shell against a profile: the tests need to age a
// profile and read its schema without going through the store under test.
func sqlite(t *testing.T, db, statement string) string {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 is not installed: cannot age a profile")
	}
	// The database and statement go in on stdin, not argv: nothing the test
	// builds becomes an argument to the shell it starts.
	cmd := exec.CommandContext(t.Context(), "sqlite3", "-batch")
	cmd.Stdin = strings.NewReader(".open '" + strings.ReplaceAll(db, "'", "''") + "'\n" + statement + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sqlite3 %q: %v: %s", statement, err, out)
	}
	return strings.TrimSpace(string(out))
}

func schemaOf(t *testing.T, db string) int {
	t.Helper()
	n, err := strconv.Atoi(sqlite(t, db, "SELECT version FROM work_meta WHERE id=1"))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// currentWorkbenchSchema is the schema the store under test gives a profile it
// creates: an upgrade must land there, whatever number DevCouncil is at.
func currentWorkbenchSchema(t *testing.T) int {
	t.Helper()
	db := filepath.Join(t.TempDir(), "fresh.sqlite")
	client := store.New(testsupport.DCStore(t), db)
	defer client.Close()
	if _, err := client.Workbench(t.Context(), "repositories.put", json.RawMessage(`{"request_id":"r","id":"r","expected_revision":0,"name":"Manvi","identity_key":"local:r"}`)); err != nil {
		t.Fatal(err)
	}
	current := schemaOf(t, db)
	if current <= 11 {
		t.Fatalf("a fresh profile is at schema %d, not past the aged 11: the upgrade tests would prove nothing", current)
	}
	return current
}

// agedProfile is a profile one schema behind the store under test, holding a
// repository and a task so an upgrade has something to lose.
func agedProfile(t *testing.T) (db string, client *store.Client) {
	t.Helper()
	db = filepath.Join(t.TempDir(), "profile.sqlite")
	client = store.New(testsupport.DCStore(t), db)
	t.Cleanup(client.Close)
	if _, err := client.Workbench(t.Context(), "repositories.put", json.RawMessage(`{"request_id":"r","id":"r","expected_revision":0,"name":"Manvi","identity_key":"local:r"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Workbench(t.Context(), "items.put", json.RawMessage(`{"request_id":"t","id":"t","expected_revision":0,"title":"keep me","repository_ids":["r"],"primary_repository_id":"r"}`)); err != nil {
		t.Fatal(err)
	}
	client.Close()
	// The 11 -> 12 step only moves the version, so this is a real older profile.
	sqlite(t, db, "UPDATE work_meta SET version=11 WHERE id=1")
	// A closed client is closed for good: the host under test gets its own.
	client = store.New(testsupport.DCStore(t), db)
	t.Cleanup(client.Close)
	return db, client
}

// manvi serve is GitPulse's sidecar. It reads a shared profile all the time and
// must never be the thing that moves its schema forward: that would lock out
// an older GitPulse, MCP server or sidecar that still shares the file.
func TestServeReadOfAnOlderProfileIsATypedRefusalAndLeavesTheProfileUntouched(t *testing.T) {
	db, client := agedProfile(t)
	responses := roundTrip(t, Options{HardRules: true, Modules: []Module{WorkbenchModule{Client: client}}},
		Request{ID: "list", Op: "work.items.list", Params: json.RawMessage(`{}`)},
		Request{ID: "get", Op: "work.items.get", Params: json.RawMessage(`{"id":"t"}`)},
	)
	for _, r := range responses {
		if r.OK || r.Error == nil || r.Error.Code != "schema_upgrade_required" {
			t.Fatalf("%s: the read of an older profile was not the typed refusal: %+v", r.ID, r)
		}
	}
	if got := schemaOf(t, db); got != 11 {
		t.Fatalf("a read moved the profile to schema %d", got)
	}
	matches, _ := filepath.Glob(db + ".pre-schema-*")
	if len(matches) != 0 {
		t.Fatalf("a refused read took a backup: %v", matches)
	}
}

// A write needs the current schema, so it upgrades — after an online backup —
// and the explicit op does the same without writing a task.
func TestServeWriteAndTheExplicitOpUpgradeAnOlderProfileAfterABackup(t *testing.T) {
	for _, via := range []string{"write", "explicit"} {
		t.Run(via, func(t *testing.T) {
			db, client := agedProfile(t)
			var request Request
			if via == "write" {
				request = Request{ID: "edit", Op: "work.items.put", Params: json.RawMessage(`{"request_id":"e","id":"t","expected_revision":1,"title":"edited","repository_ids":["r"],"primary_repository_id":"r"}`)}
			} else {
				request = Request{ID: "upgrade", Op: "work.store.upgrade", Params: json.RawMessage(`{}`)}
			}
			responses := roundTrip(t, Options{HardRules: true, Modules: []Module{WorkbenchModule{Client: client}}}, request)
			if !responses[0].OK {
				t.Fatalf("%s: %+v", via, responses[0])
			}
			if got := schemaOf(t, db); got != currentWorkbenchSchema(t) {
				t.Fatalf("schema after %s = %d", via, got)
			}
			backup := db + ".pre-schema-11.bak"
			if _, err := os.Stat(backup); err != nil {
				t.Fatalf("no backup before the upgrade: %v", err)
			}
			if got := schemaOf(t, backup); got != 11 {
				t.Fatalf("the backup is at schema %d, not the pre-upgrade 11", got)
			}
			if title := sqlite(t, backup, "SELECT json_extract(body,'$.title') FROM work_items WHERE id='t'"); title != "keep me" {
				t.Fatalf("the backup does not hold the original task: %q", title)
			}
		})
	}
}
