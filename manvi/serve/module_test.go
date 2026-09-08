package serve

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type testModule struct {
	name    string
	fn      Handler
	replace bool
}

type retainingModule struct{ router *Router }

func (m *retainingModule) Configure(r *Router) error {
	m.router = r
	return r.Register("host.one", func(context.Context, json.RawMessage) (any, *Error) { return nil, nil })
}

func (m testModule) Configure(r *Router) error {
	if m.replace {
		return r.Replace(m.name, m.fn)
	}
	return r.Register(m.name, m.fn)
}

func TestHostModuleIsNegotiatedAndDispatched(t *testing.T) {
	module := testModule{name: "host.echo", fn: func(_ context.Context, raw json.RawMessage) (any, *Error) {
		var in struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, badRequest("host.echo params: %v", err)
		}
		return in, nil
	}}
	responses := roundTrip(t, Options{HardRules: true, Modules: []Module{module}},
		Request{ID: "hello", Op: OpHello},
		Request{ID: "echo", Op: "host.echo", Params: json.RawMessage(`{"value":"ok"}`)})
	if !strings.Contains(string(responses[0].Result), `"host.echo"`) {
		t.Fatalf("hello did not negotiate module operation: %v", responses[0])
	}
	if !strings.Contains(string(responses[1].Result), `"value":"ok"`) {
		t.Fatalf("module response = %v", responses[1])
	}
}

func TestDuplicateModuleOperationFailsClosed(t *testing.T) {
	module := testModule{name: OpHello, fn: func(context.Context, json.RawMessage) (any, *Error) {
		return map[string]bool{"replaced": true}, nil
	}}
	var out strings.Builder
	err := New(&out, Options{HardRules: true, Modules: []Module{module}}).
		Serve(context.Background(), strings.NewReader(`{"id":"1","op":"hello"}`+"\n"))
	if err == nil || !strings.Contains(err.Error(), `operation "hello" is already registered`) {
		t.Fatalf("Serve error = %v, want duplicate registration refusal", err)
	}
	if out.Len() != 0 {
		t.Fatalf("invalid module configuration wrote protocol output: %q", out.String())
	}
}

func TestReplacementIsExplicitAndVisibleInHello(t *testing.T) {
	module := testModule{name: OpLocalScan, replace: true, fn: func(context.Context, json.RawMessage) (any, *Error) {
		return map[string]string{"source": "host"}, nil
	}}
	responses := roundTrip(t, Options{HardRules: true, Modules: []Module{module}},
		Request{ID: "1", Op: OpLocalScan})
	if !strings.Contains(string(responses[0].Result), `"source":"host"`) {
		t.Fatalf("replacement response = %v", responses[0])
	}
}

func TestModuleNamesAreBoundedAndWellFormed(t *testing.T) {
	for _, name := range []string{"", " hello", "host echo", strings.Repeat("x", 129)} {
		t.Run(name, func(t *testing.T) {
			module := testModule{name: name, fn: func(context.Context, json.RawMessage) (any, *Error) { return nil, nil }}
			err := New(&strings.Builder{}, Options{Modules: []Module{module}}).Serve(context.Background(), strings.NewReader(""))
			if err == nil {
				t.Fatalf("operation name %q was accepted", name)
			}
		})
	}
}

func TestRouterIsFrozenAfterConfiguration(t *testing.T) {
	m := &retainingModule{}
	srv := New(&strings.Builder{}, Options{Modules: []Module{m}})
	if err := m.router.Register("host.late", func(context.Context, json.RawMessage) (any, *Error) { return nil, nil }); err == nil {
		t.Fatal("retained router changed the negotiated contract after New")
	}
	if err := m.router.Replace("host.one", func(context.Context, json.RawMessage) (any, *Error) { return nil, nil }); err == nil {
		t.Fatal("retained router replaced a handler after New")
	}
	if err := srv.Serve(context.Background(), strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
}
