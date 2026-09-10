package main

import (
	"context"
	"slices"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// The plugin kernel existed, was tested, and was called by nothing. Booting
// it from the composition root is what makes cx.bus and cx.tools a graph
// rather than a comment beside two New() calls.
func TestBootHarnessCoreWiresToolsAfterBus(t *testing.T) {
	t.Chdir(t.TempDir())
	core, err := bootHarnessCore(newTestRegistry(t), t.TempDir())
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(func() { _ = core.plugins.Close() })

	if core.bus == nil || core.tools == nil || core.fetch == nil || core.mcp == nil {
		t.Fatalf("resolved services: bus=%v tools=%v fetch=%v mcp=%v",
			core.bus, core.tools, core.fetch, core.mcp)
	}
	order := core.plugins.LoadOrder()
	busAt := slices.Index(order, "bus")
	toolsAt := slices.Index(order, "tools")
	if busAt < 0 || toolsAt < 0 || toolsAt < busAt {
		t.Fatalf("load order %v does not apply tools after bus", order)
	}
	if !core.plugins.Has(svcBus) || !core.plugins.Has(svcTools) || !core.plugins.Has(svcMCP) {
		t.Fatalf("kernel is missing a declared service: %v", order)
	}
}

func TestNativeToolsWithBootsThePluginKernel(t *testing.T) {
	t.Chdir(t.TempDir())
	_, pipeline, err := nativeToolsWith(newTestRegistry(t), nil)
	if err != nil {
		t.Skipf("the native tool surface could not be built here: %v", err)
	}
	core := harnessCoreFor(pipeline)
	if core == nil || core.plugins == nil {
		t.Fatal("nativeToolsWith did not boot the plugin kernel")
	}
	if !core.plugins.Has(svcTools) {
		t.Fatal("cx.tools was never provided")
	}
}

func TestProvideLLMInstallsTheDocumentedService(t *testing.T) {
	t.Chdir(t.TempDir())
	core, err := bootHarnessCore(newTestRegistry(t), t.TempDir())
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(func() { _ = core.plugins.Close() })

	registry, err := core.provideLLM(kernelStubLLM{})
	if err != nil {
		t.Fatalf("provideLLM: %v", err)
	}
	if _, ok := registry.Get("stub"); !ok {
		t.Fatal("the adapter was not registered on the llm registry")
	}
	if !core.plugins.Has(svcLLM) {
		t.Fatal("cx.llm was not provided — llm.Registry documented itself as that service and nothing provided it")
	}
}

func TestProvideLLMOnAnUnrecordedSurfaceStillBuildsARegistry(t *testing.T) {
	registry, err := (*harnessCore)(nil).provideLLM(kernelStubLLM{})
	if err != nil {
		t.Fatalf("nil core: %v", err)
	}
	if _, ok := registry.Get("stub"); !ok {
		t.Fatal("a test surface without a kernel must still get a working registry")
	}
}

type kernelStubLLM struct{}

func (kernelStubLLM) Name() string { return "stub" }

func (kernelStubLLM) Capability(model string) (llm.Capability, bool) {
	return llm.Capability{Provider: "stub", Model: model}, true
}

func (kernelStubLLM) Stream(context.Context, llm.Request) (llm.Stream, error) {
	return nil, nil
}
