package serve

import (
	"context"
	"encoding/json"
	"errors"
	"log"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
	"github.com/bharathvbcr/Manvi/manvi/codingagent"
)

// WorkbenchModule exposes profile data to the local owning host. It is enabled
// explicitly with a profile Client; it is not an agent tool or permission grant.
type WorkbenchModule struct {
	Client        *store.Client
	Runner        *EnhancementRunner
	Configuration *EnhancementConfiguration
	Managed       *ManagedRunner
	// Devmap builds the code-index client for a validated repository root that
	// grounds refinements. Optional: without it a refinement is served
	// ungrounded and says so.
	Devmap func(root string) DevmapClient
	// Scan enables work.scan.gaps, .get and .cancel: a deterministic repository
	// audit that proposes inbox tasks and never writes the board itself.
	Scan *ScanRunner
	// Verify enables work.verify.task and work.verify.get. Nil leaves them
	// unregistered, so a build with no critic never offers a verdict it cannot
	// back.
	Verify *VerifyRunner
}

// EnhancementConfiguration describes the host's selection, not provider health.
// It contains no credential or permission settings and requires no model call.
type EnhancementConfiguration struct {
	OK          bool     `json:"ok"`
	Provider    string   `json:"provider"`
	Model       string   `json:"model"`
	ModelSource string   `json:"model_source"`
	Providers   []string `json:"providers"`
}

func (m WorkbenchModule) Configure(r *Router) error {
	if m.Client == nil {
		return errors.New("workbench module has no client")
	}
	if m.Runner != nil && m.Runner.client != m.Client {
		return errors.New("enhancement runner and host module must share the same profile client")
	}
	if m.Managed != nil && m.Managed.client != m.Client {
		return errors.New("managed runner and host must share the same profile client")
	}
	if m.Verify != nil && m.Verify.deps.Client != m.Client {
		return errors.New("verify runner and host module must share the same profile client")
	}
	for _, method := range store.WorkbenchMethods() {
		if err := r.Register("work."+method, func(ctx context.Context, raw json.RawMessage) (any, *Error) {
			result, err := m.Client.Workbench(ctx, method, raw)
			if err != nil {
				return nil, workbenchHostError("workbench", err)
			}
			if m.Runner != nil && m.Configuration != nil && (method == "items.put" || method == "automation.put") {
				var receipt struct {
					Queued bool `json:"automatic_enhancement_queued"`
				}
				if decodeErr := json.Unmarshal(result, &receipt); decodeErr != nil {
					log.Printf("workbench save wake unavailable: %s", m.Runner.errorText(decodeErr))
				} else if receipt.Queued || method == "automation.put" {
					// Persistence already succeeded. Wake-up is a coalesced hint; a
					// stopped coordinator must never turn that receipt into failure.
					if _, wakeErr := m.Runner.Wake(context.WithoutCancel(ctx), json.RawMessage(`{}`), *m.Configuration); wakeErr != nil {
						log.Printf("workbench save wake unavailable: %s", m.Runner.errorText(wakeErr))
					}
				}
			}
			return result, nil
		}); err != nil {
			return err
		}
	}
	if m.Managed != nil {
		// Published at `hello` so a host can refuse a provider this build has
		// no adapter for *before* it stores an attempt and takes the
		// repository's capacity. Read from the adapter set itself rather than
		// restated here: a transcribed list is the drift this exists to stop.
		r.managedProviders = append([]string(nil), codingagent.ManagedProviders...)
		if err := r.Register("work.runs.managed.prepare", func(ctx context.Context, raw json.RawMessage) (any, *Error) {
			result, err := m.Managed.Prepare(ctx, raw)
			if err != nil {
				return nil, workbenchHostError("managed preparation", err)
			}
			return result, nil
		}); err != nil {
			return err
		}
		if err := r.Register("work.runs.managed.activate", func(ctx context.Context, raw json.RawMessage) (any, *Error) {
			result, err := m.Managed.Activate(ctx, raw)
			if err != nil {
				return nil, workbenchHostError("managed activation", err)
			}
			return result, nil
		}); err != nil {
			return err
		}
		if err := r.Register("work.runs.managed.stop", func(_ context.Context, raw json.RawMessage) (any, *Error) {
			if err := m.Managed.Stop(raw); err != nil {
				return nil, workbenchHostError("managed stop", err)
			}
			return struct {
				OK bool `json:"ok"`
			}{true}, nil
		}); err != nil {
			return err
		}
	}
	if m.Scan != nil {
		for op, handler := range map[string]func(context.Context, json.RawMessage) (any, *Error){
			OpScanGaps:   m.Scan.Start,
			OpScanGet:    func(_ context.Context, raw json.RawMessage) (any, *Error) { return m.Scan.Get(raw) },
			OpScanCancel: func(_ context.Context, raw json.RawMessage) (any, *Error) { return m.Scan.Cancel(raw) },
		} {
			if err := r.Register(op, handler); err != nil {
				return err
			}
		}
	}
	if m.Verify != nil {
		if err := r.Register(OpVerifyTask, func(ctx context.Context, raw json.RawMessage) (any, *Error) {
			result, err := m.Verify.Start(ctx, raw)
			if err != nil {
				return nil, workbenchHostError("verification", err)
			}
			return result, nil
		}); err != nil {
			return err
		}
		if err := r.Register(OpVerifyGet, func(ctx context.Context, raw json.RawMessage) (any, *Error) {
			result, err := m.Verify.Get(ctx, raw)
			if err != nil {
				return nil, workbenchHostError("verification", err)
			}
			return result, nil
		}); err != nil {
			return err
		}
	}
	if m.Runner != nil {
		if m.Configuration != nil {
			configuration := *m.Configuration
			configuration.Providers = append([]string(nil), m.Configuration.Providers...)
			for _, op := range []string{"wake", "worker"} {
				if err := r.Register("work.enhancements."+op, func(ctx context.Context, raw json.RawMessage) (any, *Error) {
					if err := automaticInput(raw); err != nil {
						return nil, &Error{Code: "invalid_input", Message: err.Error()}
					}
					if op == "worker" {
						return m.Runner.AutomaticStatus(), nil
					}
					result, err := m.Runner.Wake(ctx, raw, configuration)
					if err != nil {
						return nil, workbenchHostError("automatic enhancement", err)
					}
					return result, nil
				}); err != nil {
					return err
				}
			}
			if err := r.Register(OpWorkRefine, func(ctx context.Context, raw json.RawMessage) (any, *Error) {
				// The grounding lookups run in the repository the request names,
				// never this process's working directory. A task record carries
				// repository ids, not paths, so the host supplies the root.
				var p struct {
					RepoRoot string `json:"repo_root"`
				}
				if err := json.Unmarshal(raw, &p); err != nil {
					return nil, badRequest("refinement params: %v", err)
				}
				root, e := canonicalRepoRoot(ctx, "repo_root", p.RepoRoot)
				if e != nil {
					return nil, e
				}
				var devmap DevmapClient
				if m.Devmap != nil {
					devmap = m.Devmap(root)
				}
				result, err := m.Runner.Refine(ctx, raw, configuration, devmap)
				if err != nil {
					return nil, workbenchHostError("refinement", err)
				}
				return result, nil
			}); err != nil {
				return err
			}
			if err := r.Register("work.enhancements.configuration", func(_ context.Context, raw json.RawMessage) (any, *Error) {
				if len(raw) == 0 {
					raw = json.RawMessage(`{}`)
				}
				if _, err := enhancementObject(raw, 1024); err != nil {
					return nil, &Error{Code: "invalid_input", Message: err.Error()}
				}
				return configuration, nil
			}); err != nil {
				return err
			}
		}
		return r.Register("work.enhancements.generate", func(ctx context.Context, raw json.RawMessage) (any, *Error) {
			result, err := m.Runner.Generate(ctx, raw)
			if err != nil {
				return nil, workbenchHostError("enhancement", err)
			}
			return result, nil
		})
	}
	return nil
}

func workbenchHostError(dependency string, err error) *Error {
	var refusal *store.WorkbenchError
	if errors.As(err, &refusal) {
		return &Error{Code: refusal.Code, Message: refusal.Message}
	}
	return dependencyError(dependency, err)
}
