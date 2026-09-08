package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// Handler implements one host-plane operation. Implementations return a wire
// error so callers can branch on a stable code without parsing prose.
type Handler func(context.Context, json.RawMessage) (any, *Error)

// Module adds or explicitly replaces operations on one Server. Keeping the
// router per server prevents one embedding host from changing another host's
// protocol through package-global registration.
type Module interface {
	Configure(*Router) error
}

// nilInterface reports typed nils held behind an interface. Module and service
// interfaces cross application boundaries, where `value == nil` is not enough:
// a nil pointer still has a dynamic type and would otherwise panic during
// configuration or on the first request.
func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// configureModule turns an extension panic into a configuration error. The
// server has not read protocol input yet, so refusing Serve is both fail-closed
// and fully correlated: no host request has been accepted or left unanswered.
func configureModule(module Module, router *Router) (err error) {
	completed := false
	defer func() {
		if completed {
			return
		}
		if panicked := recover(); panicked != nil {
			err = fmt.Errorf("host-plane module %T panicked during configuration: %v", module, panicked)
			return
		}
		err = fmt.Errorf("host-plane module %T panicked during configuration", module)
	}()
	err = module.Configure(router)
	completed = true
	return err
}

// Router is the configuration seam for host-plane operations.
type Router struct {
	handlers map[string]Handler
	frozen   bool
}

// Register adds an operation and refuses accidental collisions.
func (r *Router) Register(name string, handler Handler) error {
	if r.frozen {
		return fmt.Errorf("host-plane router is frozen after server configuration")
	}
	if err := validOperationName(name); err != nil {
		return err
	}
	if handler == nil {
		return fmt.Errorf("operation %q has no handler", name)
	}
	if _, exists := r.handlers[name]; exists {
		return fmt.Errorf("operation %q is already registered; use Replace to override it explicitly", name)
	}
	r.handlers[name] = handler
	return nil
}

// Replace swaps an existing operation. It refuses misspellings rather than
// silently registering a second path beside the one the host meant to replace.
func (r *Router) Replace(name string, handler Handler) error {
	if r.frozen {
		return fmt.Errorf("host-plane router is frozen after server configuration")
	}
	if name == OpHello {
		return fmt.Errorf("operation %q is reserved for protocol negotiation and cannot be replaced", name)
	}
	if err := validOperationName(name); err != nil {
		return err
	}
	if handler == nil {
		return fmt.Errorf("operation %q has no handler", name)
	}
	if _, exists := r.handlers[name]; !exists {
		return fmt.Errorf("operation %q is not registered and cannot be replaced", name)
	}
	r.handlers[name] = handler
	return nil
}

func (r *Router) freeze() { r.frozen = true }

func (r *Router) operations() []string {
	operations := make([]string, 0, len(r.handlers))
	for name := range r.handlers {
		operations = append(operations, name)
	}
	sort.Strings(operations)
	return operations
}

func validOperationName(name string) error {
	if name == "" || len(name) > 128 || strings.TrimSpace(name) != name {
		return fmt.Errorf("invalid operation name %q: names must be 1..128 non-space-delimited bytes", name)
	}
	for _, part := range strings.Split(name, ".") {
		if part == "" {
			return fmt.Errorf("invalid operation name %q: dot-delimited segments must not be empty", name)
		}
		for _, ch := range part {
			if !(ch >= 'a' && ch <= 'z') && !(ch >= '0' && ch <= '9') && ch != '-' && ch != '_' {
				return fmt.Errorf("invalid operation name %q: use lowercase letters, digits, dot, dash, or underscore", name)
			}
		}
	}
	return nil
}
