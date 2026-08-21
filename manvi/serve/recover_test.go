package serve

import (
	"errors"
	"strings"
	"testing"
)

// A panic must become an answer, not a dead process.
//
// The host is another program — DevPrism spawns this over stdio — so a panic
// costs it the session, not one command, and leaves it waiting on an id whose
// response will never arrive.
func TestAPanicBecomesAnInternalErrorRatherThanAnExit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		panic func()
	}{
		{"string", func() { panic("boom") }},
		{"error", func() { panic(errors.New("exploded")) }},
		{"nil dereference", func() {
			var p *struct{ N int }
			_ = p.N
		}},
		{"index out of range", func() {
			s := []int{}
			_ = s[3]
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, opErr := recovered(func() (any, *Error) {
				tc.panic()
				return "unreachable", nil
			})
			if opErr == nil {
				t.Fatalf("a panicking operation reported success: %v", result)
			}
			if opErr.Code != ErrInternal {
				t.Errorf("code = %q, want %q: the defect is in the harness, not the request",
					opErr.Code, ErrInternal)
			}
			if opErr.Retryable {
				t.Error("a panic was reported retryable; the same bytes panic the same way")
			}
			if result != nil {
				t.Errorf("a panicking operation produced a result: %v", result)
			}
			if !strings.Contains(opErr.Message, "defect in manvi") {
				t.Errorf("the error does not say whose defect it is: %q", opErr.Message)
			}
		})
	}
}

// Recovery must not change what a healthy operation returns.
func TestRecoveredPassesNormalOutcomesThrough(t *testing.T) {
	result, opErr := recovered(func() (any, *Error) { return "fine", nil })
	if opErr != nil {
		t.Fatalf("a healthy operation was reported as an error: %v", opErr)
	}
	if result != "fine" {
		t.Fatalf("result = %v, want %q", result, "fine")
	}

	want := badRequest("no such op")
	result, opErr = recovered(func() (any, *Error) { return nil, want })
	if opErr != want {
		t.Fatalf("an operation's own error was not passed through: got %v, want %v", opErr, want)
	}
	if result != nil {
		t.Fatalf("an erroring operation produced a result: %v", result)
	}
}

// A panic carrying nil still has to be caught. `panic(nil)` is legal, and a
// recover() that only acts on a non-nil value would let it through — which,
// before Go 1.21 made it a runtime error, was a silent process exit.
func TestAPanicWithNoValueIsStillCaught(t *testing.T) {
	defer func() {
		if escaped := recover(); escaped != nil {
			t.Fatalf("a nil panic escaped recovery: %v", escaped)
		}
	}()
	_, opErr := recovered(func() (any, *Error) {
		panic(nil) //nolint:govet // exercising the nil-panic path deliberately
	})
	if opErr == nil {
		t.Fatal("a nil panic was reported as success")
	}
	if opErr.Code != ErrInternal {
		t.Errorf("code = %q, want %q", opErr.Code, ErrInternal)
	}
}
