package serve

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	mapclient "github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/devmap"
)

type fakeDevmap struct {
	result mapclient.AdvancedResult
	err    error
	query  mapclient.AdvancedQuery
}

func (f *fakeDevmap) Status(context.Context) (*mapclient.Status, error) {
	return f.result.Index, f.err
}

func (f *fakeDevmap) Advanced(_ context.Context, q mapclient.AdvancedQuery) (mapclient.AdvancedResult, error) {
	f.query = q
	return f.result, f.err
}

func TestDevmapModuleNegotiatesAndPreservesCompleteness(t *testing.T) {
	fake := &fakeDevmap{result: mapclient.AdvancedResult{
		Data:  json.RawMessage(`{"items":[{"symbol_name":"Router"}],"resolution":"Available","shown":1,"total":7,"hidden":6,"truncated":true,"tokens_used":12,"walk_incomplete":"lower bound"}`),
		Index: &mapclient.Status{DBPath: "db", HostContractVersion: 1, SchemaVersion: 19, ExpectedSchemaVersion: 19, SchemaRelation: "current", ReaderReady: true, QueryReady: true, GenerationID: 3, IsFresh: true, Capabilities: map[string]any{"impact": true}},
	}}
	responses := roundTrip(t, Options{HardRules: true, Modules: []Module{DevmapModule{ForRoot: func(string) DevmapClient { return fake }}}},
		Request{ID: "h", Op: OpHello},
		Request{ID: "q", Op: OpDevmapQuery, Params: rootParams(t, `"kind":"impact","query":"Router","depth":3,"budget":900,"min_rung":"high"`)})
	if !strings.Contains(string(responses[0].Result), OpDevmapQuery) || !strings.Contains(string(responses[0].Result), OpDevmapStatus) {
		t.Fatalf("hello = %s", responses[0].Result)
	}
	for _, field := range []string{`"total":7`, `"truncated":true`, `"walk_incomplete":"lower bound"`, `"schema_version":19`} {
		if !strings.Contains(string(responses[1].Result), field) {
			t.Errorf("query result lost %s: %s", field, responses[1].Result)
		}
	}
	if fake.query.Kind != mapclient.QueryImpact || fake.query.Depth != 3 || fake.query.MinRung != "high" {
		t.Fatalf("query mapping = %+v", fake.query)
	}
}

func TestDevmapDependencyFailureIsTypedAndSessionSurvives(t *testing.T) {
	fake := &fakeDevmap{err: errors.New("devmap schema 18 is incompatible with expected schema 19")}
	responses := roundTrip(t, Options{HardRules: true, Modules: []Module{DevmapModule{ForRoot: func(string) DevmapClient { return fake }}}},
		Request{ID: "bad", Op: OpDevmapQuery, Params: rootParams(t, `"kind":"impact","query":"Router","depth":3`)},
		Request{ID: "ok", Op: OpHello})
	responses = byID(t, responses, "bad", "ok")
	if responses[0].OK || responses[0].Error == nil || responses[0].Error.Code != ErrDependency {
		t.Fatalf("dependency failure = %+v", responses[0])
	}
	if !responses[1].OK {
		t.Fatalf("session died after dependency failure: %+v", responses[1])
	}
}

func TestDevmapStatusAcceptsWhitespaceObjectAndRejectsFields(t *testing.T) {
	fake := &fakeDevmap{result: mapclient.AdvancedResult{Index: &mapclient.Status{HostContractVersion: 1}}}
	responses := roundTrip(t, Options{Modules: []Module{DevmapModule{ForRoot: func(string) DevmapClient { return fake }}}},
		Request{ID: "space", Op: OpDevmapStatus, Params: json.RawMessage(" \n { \"root\": " + quote(devmapRoot(t)) + " } \t")},
		Request{ID: "field", Op: OpDevmapStatus, Params: rootParams(t, `"extra":1`)})
	if !responses[0].OK {
		t.Fatalf("whitespace object refused: %+v", responses[0])
	}
	if responses[1].OK || responses[1].Error == nil || responses[1].Error.Code != ErrBadRequest {
		t.Fatalf("unexpected status params accepted: %+v", responses[1])
	}
}

func TestTypedNilDevmapClientIsADependencyFailureNotAPanic(t *testing.T) {
	var fake *fakeDevmap
	response := roundTrip(t, Options{Modules: []Module{DevmapModule{ForRoot: func(string) DevmapClient { return fake }}}},
		Request{ID: "s", Op: OpDevmapStatus, Params: rootParams(t, ``)})[0]
	if response.OK || response.Error == nil || response.Error.Code != ErrDependency {
		t.Fatalf("typed-nil client = %+v", response)
	}
	if err := New(&strings.Builder{}, Options{Modules: []Module{DevmapModule{}}}).Serve(context.Background(), strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "no client source") {
		t.Fatalf("missing client source configuration error = %v", err)
	}
}

func TestDevmapAdapterCannotReturnNilSuccess(t *testing.T) {
	fake := &fakeDevmap{result: mapclient.AdvancedResult{}}
	responses := roundTrip(t, Options{Modules: []Module{DevmapModule{ForRoot: func(string) DevmapClient { return fake }}}},
		Request{ID: "status", Op: OpDevmapStatus, Params: rootParams(t, ``)},
		Request{ID: "query", Op: OpDevmapQuery, Params: rootParams(t, `"kind":"impact","query":"Router","depth":1`)})
	for _, response := range responses {
		if response.OK || response.Error == nil || response.Error.Code != ErrDependency {
			t.Fatalf("nil adapter output became success: %+v", response)
		}
	}
}

func TestDevmapAdapterCannotReturnZeroStatusAsSuccess(t *testing.T) {
	fake := &fakeDevmap{result: mapclient.AdvancedResult{
		Data:  json.RawMessage(`{"items":[],"resolution":"Available","shown":0,"total":0,"hidden":0,"truncated":false,"tokens_used":0}`),
		Index: &mapclient.Status{},
	}}
	response := roundTrip(t, Options{Modules: []Module{DevmapModule{ForRoot: func(string) DevmapClient { return fake }}}},
		Request{ID: "query", Op: OpDevmapQuery, Params: rootParams(t, `"kind":"impact","query":"Router","depth":1`)})[0]
	if response.OK || response.Error == nil || response.Error.Code != ErrDependency {
		t.Fatalf("zero adapter status became success: %+v", response)
	}
}

func TestInvalidDevmapQueryIsABadRequest(t *testing.T) {
	fake := &fakeDevmap{}
	response := roundTrip(t, Options{Modules: []Module{DevmapModule{ForRoot: func(string) DevmapClient { return fake }}}},
		Request{ID: "bad", Op: OpDevmapQuery, Params: rootParams(t, `"kind":"impact","query":"Router","depth":0`)})[0]
	if response.OK || response.Error == nil || response.Error.Code != ErrBadRequest {
		t.Fatalf("invalid caller input = %+v", response)
	}
}

// devmapRoot is a canonical git work tree the module accepts as a root.
func devmapRoot(t *testing.T) string {
	t.Helper()
	return scanRepo(t, map[string]string{"a.go": "package a\n"})
}

// rootParams builds devmap params naming a valid root plus the given members.
func rootParams(t *testing.T, rest string) json.RawMessage {
	t.Helper()
	if rest != "" {
		rest = "," + rest
	}
	return json.RawMessage(`{"root":` + quote(devmapRoot(t)) + rest + `}`)
}

// A host that omits the root, or names one that is not an absolute existing
// git work tree, is refused with E_BAD_ROOT and no client is ever built: the
// module must never answer for the sidecar's own working directory.
func TestDevmapRefusesMissingOrUnusableRootsWithoutBuildingAClient(t *testing.T) {
	built := 0
	module := DevmapModule{ForRoot: func(string) DevmapClient { built++; return &fakeDevmap{} }}
	plain := t.TempDir()
	cases := map[string]string{
		"missing":      `{"kind":"impact","query":"R","depth":1}`,
		"empty":        `{"root":"","kind":"impact","query":"R","depth":1}`,
		"relative":     `{"root":".","kind":"impact","query":"R","depth":1}`,
		"nonexistent":  `{"root":"/nonexistent/devmap/root","kind":"impact","query":"R","depth":1}`,
		"not git tree": `{"root":` + quote(plain) + `,"kind":"impact","query":"R","depth":1}`,
	}
	for name, params := range cases {
		for _, op := range []string{OpDevmapQuery, OpDevmapStatus} {
			p := params
			if op == OpDevmapStatus {
				p = strings.Replace(strings.Replace(p, `,"kind":"impact","query":"R","depth":1`, "", 1), `{"kind":"impact","query":"R","depth":1}`, `{}`, 1)
			}
			r := roundTrip(t, Options{Modules: []Module{module}}, Request{ID: "x", Op: op, Params: json.RawMessage(p)})[0]
			if r.OK || r.Error == nil || r.Error.Code != ErrBadRoot {
				t.Errorf("%s %s: %+v", name, op, r)
			}
		}
	}
	if built != 0 {
		t.Fatalf("a client was built for %d refused roots", built)
	}
}

func TestDevmapBindsTheClientToTheRequestedRoot(t *testing.T) {
	root := devmapRoot(t)
	var got []string
	fake := &fakeDevmap{result: mapclient.AdvancedResult{Index: &mapclient.Status{HostContractVersion: 1}}}
	module := DevmapModule{ForRoot: func(r string) DevmapClient { got = append(got, r); return fake }}
	r := roundTrip(t, Options{Modules: []Module{module}}, Request{ID: "s", Op: OpDevmapStatus, Params: json.RawMessage(`{"root":` + quote(root) + `}`)})[0]
	if !r.OK || len(got) != 1 || got[0] != root {
		t.Fatalf("status = %+v, roots = %v", r, got)
	}
}

func TestDevmapRefusesAnEnvelopeAboutAnotherRepository(t *testing.T) {
	data := `{"repository":{"root":"/somewhere/else"},"items":[],"resolution":"Available","shown":0,"total":0,"hidden":0,"truncated":false,"tokens_used":0}`
	fake := &fakeDevmap{result: mapclient.AdvancedResult{Data: json.RawMessage(data), Index: &mapclient.Status{DBPath: "db", HostContractVersion: 1, SchemaVersion: 19, ExpectedSchemaVersion: 19, SchemaRelation: "current", ReaderReady: true, QueryReady: true, GenerationID: 3, IsFresh: true, Capabilities: map[string]any{"impact": true}}}}
	r := roundTrip(t, Options{Modules: []Module{DevmapModule{ForRoot: func(string) DevmapClient { return fake }}}},
		Request{ID: "q", Op: OpDevmapQuery, Params: rootParams(t, `"kind":"impact","query":"R","depth":1`)})[0]
	if r.OK || r.Error == nil || r.Error.Code != ErrDependency || !strings.Contains(r.Error.Message, "/somewhere/else") {
		t.Fatalf("foreign envelope = %+v", r)
	}
}

// A repository field that is present but cannot be read is not the same as
// one that is absent: passing it would let an answer about another repository
// through whenever its envelope was malformed in the right way.
func TestDevmapRefusesAnEnvelopeWhoseRepositoryCannotBeRead(t *testing.T) {
	for name, repository := range map[string]string{
		"string":       `"/somewhere/else"`,
		"numeric_root": `{"root":7}`,
	} {
		t.Run(name, func(t *testing.T) {
			data := `{"repository":` + repository + `,"items":[],"resolution":"Available","shown":0,"total":0,"hidden":0,"truncated":false,"tokens_used":0}`
			fake := &fakeDevmap{result: mapclient.AdvancedResult{Data: json.RawMessage(data), Index: &mapclient.Status{DBPath: "db", HostContractVersion: 1, SchemaVersion: 19, ExpectedSchemaVersion: 19, SchemaRelation: "current", ReaderReady: true, QueryReady: true, GenerationID: 3, IsFresh: true, Capabilities: map[string]any{"impact": true}}}}
			r := roundTrip(t, Options{Modules: []Module{DevmapModule{ForRoot: func(string) DevmapClient { return fake }}}},
				Request{ID: "q", Op: OpDevmapQuery, Params: rootParams(t, `"kind":"impact","query":"R","depth":1`)})[0]
			if r.OK || r.Error == nil || r.Error.Code != ErrDependency {
				t.Fatalf("unreadable repository %s = %+v", repository, r)
			}
		})
	}
}
