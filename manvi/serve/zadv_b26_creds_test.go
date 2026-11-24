package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Adversarial pass on b26-creds: local.scan result fields.

// TestAdvB26CredsLocalScanNeverSendsACredentialOrCarriesTerminalControls: the
// scan plane names endpoints by host request, so it must send no Authorization
// header, and what it returns must not carry a listener's terminal controls.
func TestAdvB26CredsLocalScanNeverSendsACredentialOrCarriesTerminalControls(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"m\u001b]8;;https://evil.test\u0007x"},{"id":"ok"}]}`))
		case "/api/version":
			_, _ = w.Write([]byte(`{"version":"1\u001b[2J"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("OPENAI_API_KEY", "sk-advb26-sentinel")
	t.Setenv("LOCAL_API_KEY", "sk-advb26-sentinel")
	raw, _ := json.Marshal(ScanParams{Endpoints: []string{srv.URL + "/v1"}})
	got, opErr := scanServer().localScan(context.Background(), raw)
	if opErr != nil {
		t.Fatalf("localScan: %v", opErr)
	}
	for _, a := range auth {
		if a != "" {
			t.Fatalf("local.scan sent Authorization %q", a)
		}
	}
	body, _ := json.Marshal(got)
	var back ScanResult
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatal(err)
	}
	for _, s := range back.Servers {
		if strings.ContainsAny(s.Version, "\x1b\a") {
			t.Errorf("version carries a control byte after a JSON round trip: %q", s.Version)
		}
		for _, m := range s.Models {
			if strings.ContainsAny(m.ID, "\x1b\a") {
				t.Errorf("model id carries a control byte after a JSON round trip: %q", m.ID)
			}
		}
	}
}
