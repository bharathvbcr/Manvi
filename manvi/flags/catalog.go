package flags

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Flag keys the harness defines. Referencing a flag by constant rather than by
// string literal means a rename is a compile error instead of a silent default.
const (
	// HarnessPosture is the top-level development posture. It is the one knob
	// an operator reaches for, and it governs the gate modes below unless one
	// of them is set explicitly.
	HarnessPosture = "harness.posture"

	// HarnessInitEnabled governs whether an invocation scaffolds the repository
	// it is standing in before it does anything else.
	HarnessInitEnabled = "harness.init.enabled"

	// Policy gates.
	PolicyFileMode      = "policy.file.mode"
	PolicyCommandMode   = "policy.command.mode"
	PolicyHardRules     = "policy.hard_rules.enabled"
	PolicyNeighborScope = "policy.scope.allow_neighbors"
	PolicyScopeSameDir  = "policy.scope.allow_same_dir"

	// Override seam.
	GrantsEnabled       = "grants.enabled"
	GrantsAgentEnabled  = "grants.agent.enabled"
	GrantsAgentMaxTTL   = "grants.agent.max_ttl"
	GrantsHumanMaxTTL   = "grants.human.max_ttl"
	GrantsRequireReason = "grants.require_reason"
	GrantsAgentCommands = "grants.agent.allow_commands"

	// Verification.
	VerifyDiffCoverageEnforce = "verify.diff_coverage.enforce"
	VerifyRigorEnabled        = "verify.rigor.enabled"

	// Orchestration.
	AgentsMaxSpawnDepth     = "agents.max_spawn_depth"
	AgentsMaxFanout         = "agents.max_fanout"
	SubagentsDynamicEnabled = "subagents.dynamic.enabled"

	// MCP & Open Plugins.
	MCPEnabled = "mcp.enabled"
	MCPConfig  = "mcp.config"

	// Pair programming.
	PairQuestionsEnabled = "pair.questions.enabled"

	// MaxSteps bounds one turn. Undotted and top-level, unlike every key
	// around it, because EnvKey derives the environment variable from the key
	// and this setting has always been read from MANVI_MAX_STEPS. Namespacing
	// it would retire that variable without saying so — an operator's existing
	// MANVI_MAX_STEPS would stop applying, `manvi flags` would report the
	// default as being in force, and nothing would report the instruction that
	// had been dropped. That is the failure StaleEnv exists to prevent for the
	// harness rename, and there is no per-key equivalent to lean on.
	MaxSteps = "max_steps"

	// Providers. Model IDs are deliberately not defaulted here — they must be
	// read from each vendor's current documentation, not from memory.
	//
	// There is one setting that chooses a provider and it is this one. There
	// used to be five: llm.provider.{anthropic,gemini,xai,local}.enabled sat
	// beside it claiming to enable each adapter, and no line of this harness
	// ever read them. See retirements below for why they were removed rather
	// than wired.
	LLMDefaultProvider = "llm.provider.default"
	LLMXAIBaseURL      = "llm.xai.base_url"
	LLMGeminiBaseURL   = "llm.gemini.base_url"
	LLMLocalBaseURL    = "llm.local.base_url"

	// The local adapter's declared dimensions. A local server publishes which
	// models it serves and nothing else, so which models exist is discovered
	// and everything below is the operator's own statement about their server.
	LLMLocalModel             = "llm.local.model"
	LLMLocalContextWindow     = "llm.local.context_window"
	LLMLocalMaxOutputTokens   = "llm.local.max_output_tokens"
	LLMLocalSupportsTools     = "llm.local.supports_tools"
	LLMLocalSupportsReasoning = "llm.local.supports_reasoning"
	LLMLocalAssumeModelServed = "llm.local.assume_model_served"
	LLMLocalTemperature       = "llm.local.temperature"
	LLMLocalTopP              = "llm.local.top_p"
	LLMLocalMinP              = "llm.local.min_p"
	LLMLocalRepetitionPenalty = "llm.local.repetition_penalty"
	LLMLocalStop              = "llm.local.stop"
	LLMLocalTopK              = "llm.local.top_k"
	LLMLocalPresencePenalty   = "llm.local.presence_penalty"
	LLMLocalFrequencyPenalty  = "llm.local.frequency_penalty"
	LLMLocalSeed              = "llm.local.seed"
	LLMLocalTrustDeclared     = "llm.local.trust_declared_context"
	LLMLocalStallTimeout      = "llm.local.stall_timeout"
	LLMLocalAssumePrefill     = "llm.local.assume_reasoning_prefill"
	LLMLocalCoreToolsOnly     = "llm.local.core_tools_only"
	LLMLocalDynamicTools      = "llm.local.dynamic_tools"
	LLMLocalGuidanceRouter    = "llm.local.guidance_router"

	// LLMEffort is the reasoning tier sent with every request, in each
	// provider's own field: Gemini's generation_config.thinking_level,
	// Anthropic's output_config.effort, xAI's reasoning_effort. One knob rather
	// than three, because the layer that assembles a request does not know
	// which vendor will receive it.
	LLMEffort = "llm.effort"

	// LLMEffortCeiling is how far the agent loop may raise LLMEffort within a
	// single turn when that turn stops making progress. Empty never raises it,
	// which is what every run did before this existed.
	//
	// It is a ceiling rather than a second static tier because the decision it
	// encodes is "spend more only if the cheap tier has been shown not to
	// work". Measured on this harness, reasoning is worth its price on a
	// genuine debugging task and costs 2.6x the tokens for nothing on a
	// mechanical one, and neither the operator nor the harness can reliably
	// tell which a prompt is before the turn runs.
	LLMEffortCeiling = "llm.effort.max"

	// Session log.
	LogModelVisibleAssert = "log.model_visible_assert"
)

// Modes shared by the policy gates, mirroring DevCouncil's gates.mode vocabulary.
const (
	ModeEnforce  = "enforce"
	ModeAdvisory = "advisory"
	ModeOff      = "off"
)

// Postures are the three ways the harness can be run.
//
// PostureDev is the default, and that default is a deliberate decision about
// what this system is for right now. A harness under active development that
// hard-stops on every scope disagreement is a harness that gets switched off,
// and a gate nobody runs protects nothing. So in dev the *soft* rules — the
// ones that encode "this task did not plan that file" — record and report
// instead of blocking. Every one still appears in the run report, named, with
// the rule that fired; the operator sees exactly what a strict run would have
// stopped.
//
// What dev posture does not touch is the hard rules. Credentials, paths
// outside the repository, and the agent client configs stay closed, because
// those are not development friction — a write to .env is not a scope
// disagreement, and no posture makes it one. Turning those off is a separate,
// explicit, startup-only flag that marks every decision it touches.
//
// PostureYolo is dev taken to its end: the soft rules do not merely report,
// they stop being asked. It exists because the alternative operators actually
// reach for is worse — a session spent answering approval cards ends with the
// operator setting policy.hard_rules.enabled=false, or running the model
// outside the harness entirely, and neither leaves a record. yolo is the
// supported way to say "stop asking me", so that saying it is one legible,
// named setting rather than an improvised dismantling of the gate.
//
// yolo takes the hard rules down with the soft ones. That is the whole of the
// difference from dev, and it is a large difference: the credential-path,
// restricted-path, malformed-path, and outside-root rungs stop firing, and so
// do the git-safety rules that refuse a force push or a --no-verify commit.
// The repository boundary goes with them — the write tools consult this same
// setting before resolving an escaping path, so under yolo the harness is not
// containing the agent to the repository at all. The only thing still refused
// is a path with no usable target: a NUL or control character, where the string
// the matcher sees and the string the kernel opens are not the same string.
//
// What does not change is the record. Every decision reached with the hard
// rules down carries policy.hard_rules.disabled in its Degraded list, so it
// can never be counted as a clean pass; the posture is never the safest value,
// so every run under it appears in Weakened(), in the run report, on the
// status bar, and in doctor; and it is human-only, because an agent that can
// put its own gate in yolo has no gate.
const (
	PostureDev    = "dev"
	PostureStrict = "strict"
	PostureYolo   = "yolo"
)

// PostureEffect describes what a posture does to the gates, in the three
// lengths the surfaces need.
//
// It is one function rather than a string literal per surface because the
// notice had already been copied into four of them, and a posture whose
// meaning is stated four times is a posture that will be described three
// different ways the first time it changes — which is exactly what adding a
// third posture does.
type PostureEffect struct {
	// Relaxed is true when this posture is not enforcing the soft rules.
	Relaxed bool
	// Short is a clause for a status bar, where there is no room for a
	// sentence: "dev posture demotes scope rules".
	Short string
	// Notice is the full sentence a session banner prints. It always says what
	// still blocks, because a warning that only lists what was turned off
	// reads as if everything was.
	Notice string
}

// DescribePosture returns what the named posture does. An unknown posture is
// reported as enforcing nothing away, because a posture this code does not
// recognise is not evidence that anything was relaxed.
func DescribePosture(posture string) PostureEffect {
	const stillBlocks = "Credential, outside-root, and agent-config rules still block."
	switch posture {
	case PostureDev:
		return PostureEffect{
			Relaxed: true,
			Short:   "dev posture demotes scope rules",
			Notice:  "dev posture: scope rules report instead of blocking. " + stillBlocks,
		}
	case PostureYolo:
		return PostureEffect{
			Relaxed: true,
			Short:   "yolo posture: all gates off, nothing contained",
			Notice: "yolo posture: every gate is off, hard rules included — credential paths, " +
				"restricted paths, git safety, and the repository boundary are not enforced, " +
				"and nothing is put to you for approval. Writes can land anywhere this process " +
				"can write. Every decision made this way is recorded as unchecked.",
		}
	default:
		return PostureEffect{}
	}
}

// DefineHarnessFlags registers the harness catalogue on a registry.
func DefineHarnessFlags(r *Registry) error {
	modes := []string{ModeEnforce, ModeAdvisory, ModeOff}
	defs := []Def{
		{
			Key: HarnessPosture, Kind: KindEnum, Values: []string{PostureDev, PostureStrict, PostureYolo},
			Default: PostureDev, Safest: PostureStrict, Mutable: HumanOnly, Safety: true,
			Description: "Development posture. dev: soft scope rules report instead of blocking, so the harness can be run while it is being built, while hard rules (credentials, outside-root, agent configs, git safety) still enforce. strict: everything enforces. yolo: every gate is off, hard rules included, and nothing is put to the operator for approval. A gate mode or policy.hard_rules.enabled set explicitly overrides this.",
		},
		{
			Key: HarnessInitEnabled, Kind: KindBool, Default: "true", Mutable: Startup,
			Description: "Scaffold the repository on startup: create the state directory the harness writes into, and append the managed rules to .gitignore. Off leaves the working tree untouched, and a missing state directory then surfaces as an unreachable store.",
		},
		{
			Key: PolicyFileMode, Kind: KindEnum, Values: modes, Default: ModeEnforce,
			Mutable: HumanOnly, Safety: true,
			Description: "File write gate: enforce blocks, advisory records findings, off skips soft rules. Hard rules always run. Left unset, harness.posture chooses.",
		},
		{
			Key: PolicyCommandMode, Kind: KindEnum, Values: modes, Default: ModeEnforce,
			Mutable: HumanOnly, Safety: true,
			Description: "Shell command gate. Same vocabulary as the file gate. Left unset, harness.posture chooses.",
		},
		{
			Key: PolicyHardRules, Kind: KindBool, Default: "true",
			Mutable: Startup, Safety: true,
			Description: "Hard rules (outside-root, secret paths, restricted paths, git safety). Startup-only and never agent-settable; turning this off is reported on every run, and every decision made without them is recorded as degraded. Left unset, the yolo posture turns them off; any other posture leaves them on.",
		},
		{
			Key: PolicyNeighborScope, Kind: KindBool, Default: "true",
			Mutable:     HumanOnly,
			Description: "Allow writes in the same or a neighbouring subsystem as a planned file, per the repo map.",
		},
		{
			Key: PolicyScopeSameDir, Kind: KindBool, Default: "true",
			Mutable: HumanOnly,
			Description: "When the repo map cannot place a path in a subsystem, allow writes in the same directory as a writable planned file. " +
				"The repository root never counts as such a directory, the allow is recorded as degraded, and this has no effect at all when " +
				"policy.scope.allow_neighbors is off — it is that rung's fallback, not a second one.",
		},

		{
			Key: GrantsEnabled, Kind: KindBool, Default: "true",
			Mutable:     HumanOnly,
			Description: "Master switch for the override seam. Off means a soft block can only be cleared by changing task scope.",
		},
		{
			Key: GrantsAgentEnabled, Kind: KindBool, Default: "true",
			Mutable: HumanOnly, Safety: true,
			Description: "Allow agents to grant their own overrides for agent-grantable rules within their lease scope.",
		},
		{
			Key: GrantsAgentMaxTTL, Kind: KindDuration, Default: "15m",
			Mutable: HumanOnly, Safety: true,
			Description: "Ceiling on the lifetime of an agent-issued grant.",
		},
		{
			Key: GrantsHumanMaxTTL, Kind: KindDuration, Default: "8h",
			Mutable:     HumanOnly,
			Description: "Ceiling on the lifetime of a human-issued grant.",
		},
		{
			Key: GrantsRequireReason, Kind: KindBool, Default: "true",
			Mutable: HumanOnly, Safety: true,
			Description: "Require a non-empty reason on every grant, so the evidence report can say why a block was cleared.",
		},
		{
			Key: GrantsAgentCommands, Kind: KindBool, Default: "false",
			Mutable: HumanOnly, Safety: true,
			Description: "Let an agent clear its own command-allowlist blocks, matching DevCouncil's agent_appended_allowed_commands. Off by default: an unplanned file write is still bounded by the gates above and by the verifier, whereas an arbitrary command is the mechanism those gates run through. Git-safety rules stay closed either way.",
		},

		{
			Key: VerifyDiffCoverageEnforce, Kind: KindBool, Default: "false",
			Mutable:     HumanOnly,
			Description: "Promote an unexercised diff to a blocking gap. Mirrors DevCouncil verification.diff_coverage.enforce. Files with no coverage data are promoted too: an absent measurement is not evidence the lines ran, and exempting it would make \"stop collecting coverage\" the cheapest way to satisfy an enforced gate.",
		},
		{
			Key: VerifyRigorEnabled, Kind: KindBool, Default: "true",
			Mutable: HumanOnly, Safety: true,
			Description: "Stub, effort, and coarse-acceptance-proof detection on added diff lines.",
		},

		{
			Key: AgentsMaxSpawnDepth, Kind: KindInt, Default: "2",
			Mutable:     HumanOnly,
			Description: "Maximum sub-agent delegation depth. A ceiling in code, not a prompt instruction.",
		},
		{
			Key: AgentsMaxFanout, Kind: KindInt, Default: "8",
			Mutable:     HumanOnly,
			Description: "Maximum concurrent sub-agents per parent.",
		},
		{
			Key: SubagentsDynamicEnabled, Kind: KindBool, Default: "true",
			Mutable:     HumanOnly,
			Description: "Let a model define new subagent role types at runtime. Off refuses devcouncil_define_subagent, naming this setting; the built-in roles stay invocable, and fan-out is bounded by agents.max_fanout rather than by this.",
		},
		{
			Key: MCPEnabled, Kind: KindBool, Default: "true",
			Mutable: HumanOnly,
			Description: "Discover and register Model Context Protocol (MCP 2.0 stateless) servers and Open Plugin 1.0 manifests. " +
				"Off means no declaration file or plugin directory is read and every mcp_* tool refuses, naming this setting — " +
				"the tools are still offered to the model, because the surface is built once and the refusal is what tells it. " +
				"Read when a tool surface is built, so a change applies to sessions opened after it.",
		},
		{
			Key: MCPConfig, Kind: KindString, Default: ".devcouncil/mcp.json",
			Mutable: Startup,
			Description: "Server declaration file, relative to the repository root or absolute. " +
				"Set explicitly, that file and only that file is read, and it must exist: a path someone wrote down " +
				"is a statement that servers are declared there, and a missing one is refused rather than resolved into 'no servers'. " +
				"Left at the default, that path is scanned along with ./mcp.json and ./.mcp.json and absence is not an error. " +
				"Either way a file that exists and does not parse fails the run.",
		},
		{
			Key: PairQuestionsEnabled, Kind: KindBool, Default: "true",
			Mutable:     HumanOnly,
			Description: "Enable interactive pair programming question asking.",
		},

		{
			// MANVI_MAX_STEPS used to be read straight from the environment
			// with fmt.Sscanf(v, "%d", &n) in cmd/manvi, bypassing this
			// catalogue entirely. Sscanf does not reject trailing input, so
			// "12x" parsed as 12 and "1e3" parsed as 1 — an operator writing
			// 1e3 meaning a thousand got a ceiling of one step and every turn
			// truncated after a single tool call. It was also invisible:
			// `manvi flags --all` could not list it and Lookup could not say
			// where its value came from, because as far as the registry was
			// concerned it did not exist.
			//
			// Defined here, it gets what every other setting already had. A
			// malformed value is refused at boot by LoadEnv rather than
			// silently reinterpreted, a config file can set it, and its origin
			// is reportable. `manvi run --max-steps N` still wins over all of
			// it, because a value typed for one invocation should.
			//
			// On the number itself: 24 was measured wrong, not merely tight.
			// Against a local Qwen3.8-27B, turns were repeatedly cut off at
			// exactly 24 steps while still making progress. A real change is
			// locate plus read plus edit plus two or three build-and-fix
			// rounds — over a hundred steps before anything has gone wrong. A
			// ceiling set in the middle of legitimate work is not a safety
			// bound, it is a random failure mode, and the exit-2 that reports
			// it stops meaning anything. See cmd/manvi's maxSteps for what
			// else bounds a turn; this is the last line, for the turn that is
			// bounded by nothing else.
			Key: MaxSteps, Kind: KindInt, Default: strconv.Itoa(DefaultMaxSteps), Mutable: HumanOnly,
			Description: "Step ceiling for one turn. Spent as a budget rather than counted: a step that made observable progress costs one, a step whose tool calls changed nothing costs three, so a turn going in circles reaches the ceiling nearer N/3 than N. A backstop rather than a work budget — context is bounded by compaction and repetition by the loop's own guards. Must be positive; 'manvi run --max-steps N' overrides it for one invocation.",
		},
		{
			Key: LLMDefaultProvider, Kind: KindString, Default: "anthropic",
			Mutable:     HumanOnly,
			Description: "Provider used when a role does not name one.",
		},
		{
			Key: LLMXAIBaseURL, Kind: KindString, Default: "https://api.x.ai/v1", Mutable: HumanOnly,
			Description: "Base URL for the xAI (Grok) adapter, from xAI's current documentation. Overridable for a proxy or gateway.",
		},
		{
			Key: LLMGeminiBaseURL, Kind: KindString,
			Default: "https://generativelanguage.googleapis.com/v1beta", Mutable: HumanOnly,
			Description: "Base URL for the Gemini adapter. The current API is /v1beta/interactions, not the older generateContent path.",
		},
		{
			Key: LLMLocalBaseURL, Kind: KindString, Default: "http://127.0.0.1:8000/v1", Mutable: HumanOnly,
			Description: "Base URL for the local OpenAI-compatible server. While this is unset the harness scans the addresses local runtimes listen on (Ollama 11434, vLLM 8000, LM Studio 1234, llama.cpp and mlx_lm.server 8080, Jan 1337) and uses the one that answers, reporting that it did; it picks nothing when several answer. Setting it stops the scan entirely — an address someone typed is used as typed. Run 'manvi local' to see what is running.",
		},
		{
			// No default, for the same reason no other provider's model is
			// defaulted: naming one the operator has not pulled produces a
			// refusal that reads as a harness fault. Empty means "read
			// MANVI_MODEL", and if that is empty too the refusal lists what the
			// server actually serves.
			Key: LLMLocalModel, Kind: KindString, Default: "", Mutable: HumanOnly,
			Description: "Model id to send to the local server. Empty reads MANVI_MODEL, then falls back to the server's own answer: a server offering exactly one model that can drive a coding turn needs no setting at all, and where there is a real choice the refusal names the candidates — excluding the embedding models and tool-less models that were never candidates.",
		},
		{
			Key: LLMLocalContextWindow, Kind: KindInt, Default: "32768", Mutable: HumanOnly,
			Description: "Declared context window of the local model, in tokens. The OpenAI model listing carries no such field, so this is the operator's statement about their own server rather than something discovered. Under-declaring costs earlier compaction; over-declaring produces a request the server truncates mid-turn.",
		},
		{
			Key: LLMLocalMaxOutputTokens, Kind: KindInt, Default: "16384", Mutable: HumanOnly,
			Description: "Declared output cap for one local response. Servers that silently truncate at their own default (mlx-vlm stops at 2048) turn a long edit into a cut-off answer the agent then retries, so this is sent explicitly on every request.",
		},
		{
			Key: LLMLocalSupportsTools, Kind: KindBool, Default: "true", Mutable: HumanOnly,
			Description: "Declare that the local server implements tool calling. True by default because a coding harness against a server without tools can do nothing at all — better to fail loudly on the first request than to be silently useless.",
		},
		{
			Key: LLMLocalSupportsReasoning, Kind: KindBool, Default: "false", Mutable: HumanOnly,
			Description: "Declare that the local server accepts reasoning_effort. Off by default: servers that do not understand the field differ in whether they ignore it or refuse the whole request, and the second loses a turn to a parameter nobody asked for.",
		},
		{
			Key: LLMLocalAssumeModelServed, Kind: KindBool, Default: "false", Mutable: HumanOnly,
			Description: "Accept the configured model without asking the server which models it serves. The escape hatch for a server with no /v1/models endpoint. Off by default: with it on the harness takes the operator's word instead of the server's, and a wrong model id fails at request time rather than at assembly.",
		},
		{
			// 0.7, and it is a deliberate departure from upstream rather than a
			// transcription of it. Read from the weights on this machine:
			// mlx-community/Qwen3.8-27B-4bit's generation_config.json declares
			// {"temperature": 1.0, "top_k": 20, "top_p": 0.95} — the 4bit and
			// nvfp4 snapshots under ~/.cache/huggingface are byte-identical on
			// these three fields. top_k and top_p below are that file as
			// written; only temperature is not.
			//
			// Why it differs. 1.0 is Qwen3's recommendation for general
			// generation, and this harness is not doing general generation: a
			// turn here is tool calls whose arguments are file paths and JSON,
			// where a sampled-off path is not a stylistic variation but a step
			// spent on a file that does not exist. Lowering temperature is the
			// available lever for that, and it costs nothing the guards do not
			// already cover.
			//
			// Why not lower still. It shipped at 0.1, which approximates greedy
			// decoding, and greedy decoding is what those same model cards name
			// as a cause of repetition loops — the failure the loop's
			// RepeatLimit guard exists to catch. 0.7 is short of the degenerate
			// end while still well below 1.0.
			//
			// If a future model's generation_config disagrees with this
			// reasoning rather than merely with this number, change the number.
			// An unacknowledged gap between what the comment cites and what the
			// catalogue ships is how a default stops being defensible.
			Key: LLMLocalTemperature, Kind: KindString, Default: "0.7", Mutable: HumanOnly,
			Description: "Sampling temperature for the local model. Accepted range is 0 to 2, the bounds OpenAI's request schema declares and every OpenAI-compatible server emulates. The shipped 0.7 is deliberately below the 1.0 Qwen3's generation_config.json recommends: a coding turn is tool arguments rather than prose, and a sampled-off file path costs a step. Empty omits the field and the server's own default applies.",
		},
		{
			Key: LLMLocalTopK, Kind: KindString, Default: "20", Mutable: HumanOnly,
			Description: "Top-k candidate cap for the local model. 20 is what Qwen3's shipped generation_config.json declares, unchanged. Must be 0 or more — the bound mlx_lm's server enforces; not an OpenAI field, so servers that do not implement it ignore it. Empty omits the field.",
		},
		{
			Key: LLMLocalPresencePenalty, Kind: KindString, Default: "", Mutable: HumanOnly,
			Description: "Presence penalty for the local model, between -2 and 2 as OpenAI's request schema declares. Empty omits the field. Raising it reduces repetition but can cause language mixing.",
		},
		{
			Key: LLMLocalFrequencyPenalty, Kind: KindString, Default: "", Mutable: HumanOnly,
			Description: "Frequency penalty for the local model, between -2 and 2 as OpenAI's request schema declares. Empty omits the field.",
		},
		{
			Key: LLMLocalSeed, Kind: KindString, Default: "", Mutable: HumanOnly,
			Description: "Sampling seed for the local model. Set it to make a run reproducible, which is what turns a failing turn from re-runnable into investigable. Empty omits the field.",
		},
		{
			Key: LLMLocalTrustDeclared, Kind: KindBool, Default: "false", Mutable: HumanOnly,
			Description: "Use the declared context window without asking the server for its own. Off by default: Ollama, vLLM and llama.cpp all publish the real window, and the declared default is small enough to waste most of a large model. Turn this on when the declaration is a deliberate restriction rather than a guess.",
		},
		{
			Key: LLMLocalStallTimeout, Kind: KindString, Default: "5m", Mutable: HumanOnly,
			Description: "Abandon a stream that produces no bytes for this long. Bounds the gap between tokens, not the length of the response, because legitimate first-token latency on a large local model can exceed a hosted model's entire reply. Unset uses the adapter default of 5m; 0, 0s, 'off' or 'none' disable it entirely. A negative value is refused — use one of those spellings to switch it off.",
		},
		{
			Key: LLMLocalCoreToolsOnly, Kind: KindBool, Default: "false", Mutable: HumanOnly,
			Description: "Offer only the core edit-loop tools to a local model, omitting task lifecycle, code-graph and sub-agent tools. Measured on Qwen3.8-27B over 240 paired trials: selection accuracy on tasks both surfaces can answer was 87.5% full vs 90.9% reduced, which is not statistically detectable (McNemar p=0.25) — though the reduced surface was never worse on any pair, and it removed the one observed distractor capture, where devcouncil_next_task answered \"delete this file\" 3 times in 8. The cost is not small and not safe: on tasks needing an omitted tool the reduced surface scored 0/32 and never once declined, substituting a confident wrong call every time (\"which task next?\" answered with list_dir). It saves 1,124 prompt tokens per request, but those sit in the cached prefix. Off by default because a measured 3.4pp that does not reach significance does not buy a silent 0%.",
		},
		{
			Key: LLMLocalDynamicTools, Kind: KindBool, Default: "true", Mutable: HumanOnly,
			Description: "Enable dynamic on-demand tool loading for local models. Starts with a lean core working set and loads extended tools (subagents, devmap navigation, MCP, artifacts) on demand via devcouncil_search_tools / devcouncil_activate_tools. Measured on the shipped registry: 37 tools and 4,695 estimated prompt tokens of schema become 17 and 2,063, a saving of 2,632 tokens per request — 8% of the default 32,768-token local window. The starting set is the same one llm.local.core_tools_only pins, but this is a floor rather than a ceiling: the discovery and activation tools are always offered, so a task needing an omitted tool can fetch it. That difference is the whole reason this defaults on where core_tools_only defaults off — core_tools_only was measured scoring 0/32 on tasks needing an omitted tool and never once declining, and an escape hatch is what turns that into a recoverable step. The prompt gains a tool-discovery section whenever this is on; without it the model is told an unlisted tool does not exist, which reproduces exactly that substitution. Note the saved tokens sit in the cached prefix, and activating a tool mid-turn invalidates it: the win is context headroom and selection accuracy, not per-turn prefill.",
		},
		{
			Key: LLMLocalGuidanceRouter, Kind: KindBool, Default: "true", Mutable: HumanOnly,
			Description: "Route the system prompt at compact density for local models: the same sections, worded tighter. Measured on the shipped local configuration, 901 estimated tokens become 696, a saving of 205 — 23%. The description this replaces claimed 50-70%, which was never measured and was reached partly by dropping the mode-guidance section outright, so turning the router on silently removed the pair-programming and YOLO guidance rather than condensing it; both densities now carry every section. Rules are never compressed and never dropped: identity, posture, the task rules, the policy rule and the tool contract are marked essential and survive any budget, so what yields under pressure is guidance and the project instructions file.",
		},
		{
			Key: LLMLocalAssumePrefill, Kind: KindBool, Default: "false", Mutable: HumanOnly,
			Description: "Declare that the server's chat template ends the prompt with an open thinking tag, as Qwen3's does. Off by default because the stream detects it and corrects the settled message; turning it on also makes the live view right from the first token.",
		},
		{
			Key: LLMLocalTopP, Kind: KindString, Default: "0.95", Mutable: HumanOnly,
			Description: "Nucleus sampling top_p threshold for the local model. 0.95 is what Qwen3's shipped generation_config.json declares, unchanged. Accepted range is 0 to 1. Empty uses provider default.",
		},
		{
			Key: LLMLocalMinP, Kind: KindString, Default: "", Mutable: HumanOnly,
			Description: "Min-P sampling threshold for the local model (e.g. 0.05), between 0 and 1. Scaled dynamically relative to top token probability. Not an OpenAI field; the bound is the one mlx_lm's server enforces. Empty uses provider default.",
		},
		{
			Key: LLMLocalRepetitionPenalty, Kind: KindString, Default: "", Mutable: HumanOnly,
			Description: "Repetition penalty for the local model, 0 or more. Not an OpenAI field; the bound is the one mlx_lm's server enforces. Empty uses provider default.",
		},
		{
			Key: LLMLocalStop, Kind: KindString, Default: "", Mutable: HumanOnly,
			Description: "Comma-separated stop tokens for the local model (e.g. '<|im_end|>,<|endoftext|>'). Empty omits custom stop sequences.",
		},
		{
			// Deliberately a string rather than an enum. The accepted levels
			// differ by model — Gemini serves low/medium/high, Anthropic adds
			// xhigh and max, xAI adds none — so a fixed list here would either
			// refuse a level a model accepts or accept one it does not. The
			// value is checked against the model's own capability when a
			// session attaches, and that error names what the model serves.
			//
			// Empty is not "no reasoning": it omits the field, which leaves the
			// provider's default in force. Choosing a level on the operator's
			// behalf would be this harness inventing a decision, the same
			// reason there is no default model.
			Key: LLMEffort, Kind: KindString, Default: "", Mutable: HumanOnly,
			Description: "Reasoning effort a turn starts at, sent in each provider's own field (Gemini thinking_level, Anthropic output_config.effort, xAI reasoning_effort). Empty omits the field and leaves the provider's default. Validated against the model when a session attaches.",
		},
		{
			// A string for the same reason llm.effort is one: the accepted
			// levels are the model's, not this catalogue's.
			//
			// Empty is the default and means the tier never moves, which is
			// what every run did before this setting existed. Raising it is
			// opt-in because it spends the operator's tokens: a harness that
			// decided on its own to start thinking harder than it was told to
			// would be making a budget decision that is not its to make.
			Key: LLMEffortCeiling, Kind: KindString, Default: "", Mutable: HumanOnly,
			Description: "Highest reasoning effort the agent loop may raise llm.effort to within one turn. A tool call refused as a verbatim repeat, or refused for making no observable progress, raises the tier one rung up the model's own ladder; a turn that keeps getting somewhere never leaves llm.effort. Empty never raises it. Requires llm.effort to name the tier a turn starts at, and must be above it.",
		},

		{
			Key: LogModelVisibleAssert, Kind: KindBool, Default: "true",
			Mutable: Startup, Safety: true,
			Description: "Assert at runtime that everything reaching a model request is reconstructable from the session log.",
		},
	}
	if err := r.Define(defs...); err != nil {
		return fmt.Errorf("harness flag catalogue: %w", err)
	}
	return nil
}

// EffectiveGateMode resolves the mode a gate actually runs in.
//
// It lives here rather than in the gate because two callers need the same
// answer: the gate, when it settles a decision, and `manvi doctor`, when it
// tells an operator what the gate will do. Those disagreeing is not a cosmetic
// bug — doctor printing "enforce" while the gate runs advisory is a report that
// actively misleads, which is worse than no report.
//
// An explicitly set mode always wins; an operator who typed a value meant it.
// Only a mode still sitting on its default defers to the posture, and the
// returned origin names whichever setting actually decided, so an operator
// knows which one to change.
func EffectiveGateMode(r *Registry, modeFlag string) (mode string, origin Origin, err error) {
	value, err := r.Lookup(modeFlag)
	if err != nil {
		return "", "", err
	}
	if value.Origin != OriginDefault {
		return value.Raw, value.Origin, nil
	}

	posture, postureOrigin, err := r.String(HarnessPosture)
	if err != nil {
		return "", "", err
	}
	// The default arm is enforce, and it covers strict together with any value
	// this code does not recognise. A posture it cannot map must resolve to the
	// strictest mode, never the loosest: an unrecognised setting is a reason to
	// enforce, not a licence to stop.
	switch posture {
	case PostureDev:
		return ModeAdvisory, postureOriginOf(PostureDev, postureOrigin), nil
	case PostureYolo:
		return ModeOff, postureOriginOf(PostureYolo, postureOrigin), nil
	default:
		return ModeEnforce, postureOrigin, nil
	}
}

// postureOriginOf names the posture that decided a mode, and where the posture
// itself came from, so an operator reading "off" knows which setting to change.
func postureOriginOf(posture string, origin Origin) Origin {
	return Origin(fmt.Sprintf("%s=%s/%s", HarnessPosture, posture, origin))
}

// EffectiveHardRules resolves whether the hard rules actually run, the way
// EffectiveGateMode resolves a gate mode and for the same reason: the gate and
// doctor must not be able to disagree about it.
//
// policy.hard_rules.enabled is startup-only, and the yolo posture reaches it
// without going through the registry — deliberately. Moving the flag itself
// would mean a runtime write to a sealed startup value, and the seal is the
// thing that stops a mid-turn caller from disarming the gate. Resolving it here
// instead leaves the flag exactly where an operator set it, and keeps the
// posture the single thing that has to be read to know what a run enforced.
//
// An explicitly set flag wins over the posture, in both directions: an operator
// who wrote policy.hard_rules.enabled=true meant it, and yolo does not overrule
// a value someone typed.
func EffectiveHardRules(r *Registry) (on bool, origin Origin, err error) {
	value, err := r.Lookup(PolicyHardRules)
	if err != nil {
		return false, "", err
	}
	if value.Origin != OriginDefault {
		return r.Bool(PolicyHardRules)
	}

	posture, postureOrigin, err := r.String(HarnessPosture)
	if err != nil {
		return false, "", err
	}
	if posture == PostureYolo {
		return false, postureOriginOf(PostureYolo, postureOrigin), nil
	}
	// Every other posture, including one this code cannot map, leaves the hard
	// rules where the flag put them. Failing to recognise a posture is not a
	// reason to stop enforcing the rules no posture was ever meant to reach.
	return r.Bool(PolicyHardRules)
}

// DefaultMaxSteps is the shipped step ceiling, and the value cmd/manvi falls
// back to when the resolved one is unusable. It lives beside the catalogue
// entry rather than at the call site because two copies of a default drift,
// and the one that drifts is always the one nobody is looking at.
const DefaultMaxSteps = 500

// Retirement names a setting this catalogue used to define and no longer does,
// with the environment variable that used to set it and what to do instead.
type Retirement struct {
	Key string
	Env string
	Why string
}

// retirements is every key removed from the catalogue, and it is the second
// half of removing one.
//
// Deleting a Def is not enough on its own, and the two layers fail differently.
// A config file is refused, but as "unknown key", which reads as a typo an
// operator should correct rather than as a decision someone made. The
// environment is not refused at all: LoadEnv iterates the *defined* flags, so
// MANVI_LLM_PROVIDER_LOCAL_ENABLED simply stops being looked at — no error, no
// warning, and `manvi flags` reporting the defaults as though nobody had asked
// for anything else. That is the failure StaleEnv exists to prevent for the
// harness rename, in the one shape StaleEnv cannot see, because these names
// carry the current prefix and always did.
//
// On the four provider switches. llm.provider.{anthropic,gemini,xai,local}
// .enabled each said "Enable the <vendor> adapter" and no line of this harness
// read any of them. They were not wired, for two reasons. The first is that
// there is nothing left for them to decide: llm.provider.default names the one
// adapter a run constructs (cmd/manvi's buildProvider switches on that name and
// nothing else), and the provider's credential decides whether it can be used
// — `manvi providers` reports exactly that. A per-provider gate on top has no
// third question to answer; the whole of its behaviour would be refusing the
// provider the operator had just explicitly selected.
//
// The second is what the shipped values would then have meant. local defaulted
// to false, and running a local server is this harness's main workflow — so
// wiring the gate as written would have broken every run using it, against a
// default nobody chose deliberately. Correcting the defaults to true instead
// leaves four keys whose only reachable effect is to break a working
// configuration. Neither is a capability; both are a second mechanism drifting
// beside the one that works.
var retirements = []Retirement{
	{
		Key: "llm.provider.anthropic.enabled",
		Why: "it was never read; llm.provider.default chooses the adapter and the credential decides whether it is usable",
	},
	{
		Key: "llm.provider.gemini.enabled",
		Why: "it was never read; llm.provider.default chooses the adapter and the credential decides whether it is usable",
	},
	{
		Key: "llm.provider.xai.enabled",
		Why: "it was never read; llm.provider.default chooses the adapter and the credential decides whether it is usable",
	},
	{
		Key: "llm.provider.local.enabled",
		Why: "it was never read; llm.provider.default chooses the adapter, and setting it to 'local' is what runs a local server",
	},
	// The two migration switches. Both named a second implementation to fall
	// back to, and in this tree there is no second implementation to name.
	//
	// devcouncil.surface offered "bridge", meaning shell out to the Python CLI.
	// There is no Python CLI: the port finished, this tree holds no .py file at
	// all, and `manvi tools` has been printing "all native — no Python process
	// is involved" beside a setting that offered to involve one. codeintel
	// .engine offered "python" and "auto" over the same absent engine; the one
	// engine is the devmap Rust binary, and the honest half of its description
	// — reporting which path executed — already ships on every navigation
	// answer as the degraded/stale fields.
	//
	// A rollback switch outlives the migration it was written for, and this is
	// what it becomes: an operator reading the catalogue is told they can run
	// an implementation nobody can run, and selecting it changes nothing.
	{
		Key: "devcouncil.surface",
		Why: "the migration it rolled back is finished; there is no Python CLI to bridge to, and every tool is native",
	},
	{
		Key: "codeintel.engine",
		Why: "there is one engine — the devmap Rust binary; which path executed is already reported on each navigation answer",
	},
}

// Retirements lists every removed setting, with its variable filled in.
func Retirements() []Retirement {
	out := make([]Retirement, 0, len(retirements))
	for _, r := range retirements {
		r.Env = EnvKey(r.Key)
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// RetiredEnv reports variables in environ that set a setting this harness no
// longer has.
//
// It takes the environment rather than reading it, for the reason StaleEnv
// does: a test should not have to mutate the process to exercise it. The caller
// turns a non-empty result into a startup error, not a warning — a warning
// about a setting that is already not applying is a warning about a decision
// already made.
func RetiredEnv(environ []string) []Retirement {
	set := map[string]bool{}
	for _, kv := range environ {
		if key, _, ok := strings.Cut(kv, "="); ok {
			set[key] = true
		}
	}
	var out []Retirement
	for _, r := range Retirements() {
		if set[r.Env] {
			out = append(out, r)
		}
	}
	return out
}

// RetiredConfig reports which keys in a config-file mapping are retired.
func RetiredConfig(values map[string]string) []Retirement {
	var out []Retirement
	for _, r := range Retirements() {
		if _, ok := values[r.Key]; ok {
			out = append(out, r)
		}
	}
	return out
}

// refuseRetiredConfig reads path and refuses it if it still sets a retired key.
//
// It runs before LoadConfigFile so the operator is told the setting was removed
// and why, rather than being told the name is unknown and left to conclude they
// mistyped it. The file is parsed twice as a result, which costs a few hundred
// bytes of IO once per process and keeps both readers on the same parser.
//
// A file that does not parse is passed over silently here, deliberately:
// LoadConfigFile is about to report it and reports it better, naming the line.
func refuseRetiredConfig(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		// Missing is not an error at this layer and every other read failure is
		// LoadConfigFile's to report; it opens the same path immediately after.
		return nil
	}
	defer func() { _ = f.Close() }()

	values, err := ParseConfig(f)
	if err != nil {
		return nil
	}
	retired := RetiredConfig(values)
	if len(retired) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "flags: %s sets settings this harness no longer has:\n", path)
	for _, r := range retired {
		fmt.Fprintf(&b, "  %s\t— %s\n", r.Key, r.Why)
	}
	b.WriteString("remove those lines")
	return errors.New(b.String())
}

// NewHarnessRegistry returns a registry with the harness catalogue defined and
// the environment layer loaded.
// configPath is where the committable config file lives, or empty to skip the
// layer entirely. The path is the caller's because MANVI_STATE_DIR is already
// owned there, and a second reader of it here would be a second answer to where
// the harness keeps its state.
//
// The layers are applied in precedence order, lowest first: config is written
// down and durable, the environment is what someone typed for this run, and the
// environment wins. That is the order Lookup has always documented; until now
// the middle layer was simply never filled.
func NewHarnessRegistry(configPath string) (*Registry, error) {
	r := New()
	if err := DefineHarnessFlags(r); err != nil {
		return nil, err
	}
	if err := refuseRetiredConfig(configPath); err != nil {
		return nil, err
	}
	if _, err := LoadConfigFile(r, configPath); err != nil {
		return nil, err
	}
	if err := r.LoadEnv(); err != nil {
		return nil, err
	}
	return r, nil
}

// Reach says how far a runtime change to a flag actually gets.
//
// A registry that accepts a change and reports the new value has done half a
// job: the other half is whether the code governed by that flag will read it.
// Most of this catalogue is consulted at the point of use, so a change lands
// immediately. A few keys are read once and snapshotted into something
// long-lived, and for those the honest answers are different — reload the thing
// that snapshotted it, or say plainly that it applies to the next session.
//
// This exists so `flags set` can say which of the three happened rather than
// printing a new value and leaving the operator to discover that nothing
// changed. TestFlagReachCoversTheCatalogue keeps it total: a flag added without
// a classification fails the build's tests rather than defaulting to the
// reassuring answer.
type Reach string

const (
	// ReachLive means every consumer reads the registry at the point of use, so
	// the next decision uses the new value.
	ReachLive Reach = "live"
	// ReachReload means a consumer snapshotted the value and an attended face
	// rebuilds that consumer when the flag moves. Live once the reload runs.
	ReachReload Reach = "reload"
	// ReachNewSession means the value is snapshotted into state a running
	// session cannot rebuild without dropping work in progress. Sessions
	// started afterwards pick it up; the ones already open do not.
	ReachNewSession Reach = "new-session"
	// ReachBoot is a startup-only flag: it cannot be moved after Seal at all.
	ReachBoot Reach = "boot"
)

// reachByPrefix classifies the keys that are not read at the point of use.
//
// Each entry names the snapshot site that put it here, so the classification
// can be re-checked against the code rather than trusted:
//
//   - grants.*  — gate.grantPolicyFrom copies these into the Ledger's Policy at
//     gate.New. Gate.ReloadPolicy recomputes it, which is why this is reload
//     rather than new-session.
//   - llm.*     — harnessHost.attachProvider resolves the provider, model and
//     effort once and caches them on the session. Dropping the cached provider
//     makes the next turn resolve them again.
//   - mcp.*     — buildMCP builds the MCP manager while the tool surface is
//     being assembled, and the surface holds live server processes and the
//     session's lease. Rebuilding it under a running session would strand both.
var reachByPrefix = []struct {
	prefix string
	reach  Reach
}{
	{"grants.", ReachReload},
	{"llm.", ReachReload},
	{"mcp.", ReachNewSession},
}

// ReachOf reports how far a change to this flag gets. A startup flag answers
// ReachBoot whatever its namespace, because Seal refuses it before reach is
// even a question.
func ReachOf(d Def) Reach {
	if d.Mutable == Startup {
		return ReachBoot
	}
	for _, r := range reachByPrefix {
		if strings.HasPrefix(d.Key, r.prefix) {
			return r.reach
		}
	}
	return ReachLive
}
