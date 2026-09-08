package serve

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	mapclient "github.com/bharathvbcr/Manvi/manvi/dc/devmap"
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
	responses := roundTrip(t, Options{HardRules: true, Modules: []Module{DevmapModule{Client: fake}}},
		Request{ID: "h", Op: OpHello},
		Request{ID: "q", Op: OpDevmapQuery, Params: json.RawMessage(`{"kind":"impact","query":"Router","depth":3,"budget":900,"min_rung":"high"}`)})
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
	responses := roundTrip(t, Options{HardRules: true, Modules: []Module{DevmapModule{Client: fake}}},
		Request{ID: "bad", Op: OpDevmapQuery, Params: json.RawMessage(`{"kind":"impact","query":"Router","depth":3}`)},
		Request{ID: "ok", Op: OpHello})
	if responses[0].OK || responses[0].Error == nil || responses[0].Error.Code != ErrDependency {
		t.Fatalf("dependency failure = %+v", responses[0])
	}
	if !responses[1].OK {
		t.Fatalf("session died after dependency failure: %+v", responses[1])
	}
}

func TestDevmapStatusAcceptsWhitespaceObjectAndRejectsFields(t *testing.T) {
	fake := &fakeDevmap{result: mapclient.AdvancedResult{Index: &mapclient.Status{HostContractVersion: 1}}}
	responses := roundTrip(t, Options{Modules: []Module{DevmapModule{Client: fake}}},
		Request{ID: "space", Op: OpDevmapStatus, Params: json.RawMessage(" \n { } \t")},
		Request{ID: "field", Op: OpDevmapStatus, Params: json.RawMessage(`{"root":"/tmp"}`)})
	if !responses[0].OK {
		t.Fatalf("whitespace object refused: %+v", responses[0])
	}
	if responses[1].OK || responses[1].Error == nil || responses[1].Error.Code != ErrBadRequest {
		t.Fatalf("unexpected status params accepted: %+v", responses[1])
	}
}

func TestTypedNilDevmapClientFailsDuringConfiguration(t *testing.T) {
	var fake *fakeDevmap
	err := New(&strings.Builder{}, Options{Modules: []Module{DevmapModule{Client: fake}}}).
		Serve(context.Background(), strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "no client") {
		t.Fatalf("typed-nil client configuration error = %v", err)
	}
}

func TestDevmapAdapterCannotReturnNilSuccess(t *testing.T) {
	fake := &fakeDevmap{result: mapclient.AdvancedResult{}}
	responses := roundTrip(t, Options{Modules: []Module{DevmapModule{Client: fake}}},
		Request{ID: "status", Op: OpDevmapStatus, Params: json.RawMessage(`{}`)},
		Request{ID: "query", Op: OpDevmapQuery, Params: json.RawMessage(`{"kind":"impact","query":"Router","depth":1}`)})
	for _, response := range responses {
		if response.OK || response.Error == nil || response.Error.Code != ErrDependency {
			t.Fatalf("nil adapter output became success: %+v", response)
		}
	}
}

func TestInvalidDevmapQueryIsABadRequest(t *testing.T) {
	fake := &fakeDevmap{}
	response := roundTrip(t, Options{Modules: []Module{DevmapModule{Client: fake}}},
		Request{ID: "bad", Op: OpDevmapQuery, Params: json.RawMessage(`{"kind":"impact","query":"Router","depth":0}`)})[0]
	if response.OK || response.Error == nil || response.Error.Code != ErrBadRequest {
		t.Fatalf("invalid caller input = %+v", response)
	}
}
