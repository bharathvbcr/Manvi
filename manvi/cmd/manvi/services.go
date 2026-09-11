package main

import (
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/artifacts"
	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/core/plugin"
	"github.com/bharathvbcr/Manvi/manvi/fetch"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/mcp"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// Service keys the composition root Provides. They are the names plugin.go
// used as examples and llm.Registry documented itself as, while nothing ever
// called Boot — so the kernel was a tested container with no occupants.
const (
	svcBus       = "cx.bus"
	svcFetch     = "cx.fetch"
	svcMCP       = "cx.mcp"
	svcArtifacts = "cx.artifacts"
	svcTools     = "cx.tools"
	svcLLM       = "cx.llm"
)

// servicePlugin is a Plugin whose behaviour is a function. The production
// composition root is the one caller; tests that need a bare Apply use the
// same shape in core/plugin.
type servicePlugin struct {
	name     string
	provides []string
	deps     []string
	apply    func(*plugin.Ctx) (plugin.Dispose, error)
}

func (p *servicePlugin) Name() string       { return p.name }
func (p *servicePlugin) Provides() []string { return p.provides }
func (p *servicePlugin) Deps() []string     { return p.deps }
func (p *servicePlugin) Apply(cx *plugin.Ctx) (plugin.Dispose, error) {
	return p.apply(cx)
}

// harnessCore is the native tool surface, resolved out of the plugin kernel
// after Boot so a consumer never reaches a service it did not declare.
type harnessCore struct {
	plugins   *plugin.Registry
	bus       *bus.Bus
	fetch     *fetch.Client
	mcp       *mcp.Manager
	artifacts *artifacts.Store
	tools     *tools.Registry
}

func (c *harnessCore) Name() string { return "harness" }
func (c *harnessCore) Provides() []string {
	return nil
}
func (c *harnessCore) Deps() []string {
	return []string{svcBus, svcFetch, svcMCP, svcArtifacts, svcTools}
}

func (c *harnessCore) Apply(cx *plugin.Ctx) (plugin.Dispose, error) {
	var err error
	if c.bus, err = plugin.Resolve[*bus.Bus](cx, svcBus); err != nil {
		return nil, err
	}
	if c.fetch, err = plugin.Resolve[*fetch.Client](cx, svcFetch); err != nil {
		return nil, err
	}
	if c.mcp, err = plugin.Resolve[*mcp.Manager](cx, svcMCP); err != nil {
		return nil, err
	}
	if c.artifacts, err = plugin.Resolve[*artifacts.Store](cx, svcArtifacts); err != nil {
		return nil, err
	}
	if c.tools, err = plugin.Resolve[*tools.Registry](cx, svcTools); err != nil {
		return nil, err
	}
	return nil, nil
}

// provideLLM registers the session's adapter as cx.llm on the same kernel
// that already holds the tool surface. A nil core is the test path that
// never went through nativeToolsWith: the registry is still built, it is just
// not a plugin service.
func (c *harnessCore) provideLLM(provider llm.Provider) (*llm.Registry, error) {
	registry := llm.NewRegistry()
	if err := registry.Register(provider); err != nil {
		return nil, err
	}
	if c == nil || c.plugins == nil {
		return registry, nil
	}
	err := c.plugins.Boot(&servicePlugin{
		name:     "llm",
		provides: []string{svcLLM},
		apply: func(cx *plugin.Ctx) (plugin.Dispose, error) {
			return nil, cx.Provide(svcLLM, registry)
		},
	})
	return registry, err
}

func registerSessionLLM(pipeline *tools.Registry, provider llm.Provider) (*llm.Registry, error) {
	return harnessCoreFor(pipeline).provideLLM(provider)
}

func bootHarnessCore(reg *flags.Registry, root string) (*harnessCore, error) {
	core := &harnessCore{}
	plugins := plugin.New()
	if err := plugins.Boot(
		&servicePlugin{
			name:     "bus",
			provides: []string{svcBus},
			apply: func(cx *plugin.Ctx) (plugin.Dispose, error) {
				return nil, cx.Provide(svcBus, bus.New())
			},
		},
		&servicePlugin{
			name:     "fetch",
			provides: []string{svcFetch},
			apply: func(cx *plugin.Ctx) (plugin.Dispose, error) {
				return nil, cx.Provide(svcFetch, fetch.New(operatorFetchHosts(), fetch.Limits{}))
			},
		},
		&servicePlugin{
			name:     "mcp",
			provides: []string{svcMCP},
			apply: func(cx *plugin.Ctx) (plugin.Dispose, error) {
				mgr, err := buildMCP(reg, root)
				if err != nil {
					return nil, err
				}
				if err := cx.Provide(svcMCP, mgr); err != nil {
					return nil, err
				}
				return func() error {
					mgr.CloseAll()
					return nil
				}, nil
			},
		},
		&servicePlugin{
			name:     "artifacts",
			provides: []string{svcArtifacts},
			apply: func(cx *plugin.Ctx) (plugin.Dispose, error) {
				store, err := artifacts.NewStore(artifactsDir())
				if err != nil {
					store = nil
				}
				return nil, cx.Provide(svcArtifacts, store)
			},
		},
		&servicePlugin{
			name:     "tools",
			provides: []string{svcTools},
			deps:     []string{svcBus},
			apply: func(cx *plugin.Ctx) (plugin.Dispose, error) {
				b, err := plugin.Resolve[*bus.Bus](cx, svcBus)
				if err != nil {
					return nil, err
				}
				return nil, cx.Provide(svcTools, tools.NewRegistry(b))
			},
		},
		core,
	); err != nil {
		return nil, err
	}
	core.plugins = plugins
	return core, nil
}
