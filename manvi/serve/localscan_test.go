package serve

import (
	"context"
	"encoding/json"
	"testing"
)

func scanServer() *Server { return &Server{posture: PostureHost, hardRules: true} }

func scan(t *testing.T, params any) (ScanResult, *Error) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, opErr := scanServer().localScan(context.Background(), raw)
	if opErr != nil {
		return ScanResult{}, opErr
	}
	result, ok := got.(ScanResult)
	if !ok {
		t.Fatalf("localScan returned %T, want ScanResult", got)
	}
	return result, nil
}

// A scan that finds nothing must still say how hard it looked. "Nothing is
// running" and "we only looked in one place" are different answers.
func TestAScanReportsHowManyEndpointsItProbed(t *testing.T) {
	result, opErr := scan(t, ScanParams{Endpoints: []string{"http://127.0.0.1:9"}})
	if opErr != nil {
		t.Fatalf("unexpected error: %+v", opErr)
	}
	if result.Scanned != 1 {
		t.Errorf("Scanned = %d, want 1", result.Scanned)
	}
	if len(result.Servers) != 0 {
		t.Errorf("a closed port answered: %+v", result.Servers)
	}
}

func TestTheDefaultSweepCoversTheWellKnownEndpoints(t *testing.T) {
	result, opErr := scan(t, ScanParams{TimeoutMS: 50})
	if opErr != nil {
		t.Fatalf("unexpected error: %+v", opErr)
	}
	if result.Scanned < 2 {
		t.Errorf("Scanned = %d; the well-known list should be larger", result.Scanned)
	}
}

// Whether capabilities were asked for travels with the answer. Without it, a
// model list carrying no capabilities is indistinguishable from a scan that
// never asked, and a host would render capable models as incapable.
func TestTheResultRecordsWhetherCapabilitiesWereAsked(t *testing.T) {
	off, _ := scan(t, ScanParams{Endpoints: []string{"http://127.0.0.1:9"}})
	if off.Capabilities {
		t.Error("Capabilities reported true when it was not requested")
	}
	on, _ := scan(t, ScanParams{Endpoints: []string{"http://127.0.0.1:9"}, Capabilities: true})
	if !on.Capabilities {
		t.Error("Capabilities reported false when it was requested")
	}
}

// Serial dispatch makes a timeout everyone else's problem.
func TestTheTimeoutIsBoundedInBothDirections(t *testing.T) {
	if _, opErr := scan(t, ScanParams{TimeoutMS: -1}); opErr == nil {
		t.Error("a negative timeout was accepted")
	}
	if _, opErr := scan(t, ScanParams{TimeoutMS: maxScanTimeoutMS + 1}); opErr == nil {
		t.Error("a timeout past the ceiling was accepted; dispatch is serial, so " +
			"this is how long one scan holds every other call behind it")
	}
	if _, opErr := scan(t, ScanParams{TimeoutMS: 50}); opErr != nil {
		t.Errorf("a reasonable timeout was refused: %+v", opErr)
	}
}

func TestEmptyParamsAreAValidSweep(t *testing.T) {
	got, opErr := scanServer().localScan(context.Background(), nil)
	if opErr != nil {
		t.Fatalf("nil params should scan the well-known list: %+v", opErr)
	}
	if _, ok := got.(ScanResult); !ok {
		t.Fatalf("got %T, want ScanResult", got)
	}
}

func TestMalformedParamsAreRefusedRatherThanScannedAsDefaults(t *testing.T) {
	if _, opErr := scanServer().localScan(context.Background(), json.RawMessage(`{"timeout_ms":`)); opErr == nil {
		t.Error("malformed params were accepted")
	}
}

// The op is announced, or a host cannot feature-detect it and must guess.
func TestLocalScanIsAnnouncedInTheServedSet(t *testing.T) {
	found := false
	for _, op := range ops {
		if op == OpLocalScan {
			found = true
		}
	}
	if !found {
		t.Errorf("%q is dispatched but not in ops, so hello never mentions it", OpLocalScan)
	}
}
