package devmap

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdvancedQueryPreservesProducerEnvelopeAndIndexContract(t *testing.T) {
	status := `{"host_contract_version":1,"schema_version":19,"expected_schema_version":19,"schema_relation":"current","reader_ready":true,"query_ready":true,"db_path":"db","generation_id":7,"node_count":10,"edge_count":20,"is_fresh":true,"capabilities":{"explore":true}}`
	c := fake(t, map[string]string{
		"explore": `{"definitions":{"items":[{"symbol_name":"Router"}],"resolution":"Available","shown":1,"total":9,"hidden":8,"truncated":true,"tokens_used":12},"blast_radius":{"layers":{"items":[],"resolution":"Available","shown":0,"total":0,"hidden":0,"truncated":false,"tokens_used":0}},"budget":{},"query":"Router","limit":20}`,
		"status":  status,
	})
	result, err := c.Advanced(context.Background(), AdvancedQuery{Kind: QueryExplore, Query: "Router", Depth: 2, Budget: 1234})
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(result.Data, &envelope); err != nil {
		t.Fatal(err)
	}
	definitions, ok := envelope["definitions"].(map[string]any)
	if !ok {
		t.Fatalf("definitions envelope has type %T", envelope["definitions"])
	}
	if definitions["total"] != float64(9) || definitions["truncated"] != true {
		t.Fatalf("producer completeness fields were lost: %s", result.Data)
	}
	if result.Index.SchemaVersion != 19 || result.Index.ExpectedSchemaVersion != 19 {
		t.Fatalf("schema negotiation was lost: %+v", result.Index)
	}
}

func TestAdvancedQueryRefusesUnboundedOrMalformedArguments(t *testing.T) {
	c := fake(t, map[string]string{"status": healthyStatus})
	for _, query := range []AdvancedQuery{
		{Kind: QueryExplore, Query: "x", Depth: 0, Budget: 1},
		{Kind: QueryImpact, Query: "x", Depth: 65, Budget: 1},
		{Kind: QueryTrace, Query: "only-one-end", Depth: 1, Budget: 1},
		{Kind: QueryAffected, Targets: nil, Depth: 1, Budget: 1},
		{Kind: "shell", Query: "x", Depth: 1, Budget: 1},
		{Kind: QueryExplore, Query: "x", Depth: 1, Budget: 1, MinConfidence: math.NaN()},
		{Kind: QueryExplore, Query: strings.Repeat("x", 4097), Depth: 1, Budget: 1},
	} {
		if _, err := c.Advanced(context.Background(), query); err == nil {
			t.Errorf("accepted malformed query %+v", query)
		}
	}
}

func TestAdvancedQueryRefusesUnadvertisedOrIncompatibleProducer(t *testing.T) {
	cases := map[string]string{
		"future contract":    `{"host_contract_version":2,"schema_version":19,"expected_schema_version":19,"schema_relation":"current","reader_ready":true,"query_ready":true,"generation_id":7,"capabilities":{"impact":true}}`,
		"schema mismatch":    `{"host_contract_version":1,"schema_version":18,"expected_schema_version":19,"schema_relation":"upgradeable","reader_ready":false,"query_ready":false,"generation_id":7,"capabilities":{"impact":true}}`,
		"missing capability": `{"host_contract_version":1,"schema_version":19,"expected_schema_version":19,"schema_relation":"current","reader_ready":true,"query_ready":true,"generation_id":7,"capabilities":{"impact":false}}`,
	}
	for name, status := range cases {
		t.Run(name, func(t *testing.T) {
			c := fake(t, map[string]string{"status": status, "impact": `{}`})
			if _, err := c.Advanced(context.Background(), AdvancedQuery{Kind: QueryImpact, Query: "Router", Depth: 1, Budget: 1}); err == nil {
				t.Fatal("incompatible producer was queried")
			}
		})
	}
}

func TestAdvancedQueryRefusesGenerationDrift(t *testing.T) {
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	script := `#!/bin/sh
case "$*" in
  *status*)
    n=1
    if [ -f "` + count + `" ]; then n=2; fi
    echo '{"host_contract_version":1,"schema_version":19,"expected_schema_version":19,"schema_relation":"current","reader_ready":true,"query_ready":true,"db_path":"db","generation_id":'"$n"',"capabilities":{"impact":true}}'
    ;;
  *impact*) touch "` + count + `"; echo '{"items":[],"resolution":"Available","shown":0,"total":0,"hidden":0,"truncated":false,"tokens_used":0}' ;;
esac
`
	path := filepath.Join(dir, "devmap")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	// #nosec G302 -- this test fixture must be executable and is private to its temp directory.
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	c := New(path, dir)
	_, err := c.Advanced(context.Background(), AdvancedQuery{Kind: QueryImpact, Query: "Router", Depth: 1, Budget: 1})
	if err == nil || !strings.Contains(err.Error(), "changed during") {
		t.Fatalf("generation drift error = %v", err)
	}
}

func TestAdvancedQueryRefusesANonObjectEnvelope(t *testing.T) {
	status := `{"host_contract_version":1,"schema_version":19,"expected_schema_version":19,"schema_relation":"current","reader_ready":true,"query_ready":true,"db_path":"db","generation_id":7,"capabilities":{"impact":true}}`
	for _, payload := range []string{"null", `[]`, `"ok"`, `123`} {
		t.Run(payload, func(t *testing.T) {
			c := fake(t, map[string]string{"status": status, "impact": payload})
			_, err := c.Advanced(context.Background(), AdvancedQuery{Kind: QueryImpact, Query: "Router", Depth: 1, Budget: 1})
			if err == nil || !strings.Contains(err.Error(), "JSON object") {
				t.Fatalf("payload %s error = %v", payload, err)
			}
		})
	}
}

func TestAdvancedQueryRefusesObjectWithoutKindEnvelope(t *testing.T) {
	status := `{"host_contract_version":1,"schema_version":19,"expected_schema_version":19,"schema_relation":"current","reader_ready":true,"query_ready":true,"db_path":"db","generation_id":7,"capabilities":{"impact":true}}`
	c := fake(t, map[string]string{"status": status, "impact": `{}`})
	_, err := c.Advanced(context.Background(), AdvancedQuery{Kind: QueryImpact, Query: "Router", Depth: 1, Budget: 1})
	if err == nil || !strings.Contains(err.Error(), "completeness") {
		t.Fatalf("empty envelope error = %v", err)
	}
}

func TestAdvancedStatusRequiresConcreteSnapshotIdentity(t *testing.T) {
	base := Status{
		DBPath: "db", GenerationID: 7,
		HostContractVersion: 1, SchemaVersion: 19, ExpectedSchemaVersion: 19,
		SchemaRelation: "current", ReaderReady: true, QueryReady: true,
		Capabilities: map[string]any{"impact": true},
	}
	for name, mutate := range map[string]func(*Status){
		"empty database path": func(s *Status) { s.DBPath = "" },
		"blank database path": func(s *Status) { s.DBPath = " \t" },
		"zero generation":     func(s *Status) { s.GenerationID = 0 },
		"negative generation": func(s *Status) { s.GenerationID = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			status := base
			mutate(&status)
			if err := validateAdvancedStatus(&status, QueryImpact); err == nil {
				t.Fatalf("invalid snapshot identity was accepted: %+v", status)
			}
		})
	}
}

func TestAdvancedStatusUsesTheVersionedReadinessContract(t *testing.T) {
	status := &Status{
		DBPath: "db", GenerationID: 7,
		HostContractVersion: 1, SchemaVersion: 18, ExpectedSchemaVersion: 19,
		SchemaRelation: "compatible", ReaderReady: true, QueryReady: true,
		Capabilities: map[string]any{"impact": true},
	}
	if err := validateAdvancedStatus(status, QueryImpact); err != nil {
		t.Fatalf("versioned host contract declared this store readable and queryable: %v", err)
	}
}

func TestAdvancedResultRefusesInvalidAdapterStatusAndCompleteness(t *testing.T) {
	validStatus := &Status{
		DBPath: "db", GenerationID: 7,
		HostContractVersion: 1, SchemaVersion: 19, ExpectedSchemaVersion: 19,
		SchemaRelation: "current", ReaderReady: true, QueryReady: true,
		Capabilities: map[string]any{"impact": true},
	}
	validData := json.RawMessage(`{"items":[],"resolution":"Available","shown":0,"total":0,"hidden":0,"truncated":false,"tokens_used":0}`)

	incompatible := *validStatus
	incompatible.HostContractVersion = 2
	cases := map[string]AdvancedResult{
		"incompatible adapter status": {
			Data: validData, Index: &incompatible,
		},
		"impossible hidden count": {
			Data:  json.RawMessage(`{"items":[],"resolution":"Available","shown":0,"total":10,"hidden":0,"truncated":false,"tokens_used":0}`),
			Index: validStatus,
		},
		"shown differs from items": {
			Data:  json.RawMessage(`{"items":[],"resolution":"Available","shown":1,"total":1,"hidden":0,"truncated":false,"tokens_used":0}`),
			Index: validStatus,
		},
		"missing token accounting": {
			Data:  json.RawMessage(`{"items":[],"resolution":"Available","shown":0,"total":0,"hidden":0,"truncated":false}`),
			Index: validStatus,
		},
		"truncation disagrees with hidden": {
			Data:  json.RawMessage(`{"items":[],"resolution":"Available","shown":0,"total":1,"hidden":1,"truncated":false,"tokens_used":0}`),
			Index: validStatus,
		},
		"null resolution": {
			Data:  json.RawMessage(`{"items":[],"resolution":null,"shown":0,"total":0,"hidden":0,"truncated":false,"tokens_used":0}`),
			Index: validStatus,
		},
		"empty resolution string": {
			Data:  json.RawMessage(`{"items":[],"resolution":"","shown":0,"total":0,"hidden":0,"truncated":false,"tokens_used":0}`),
			Index: validStatus,
		},
		"empty resolution object": {
			Data:  json.RawMessage(`{"items":[],"resolution":{},"shown":0,"total":0,"hidden":0,"truncated":false,"tokens_used":0}`),
			Index: validStatus,
		},
	}
	for name, result := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateAdvancedResult(QueryImpact, result); err == nil {
				t.Fatalf("invalid adapter result was accepted: %+v", result)
			}
		})
	}
}

func TestAdvancedResultRefusesMissingNestedBlastRadiusCompleteness(t *testing.T) {
	status := &Status{DBPath: "db", GenerationID: 7, HostContractVersion: 1, SchemaVersion: 19, ExpectedSchemaVersion: 19, SchemaRelation: "current", ReaderReady: true, QueryReady: true, Capabilities: map[string]any{"explore": true}}
	data := json.RawMessage(`{"definitions":{"items":[],"resolution":"Available","shown":0,"total":0,"hidden":0,"truncated":false,"tokens_used":0},"blast_radius":{}}`)
	if err := ValidateAdvancedResult(QueryExplore, AdvancedResult{Data: data, Index: status}); err == nil || !strings.Contains(err.Error(), "blast_radius.layers") {
		t.Fatalf("missing nested completeness error = %v", err)
	}
}

func TestAdvancedResultRefusesNullResolution(t *testing.T) {
	status := &Status{DBPath: "db", GenerationID: 7, HostContractVersion: 1, SchemaVersion: 19, ExpectedSchemaVersion: 19, SchemaRelation: "current", ReaderReady: true, QueryReady: true, Capabilities: map[string]any{"impact": true}}
	data := json.RawMessage(`{"items":[],"resolution":null,"shown":0,"total":0,"hidden":0,"truncated":false,"tokens_used":0}`)
	if err := ValidateAdvancedResult(QueryImpact, AdvancedResult{Data: data, Index: status}); err == nil || !strings.Contains(err.Error(), "resolution") {
		t.Fatalf("null resolution error = %v", err)
	}
}
