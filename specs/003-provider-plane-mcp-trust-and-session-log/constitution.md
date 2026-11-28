# Constitution: 003 - Model Provider Plane, Spend Ledger, MCP Trust, and Session Log

Derived from `docs/PHILOSOPHY.md` (principles 2 local-model-first, 4 fail-closed boundaries, 5 zero paywalls). Every change under `manvi/llm/`, `manvi/mcp/` and `manvi/session/` obeys these articles.

## Article I: Incomplete Is Never Complete
A tool call without its framing, a stream without its terminal event, a session file that fails its checksum, and a ledger file that cannot be parsed are errors. They are never treated as an empty or successful result (PHILOSOPHY.md principle 4).

## Article II: Over-Report Spend, Never Under-Report
A reservation is reduced only by proof that nothing was sent or by authoritative usage. Unprovable outcomes keep the reservation. A request the ledger cannot price is refused, because a cap that cannot price cannot enforce.

## Article III: An Untrusted Tree Cannot Authorize Itself
A declaration read from a checked-out tree spawns nothing until a fingerprint outside the tree authorizes it. Any change to what runs or what it is handed changes the fingerprint.

## Article IV: Model-Visible Means Logged
Content that reaches a model request must be reconstructable from the session log; a divergence is reported by index, not as a bare mismatch.

## Article V: Local-First, Free, No Account Logic
Local servers are discovered, not guessed; an ambiguous result is reported with the setting that resolves it. These packages contain no billing, sign-in or subscription code (PHILOSOPHY.md principle 5).

## Article VI: One Owner Per Rule
A rule shared by adapters (tool-call settlement, catalogue refusal wording, truncation typing) lives once and every adapter calls it; per-adapter copies are defects.
