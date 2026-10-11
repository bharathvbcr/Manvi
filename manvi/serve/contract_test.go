package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/testsupport"
	"github.com/bharathvbcr/Manvi/manvi/codingagent"
	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// contractPath is the serve seam contract, committed beside the verdict, event
// and lease contracts so a host vendors it the same way.
const contractPath = "../../contracts/serve.ops.schema.json"

// contractTypes are the wire shapes a host sends or reads, by the name the
// contract gives them. The map is the only hand-kept part of the contract; the
// field sets are read from the Go types themselves.
var contractTypes = map[string]reflect.Type{
	"HelloParams":        reflect.TypeFor[HelloParams](),
	"HelloResult":        reflect.TypeFor[HelloResult](),
	"OpLimit":            reflect.TypeFor[OpLimit](),
	"DevmapStatusParams": reflect.TypeFor[devmapStatusParams](),
	"DevmapQueryParams":  reflect.TypeFor[devmapQueryParams](),
	"RefineParams":       reflect.TypeFor[refineRequest](),
	"FileCheckParams":    reflect.TypeFor[FileCheckParams](),
	"CommandCheckParams": reflect.TypeFor[CommandCheckParams](),
	"HostScope":          reflect.TypeFor[HostScope](),
	"ManagedPrepared":    reflect.TypeFor[ManagedPrepared](),
	"ManagedRun":         reflect.TypeFor[managedRun](),
	"GapScanParams":      reflect.TypeFor[GapScanParams](),
	"GapScanReadParams":  reflect.TypeFor[GapScanReadParams](),
	"ScanView":           reflect.TypeFor[ScanView](),
	"VerifyTaskParams":   reflect.TypeFor[VerifyTaskParams](),
	"VerifyGetParams":    reflect.TypeFor[VerifyGetParams](),
	"VerifyResult":       reflect.TypeFor[VerifyResult](),
	"SuggestParams":      reflect.TypeFor[SuggestParams](),
	"SuggestResult":      reflect.TypeFor[SuggestResult](),
}

type contractField struct {
	Type     string                    `json:"type"`
	Required bool                      `json:"required"`
	Nullable bool                      `json:"nullable,omitempty"`
	Items    *contractField            `json:"items,omitempty"`
	Fields   map[string]*contractField `json:"fields,omitempty"`
}

type serveContract struct {
	Comment  string                    `json:"$comment"`
	Protocol int                       `json:"protocol"`
	Ops      contractOps               `json:"ops"`
	Types    map[string]*contractField `json:"types"`
}

type contractOps struct {
	// Always is what every build serves.
	Always []string `json:"always"`
	// Optional is what a build serves only when the host-plane module that
	// owns it is configured.
	Optional []string `json:"optional"`
}

func describeType(t reflect.Type) *contractField {
	out := &contractField{}
	if t.Kind() == reflect.Pointer {
		out.Nullable = true
		t = t.Elem()
	}
	switch {
	case t == reflect.TypeFor[json.RawMessage]():
		out.Type = "any"
	case t.Kind() == reflect.String:
		out.Type = "string"
	case t.Kind() == reflect.Bool:
		out.Type = "boolean"
	case t.Kind() >= reflect.Float32 && t.Kind() <= reflect.Float64:
		out.Type = "number"
	case t.Kind() >= reflect.Int && t.Kind() <= reflect.Uint64:
		out.Type = "integer"
	case t.Kind() == reflect.Map:
		out.Type = "map"
		out.Items = describeType(t.Elem())
	case t.Kind() == reflect.Slice:
		out.Type = "array"
		out.Items = describeType(t.Elem())
	case t.Kind() == reflect.Struct:
		out.Type = "object"
		out.Fields = map[string]*contractField{}
		for i := range t.NumField() {
			f := t.Field(i)
			tag, ok := f.Tag.Lookup("json")
			if !f.IsExported() || tag == "-" || !ok {
				continue
			}
			name, opts, _ := strings.Cut(tag, ",")
			child := describeType(f.Type)
			child.Required = !strings.Contains(opts, "omitempty")
			out.Fields[name] = child
		}
	default:
		panic("contract: unsupported wire type " + t.String())
	}
	return out
}

// opsOf reports the ops a server advertises in hello.
func opsOf(t *testing.T, opts Options) []string {
	t.Helper()
	resp := roundTrip(t, opts, Request{ID: "hello", Op: OpHello, Params: json.RawMessage(`{"protocol":1}`)})[0]
	if !resp.OK {
		t.Fatalf("hello failed: %+v", resp.Error)
	}
	var hello HelloResult
	if err := json.Unmarshal(resp.Result, &hello); err != nil {
		t.Fatal(err)
	}
	slices.Sort(hello.Ops)
	return hello.Ops
}

func currentContract(t *testing.T) serveContract {
	t.Helper()
	client := store.New(testsupport.DCStore(t), filepath.Join(t.TempDir(), "profile.sqlite"))
	t.Cleanup(client.Close)
	failure := func(err error) string { return err.Error() }
	managed, err := NewManagedRunner(client, func(context.Context, codingagent.Options) (ManagedSession, error) {
		return nil, errors.New("contract test starts no session")
	}, failure)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = managed.Close() })
	runner, err := NewEnhancementRunner(client, func(context.Context, string, string) (llm.Provider, error) {
		return nil, errors.New("contract test resolves no provider")
	}, failure)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.Close() })

	verify, err := NewVerifyRunner(VerifyDeps{
		Client: client, Critic: &scriptedCritic{}, Devmap: func(string) DevmapClient { return &fakeDevmap{} },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = verify.Close() })

	always := opsOf(t, Options{})
	scan, err := NewScanRunner(ScanDeps{
		Devmap: func(string) ScanDevmap { return &scanFakeDevmap{} },
		Text:   func(string) ScanText { return &scanFakeText{} },
		Board:  &scanFakeBoard{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scan.Close() })
	full := opsOf(t, Options{Modules: []Module{
		WorkbenchModule{Client: client, Runner: runner, Managed: managed, Scan: scan, Verify: verify, Configuration: &EnhancementConfiguration{}},
		DevmapModule{ForRoot: func(string) DevmapClient { return &fakeDevmap{} }},
	}})
	var optional []string
	for _, op := range full {
		if !slices.Contains(always, op) {
			optional = append(optional, op)
		}
	}
	c := serveContract{
		Comment: "Generated from the Go types in manvi/serve by TestServeContractMatchesGoTypes; " +
			"regenerate with MANVI_UPDATE_CONTRACT=1. Hosts mirror it and fail when they send a " +
			"field or name an op this file does not declare. Additive-only within a protocol version.",
		Protocol: ProtocolVersion,
		Ops:      contractOps{Always: always, Optional: optional},
		Types:    map[string]*contractField{},
	}
	for name, typ := range contractTypes {
		c.Types[name] = describeType(typ)
	}
	return c
}

func encodeContract(t *testing.T, c serveContract) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The contract is what a host's test reads to learn which fields it may send
// and which ops it may name. If it is allowed to drift from the Go types the
// host's test passes against a description nobody serves, which is how
// scope.allowed_commands went unnoticed: Rust sent it and Go dropped it.
func TestServeContractMatchesGoTypes(t *testing.T) {
	want := encodeContract(t, currentContract(t))
	if os.Getenv("MANVI_UPDATE_CONTRACT") == "1" {
		// #nosec G306 -- regenerates a tracked, published contract file; it is
		// world-readable like every other file in the repository.
		if err := os.WriteFile(contractPath, want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with MANVI_UPDATE_CONTRACT=1, then refresh contracts/CHECKSUMS)", contractPath, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s no longer matches the Go types in manvi/serve.\n"+
			"Regenerate: MANVI_UPDATE_CONTRACT=1 go test ./serve -run TestServeContractMatchesGoTypes, "+
			"then refresh contracts/CHECKSUMS and re-vendor into GitPulse.", contractPath)
	}
}

// The host's own check is only as good as the field set it reads; pin the one
// field whose absence caused the drift this contract exists to catch.
func TestServeContractDeclaresScopeAllowedCommands(t *testing.T) {
	scope, ok := currentContract(t).Types["HostScope"]
	if !ok || scope == nil {
		t.Fatal("the contract declares no HostScope type")
	}
	if _, ok := scope.Fields["allowed_commands"]; !ok {
		t.Fatal("HostScope does not declare allowed_commands")
	}
}
