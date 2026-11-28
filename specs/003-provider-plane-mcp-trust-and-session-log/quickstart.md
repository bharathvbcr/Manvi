# Quickstart: 003 - Model Provider Plane, Spend Ledger, MCP Trust, and Session Log

Run from the repository root `/Users/bharath/Code/devtools/Manvi`.

1. Run the provider plane suites (no network; adapters are exercised against local `httptest` servers):
   ```bash
   GOMAXPROCS=4 go -C manvi test -count=1 ./llm/...
   ```
2. Run MCP trust and session suites:
   ```bash
   GOMAXPROCS=4 go -C manvi test -count=1 -p 2 ./mcp/... ./session/...
   ```
3. Discover a local model server (needs Ollama, LM Studio, vLLM, llama.cpp or an MLX server running on loopback):
   ```bash
   manvi local
   manvi local --resolve
   ```
4. Authorize a workspace-declared MCP server by listing its fingerprint in `mcp-trust.json` in the user config directory, or for a headless run set `MANVI_MCP_TRUST` to the fingerprint. The fingerprint comes from the refusal the `Manager` prints for an unauthorized declaration.
5. Validate the specifications:
   ```bash
   bun run check:specs
   ```
