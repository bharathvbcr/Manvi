// Phase 5 note (DevCouncil verify/exec cutover):
//
// DevCouncil's Python package (`src/devcouncil/`) is deleted. Those
// `llm/{provider,router}.py` modules are not reimplemented here and must not
// be imported. Manvi already owns provider routing through this package and its
// adapters (anthropic, openaicompat, local, gemini, xai). DevCouncil's Go
// verify path does not call an LLM; when a host needs model access it uses
// Manvi.
//
// The former `executors/` tree (coding_cli, claude_sdk, agent_registry) maps
// to manvi/agents profiles and the agent loop — add profiles here rather than
// growing a second executor registry in DevCouncil Go.
package llm
