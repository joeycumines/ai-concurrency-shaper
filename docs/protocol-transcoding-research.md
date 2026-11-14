# Protocol Transcoding: Research Reference and Architectural Tradeoffs

> [!NOTE]
> **Status:** Non-normative research reference. This document synthesizes formal models, failure-mode taxonomies, and verification techniques from recent protocol-transcoding literature alongside an analysis of their engineering tradeoffs in a Go reverse proxy.
>
> **Authoritative behavior:** The behavioral invariants, contract-role strictness rules, directional loss policy, streaming lifecycle, and proxy guarantees for this repository are defined in [AGENTS.md](file:///Users/joeyc/dev/ai-concurrency-shaper/AGENTS.md). Nothing in this document overrides `AGENTS.md` or mandates a specific internal refactoring.

---

## 1. Context and Problem Scope

Translating bidirectional, streaming requests and responses across $N$ heterogeneous LLM APIs—specifically **OpenAI Chat Completions**, **OpenAI Responses / Open Responses**, **Anthropic Messages**, and **Google Gemini (`generateContent` / `streamGenerateContent`)**—presents two distinct engineering problems:

1. **Structural divergence:** Each protocol enforces a different streaming lifecycle (flat delta chunks, strictly bracketed content-block indices, hierarchical item-and-part events, or full-part candidate chunks) and emits metadata in different orders.
2. **Semantic and multi-turn preservation:** Protocols do not share identical capabilities. A translation can pass static JSON schema validation and return `HTTP 200` while silently dropping semantic flags (`is_error`), altering causal turn order, corrupting parallel tool-call indices, or stripping cryptographic continuity tokens (`thinking.signature`, `thoughtSignature`, `reasoning.encrypted_content`) required on turn $k+1$.

### Research Summary

This reference draws on four lines of work:

| Thread | Core Idea | Primary Source |
| :--- | :--- | :--- |
| **Symbolic Register Transducers (SRTs) & Featured Transition Systems (FTS)** | Model streaming translators as control-flow phase machines paired with mutable registers (block indices, ID maps, deferred metadata), guarded by boolean capability profiles. | Pettinau & Matheja (`arXiv:2601.15756`, TACAS 2026); `HoldSpec` (Oct 2026); Veanes et al. |
| **Coalgebraic Lenses & Galois Connections** | Model bidirectional protocol spokes as symmetric lenses with quotient equivalence (syntactic reshaping) and complement stashes (sideband retention of opaque tokens), with loss ordered on a bounded semilattice. | Niu & Spivak (*Polynomial Functors*, 2024–2026); Clarke; Abou-Saleh et al. |
| **Hub-and-Spoke Canonical IR (`LLM-Rosetta`)** | Factor protocol translation through a typed 9-part `ContentPart` model and 10-event `StreamEvent` schema, splitting each provider into orthogonal `ContentOps`, `MessageOps`, `ToolOps`, and `ConfigOps`. | Ding (`arXiv:2604.09360`, 2026); Open Responses spec (2026) |
| **Deterministic Translation Oracles (`isloss` / `ConvIR`)** | Project source and translated payloads/streams into a semantic conversation IR (`ConvIR`) and evaluate 27 preservation rules (`S01`–`S13`, `D01`–`D06`, `R01`–`R08`) to separate avoidable defects from target limitations. | Liu et al. (`arXiv:2611.15543`, Nov 2026) |

Empirical findings from `isloss` (`arXiv:2611.15543`) motivate why schema validation alone is insufficient:
- Across 685 completed request translations in five major translators (`LiteLLM`, `Vercel AI SDK v5/v7`, `LLM-Rosetta`, `@musistudio/llms`, `claude-code-proxy`), **18.5% (127/685)** passed target schema validation and returned `HTTP 200` while silently altering or dropping prompt, role, or tool information the target API could have represented natively.
- Across 558 `HTTP 200` translated reply streams from seven gateways/libraries replayed through official `openai-python` and `openai-node` stream accumulators, **36.4% (203/558)** raised unhandled SDK exceptions or corrupted the reassembled message; **98.0% (199/203)** of those failures were avoidable state-machine translation bugs. Python and Node accumulators disagreed on **129/558** streams.
- Dropping a single semantic flag (`is_error: true` on a failed tool result) reduced live task compliance across six tested models from **30/30** to **0/30**.

---

## 2. Theoretical Models and What They Buy Us

### 2.1 Symbolic Register Transducers (SRTs) over Infinite Alphabets

Classical Mealy machines ($\delta: Q \times \Sigma \to Q \times \Gamma^*$) assume finite alphabets. LLM SSE streams carry infinite-domain payloads (JSON argument fragments, call IDs, sequence numbers, opaque signatures, usage counters) and often arrive out of logical order.

An **SRT** models a stream converter as:

$$\mathcal{T} = (Q, q_0, F, R, \vec{r}_0, \Sigma_{\mathcal{D}}, \Gamma_{\mathcal{D}}, \Delta)$$

- **$Q$**: Finite control-flow phases (e.g., `Idle`, `OpenIdle`, `InReasoning`, `InText`, `InTool`, `Draining`, `Closed`).
- **$R = (r_1, \dots, r_k) \in \mathcal{D}^k$**: Mutable registers (`StreamContext`):
  - $r_{\text{block\_idx}} \in \mathbb{N}$: active block index.
  - $r_{\text{id\_map}} \in \text{Map}[\text{CallID}, \text{ToolMeta}]$: binds tool-call headers to subsequent argument deltas.
  - $r_{\text{arg\_buf}} \in \text{Map}[\text{CallID}, \text{String}]$: accumulates partial JSON arguments (or tracks whether any bytes were emitted so empty calls can emit `"{}"`).
  - $r_{\text{deferred}} \in \text{Queue}[\Gamma_{\mathcal{D}}]$: holds out-of-order terminal metadata (e.g., Gemini `finishReason` / `usageMetadata` arriving alongside or before final content).
  - $r_{\text{stash}} \in \text{Map}[\text{ItemID}, \text{OpaqueBlob}]$: retains cryptographic tokens for round-trip rehydration.
- **$\Delta$**: Guarded transitions $q \xrightarrow{\phi(\sigma, R) \;/\; R' := \text{upd}(\sigma, R),\; \vec{\gamma} := \text{emit}(\sigma, R)} q'$.

Two formal properties matter directly in code:
- **Totality (input-enabledness):** $\bigvee \phi_i(\sigma, R) \equiv \top$ in every reachable state $q$. Every unexpected chunk order either transitions cleanly or fails with an explicit typed protocol error rather than panicking or dropping data.
- **Determinism:** $\phi_i(\sigma, R) \land \phi_j(\sigma, R) \equiv \bot$ for $i \neq j$. Guards out of a state do not overlap.

### 2.2 Featured Transition Systems (FTS) for Protocol Variants

Rather than maintaining separate state machines for every upstream dialect variation, an **FTS** parameterizes transitions with a boolean **Capability Profile** $\mathcal{FM} = (\mathcal{F}, \Psi)$:

$$\Delta_{\text{FTS}} \subseteq Q \times \Phi(\Sigma_{\mathcal{D}}, R) \times \mathbb{B}(\mathcal{F}) \times \text{Upd}(R) \times \text{Emit}(\Gamma_{\mathcal{D}}) \times \mathcal{L}_{\text{loss}} \times Q$$

For any state $q$ and input $\sigma$, transitions follow a priority lattice:
1. **Primary ($\chi_1 = f_{\text{native}}$):** Emit the native construct with zero loss (`L0`).
2. **Coercion ($\chi_2 = \neg f_{\text{native}} \land f_{\text{coercible}}$):** Emit a structurally valid fallback (recorded as a policy-gated loss or note).
3. **Rejection/Drop ($\chi_3 = \neg f_{\text{native}} \land \neg f_{\text{coercible}}$):** Fail closed under strict policy, or drop with an explicit ledger entry if permitted.

Because $\chi_1 \lor \chi_2 \lor \chi_3 \equiv \top$, a single parameterized state machine covers multiple provider capability profiles without duplicating control flow.

### 2.3 Coalgebraic Symmetric Lenses & Galois Connections

In bidirectional API translation, neither source protocol $P_A$ nor target protocol $P_B$ is a strict subset of the other. Modeling a spoke $P_A \leftrightarrow \text{IR}$ as a **Quotient + Complement Coalgebraic Lens** explains two necessary mechanisms:

1. **Semantic Quotient Equivalence ($\equiv_{\text{ConvIR}}$):** Syntactic framing differences (top-level `system` vs. leading `system` message; splitting an assistant message with `content` + `tool_calls` into ordered parts) collapse to the same semantic conversation.
2. **Complement Stashing (`PutGet` with Residual):** When lowering an IR construct $u_{\text{IR}}$ to a weaker protocol $P_B$ that has no field for component $c$ (e.g., Anthropic `thinking.signature` or Gemini `thoughtSignature` when serving a Chat Completions client), `lower_B` stores $c_{\text{residual}}$ in a sideband state complement $x_B$. On turn $k+1$, `lift_B` recombines the incoming history with $x_B[k]$ so multi-turn tool loops do not fail with `HTTP 400`.

Viewing the weaker protocol $P_B$ through a **Galois Connection** ($\text{IR} \leftrightarrows P_B$) shows that a round-trip $\rho_B = \alpha_B \circ \gamma_B$ without a sideband stash is an *interior operator*: it is **contractive** ($\rho_B(u) \sqsubseteq_{\text{IR}} u$, never inventing information) and **idempotent** ($\rho_B(\rho_B(u)) = \rho_B(u)$, meaning a second pass through the same capability ceiling loses nothing further).

### 2.4 The 6-Tier Fidelity Degradation Semilattice

Every transition emits a weight in a bounded join-semilattice $(\mathcal{L}_{\text{fidelity}}, \sqcup, \text{L0})$:

| Level | Classification | Alignment with `isloss` / `LLM-Rosetta` | Example |
| :--- | :--- | :--- | :--- |
| **`L0`** | Lossless ($\equiv_s$) | `LLM-Rosetta` Preserve Mode | Plain user/assistant text or `temperature` mapped 1:1. |
| **`L1`** | Syntactic Normalization ($\equiv_{\text{ConvIR}}$) | `isloss` T3 (`R01`–`R08`) | Extracting leading `system` message to top-level `system`; splitting `content` + `tool_calls`. |
| **`L2`** | Stashed Lossless | Sideband `provider_metadata` / proxy store | Stashing `thinking.signature` or `thoughtSignature` across turns via a proxy store. |
| **`L3`** | Target Capability Degradation | `isloss` T2 (`D01`–`D06`) | Dropping `cache_control` or `top_k` when the target lacks support; recorded as an observable loss/warning. |
| **`L4`** | Avoidable Semantic Loss | `isloss` T1 (`S01`–`S13`) | Silently dropping `is_error: true` or demoting mid-turn `system` to `user` when the target supports it natively. |
| **`L5`** | Degenerate / Protocol-Fatal | Stream / SDK failure or multi-turn deadlock | Sparse `tool_calls[].index` crashing SDK accumulators, or dropping `thinking` blocks before turn $k+1$. |

---

## 3. Protocol Comparison & Reference IR Sketch

### 3.1 Structural Mismatches Across the Four Major Protocols

| Dimension | OpenAI Chat Completions | OpenAI Responses | Anthropic Messages | Google Gemini (`GenerateContent`) |
| :--- | :--- | :--- | :--- | :--- |
| **Paradigm** | Stateless message list; unbracketed delta chunks. | Hierarchical item and content-part state machine. | Bracketed content-block index state machine. | Candidate-parts array; chunks carry full `Part` objects. |
| **System Authority** | `system` or `developer` allowed anywhere in `messages[]`. | Top-level `instructions` plus `developer`/`system` items anywhere in `input[]`. | Top-level `system` only; `messages[]` strictly alternates `user`/`assistant`. | Top-level `systemInstruction` only; `contents[]` allows `user`/`model`. |
| **Reasoning Continuity** | No standard block (extensions like `reasoning_content`). | Typed `reasoning` items with `summary` and `encrypted_content` (or `previous_response_id`). | `thinking` (`thinking_delta` + `signature_delta`) and `redacted_thinking`; must echo verbatim in tool loops. | `Part` with `thought: true` and opaque `thoughtSignature` (preserve before coalescing). |
| **Tool Linking** | `tool_calls[]` on assistant; separate `role: "tool"` messages. No native `is_error`. | Top-level `function_call` and `function_call_output` items linked by `call_id`. | `tool_use` in `assistant`, `tool_result` in `user`, with explicit `is_error: bool`. | `functionCall` in `model`, `functionResponse` in `user`/`function`. |
| **Streaming Quirks** | No `block_start`/`block_stop`; optional usage chunk before `[DONE]`. | Strict lifecycle: `response.created` $\to$ `output_item.added` $\to$ `content_part.added` $\to$ `*.delta` $\to$ `*.done` $\to$ `response.completed`. | Strict bracketed lifecycle: `message_start` $\to$ (`content_block_start` $\to$ `delta`* $\to$ `stop`)* $\to$ `message_delta` $\to$ `message_stop`. | Frequently emits `finishReason` and `usageMetadata` on the same chunk as, or before, final content. |

### 3.2 Reference Canonical IR Data Model (Python Sketch)

The literature converges on a **9-type `ContentPart` model** (`TextPart`, `ImagePart`, `AudioPart`, `FilePart`, `ToolCallPart`, `ToolResultPart`, `ReasoningPart`, `RefusalPart`, `CitationPart`) and a **10-type `IRStreamEvent` schema**, paired with a `CapabilityProfile` and `FidelityLedger`. Below is the reference data model from the literature:

```python
from __future__ import annotations
from dataclasses import dataclass, field
from enum import Enum, IntEnum
from typing import Any, Literal, Optional, Union

class FidelityLevel(IntEnum):
    L0_LOSSLESS = 0             # Structural isomorphism (Preserve Mode)
    L1_SYNTACTIC_NORM = 1       # Permitted representation change (isloss T3: R01-R08)
    L2_STASHED_LOSSLESS = 2     # Preserved via sideband/metadata stash
    L3_TARGET_DEGRADATION = 3   # Unavoidable target API limitation (isloss T2: D01-D06)
    L4_AVOIDABLE_LOSS = 4       # Defect: Target could represent this, or silent loss (isloss T1: S01-S13)
    L5_DEGENERATE_FATAL = 5     # Protocol violation, SDK crash, or multi-turn deadlock

@dataclass(frozen=True)
class FidelityEvent:
    rule_id: str                # e.g., "S02_TOOL_ERROR_FLAG_LOST", "D01_CACHE_CONTROL_DROPPED"
    level: FidelityLevel
    avoidable: bool
    warned_caller: bool
    path: str
    message: str

@dataclass
class FidelityLedger:
    events: list[FidelityEvent] = field(default_factory=list)

    @property
    def max_level(self) -> FidelityLevel:
        return max((e.level for e in self.events), default=FidelityLevel.L0_LOSSLESS)

    def record(self, rule_id: str, level: FidelityLevel, avoidable: bool, path: str, msg: str, warned: bool = True) -> None:
        self.events.append(FidelityEvent(rule_id, level, avoidable, warned, path, msg))

    def merge(self, other: FidelityLedger) -> None:
        self.events.extend(other.events)

@dataclass(frozen=True)
class CapabilityProfile:
    provider_id: str
    supports_top_level_system: bool = True
    supports_mid_conversation_system: bool = False  # True for OpenAI Chat/Responses; False for Anthropic/Gemini
    supports_developer_role: bool = False           # True for OpenAI Chat/Responses
    supports_native_tool_error_flag: bool = False   # True for Anthropic/OpenResponses; False for classic OpenAI Chat
    supports_thinking_blocks: bool = False          # True for Anthropic, OpenAI Responses, Gemini
    requires_thinking_signature_echo: bool = False  # True for Anthropic tool-use loops
    requires_encrypted_reasoning_echo: bool = False # True for stateless OpenAI Responses reasoning loops
    requires_thought_signature_before_coalesce: bool = False # True for Gemini
    supports_citations_block: bool = False          # True for Anthropic, Google Grounding
    supports_refusal_block: bool = False            # True for OpenAI Chat/Responses, Anthropic
    supports_server_side_tools: bool = False        # True for OpenAI Responses / Anthropic MCP
    supports_cache_control: bool = False            # True for Anthropic, Bedrock
    supports_top_k: bool = False                    # True for Anthropic, Gemini; False for OpenAI
    requires_strict_role_alternation: bool = False  # True for Anthropic / certain Gemini endpoints

@dataclass
class BasePart:
    provider_metadata: dict[str, Any] = field(default_factory=dict)

@dataclass
class TextPart(BasePart):
    type: Literal["text"] = "text"
    text: str = ""

@dataclass
class ImagePart(BasePart):
    type: Literal["image"] = "image"
    source_type: Literal["base64", "url"] = "url"
    media_type: Optional[str] = None
    data_or_url: str = ""
    detail: Optional[Literal["auto", "low", "high"]] = None

@dataclass
class AudioPart(BasePart):
    type: Literal["audio"] = "audio"
    media_type: str = "audio/wav"
    data: str = ""
    transcript: Optional[str] = None

@dataclass
class FilePart(BasePart):
    type: Literal["file"] = "file"
    file_id_or_data: str = ""
    filename: Optional[str] = None
    media_type: Optional[str] = None

@dataclass
class ToolCallPart(BasePart):
    type: Literal["tool_call"] = "tool_call"
    tool_call_id: str = ""
    tool_name: str = ""
    tool_input: dict[str, Any] = field(default_factory=dict)
    raw_arguments_json: Optional[str] = None
    is_server_tool: bool = False

@dataclass
class ToolResultPart(BasePart):
    type: Literal["tool_result"] = "tool_result"
    tool_call_id: str = ""
    tool_name: Optional[str] = None
    content: list[Union[TextPart, ImagePart]] = field(default_factory=list)
    is_error: bool = False

@dataclass
class ReasoningPart(BasePart):
    type: Literal["reasoning"] = "reasoning"
    thinking_text: str = ""
    summary_text: Optional[str] = None
    is_redacted: bool = False
    signature: Optional[str] = None
    encrypted_content: Optional[str] = None

@dataclass
class RefusalPart(BasePart):
    type: Literal["refusal"] = "refusal"
    refusal_text: str = ""

@dataclass
class CitationPart(BasePart):
    type: Literal["citation"] = "citation"
    url: Optional[str] = None
    title: Optional[str] = None
    cited_text: Optional[str] = None
    start_index: Optional[int] = None
    end_index: Optional[int] = None

ContentPart = Union[
    TextPart, ImagePart, AudioPart, FilePart,
    ToolCallPart, ToolResultPart, ReasoningPart, RefusalPart, CitationPart
]

class StreamEventType(str, Enum):
    STREAM_START = "stream_start"
    STREAM_END = "stream_end"
    CONTENT_BLOCK_START = "content_block_start"
    CONTENT_BLOCK_END = "content_block_end"
    TEXT_DELTA = "text_delta"
    REASONING_DELTA = "reasoning_delta"
    TOOL_CALL_START = "tool_call_start"
    TOOL_CALL_DELTA = "tool_call_delta"
    FINISH = "finish"
    USAGE = "usage"

@dataclass
class IRStreamEvent:
    type: StreamEventType
    block_index: Optional[int] = None
    block_type: Optional[Literal["text", "reasoning", "tool_call", "refusal", "citation"]] = None
    text_delta: Optional[str] = None
    is_refusal_delta: bool = False
    reasoning_delta: Optional[str] = None
    signature_delta: Optional[str] = None
    encrypted_content_delta: Optional[str] = None
    tool_call_id: Optional[str] = None
    tool_name: Optional[str] = None
    tool_args_delta: Optional[str] = None
    finish_reason: Optional[Literal["stop", "length", "tool_calls", "content_filter", "error", "incomplete"]] = None
    usage: Optional[dict[str, int]] = None
    response_id: Optional[str] = None
    model: Optional[str] = None
    provider_metadata: dict[str, Any] = field(default_factory=dict)
```

### 3.3 The Canonical Mediating Stream Automaton (`StreamContext` & Transition Table)

To prevent the **36.4% translated reply stream failure rate** observed in `arXiv:2611.15543`, the mediating-hub design enforces a strict **Bracketed Block Lifecycle Invariant** over the 10 IR stream events. Every ingress spoke ($P_A \to \text{IR}$) normalizes raw provider chunks into a well-bracketed trace satisfying:

$$\mathcal{L}(\text{IRStream}) = \texttt{stream\_start} \cdot \Big( \texttt{Block}_{\text{text}} \;\cup\; \texttt{Block}_{\text{reasoning}} \;\cup\; \texttt{Block}_{\text{tool}} \Big)^* \cdot \texttt{finish} \cdot \texttt{usage}^? \cdot \texttt{stream\_end}$$

where:
- $\texttt{Block}_{\text{text}} = \texttt{content\_block\_start}(\text{text}, i) \cdot \texttt{text\_delta}(i)^* \cdot \texttt{content\_block\_end}(i)$
- $\texttt{Block}_{\text{reasoning}} = \texttt{content\_block\_start}(\text{reasoning}, i) \cdot \texttt{reasoning\_delta}(i)^* \cdot \texttt{content\_block\_end}(i)$
- $\texttt{Block}_{\text{tool}} = \texttt{content\_block\_start}(\text{tool\_call}, i) \cdot \texttt{tool\_call\_start}(i, \text{id}, \text{name}) \cdot \texttt{tool\_call\_delta}(i, \text{json\_frag})^* \cdot \texttt{content\_block\_end}(i)$

#### Four Stream Desynchronization Modes Addressed by `StreamContext`

1. **Unbracketed Source Streams (OpenAI Chat Completions & Gemini):** Chat Completions emits `delta.content` and `delta.tool_calls[k]` without explicit `block_start` or `block_end` events. On a phase shift from `delta.content` to `delta.tool_calls[0]`, `StreamContext` synthesizes `content_block_end(0)`, increments $r_{\text{block\_idx}} \leftarrow 1$, and synthesizes `content_block_start(tool_call, 1)` + `tool_call_start(1, id, name)` before emitting `tool_call_delta`.
2. **Premature `finishReason` / `usage` Chunks (Gemini & OpenAI):** Gemini frequently attaches `finishReason: "STOP"` and `usageMetadata` to the final content chunk. Emitting `finish` before closing the active content block causes downstream Anthropic or Responses parsers to fail. `StreamContext` captures premature `finish` and `usage` payloads in $r_{\text{deferred}}$, flushes all pending deltas, emits `content_block_end(i)`, and only then drains $r_{\text{deferred}}$ (`finish` $\to$ `usage` $\to$ `stream_end`).
3. **Empty Tool Call Arguments:** In Anthropic Messages, a zero-argument tool call emits `content_block_start` (`input: {}`) followed by `content_block_stop` with zero `input_json_delta` events. When lowered to Chat Completions without synthesizing `"{}"`, `openai-python` / `openai-node` accumulators leave `arguments: ""`, which fails `json.loads("")` in caller code. Tracking $r_{\text{arg\_emitted}}[i]$ lets the egress spoke emit `"{}"` on `content_block_end(i)` when zero bytes were emitted.
4. **Parallel Tool Call Index Collisions:** Chat Completions indexes tool calls densely (`tool_calls[0].index = 0`, `1`, ...), whereas Anthropic uses a global monotonic `index` across thinking, text, and tool blocks (`0: thinking`, `1: text`, `2: tool_use`, `3: tool_use`). Passing Anthropic's global index `2` directly into OpenAI `tool_calls[].index` causes `IndexError` or sparse `None` slots in `openai-python`. A dedicated map $r_{\text{tool\_ordinal}}: \text{IRBlockIdx} \mapsto \{0, 1, \dots\}$ compacts tool indices.

#### Formal SRT Transition Table

| Current State $q \in Q$ | Incoming Event / Condition $\phi(\sigma, R)$ | Register Update $R' := \text{upd}(\sigma, R)$ | Emitted Canonical IR Events $\vec{\gamma}$ | Next State $q'$ |
| :--- | :--- | :--- | :--- | :--- |
| `q0: IDLE` | First provider chunk $\sigma$ arrives | $r_{\text{id}} := \sigma.\text{id}$, $r_{\text{idx}} := -1$ | `[stream_start(id, model)]` (plus process $\sigma$ in `q1`) | `q1: OPEN_IDLE` |
| `q1: OPEN_IDLE` | $\sigma$ has `reasoning` / `thinking` delta | $r_{\text{idx}} \mathrel{+}= 1$ | `[content_block_start(r_idx, "reasoning"), reasoning_delta(r_idx, ...)]` | `q2: IN_REASONING` |
| `q1: OPEN_IDLE` | $\sigma$ has `text` or `refusal` delta | $r_{\text{idx}} \mathrel{+}= 1$ | `[content_block_start(r_idx, "text"), text_delta(r_idx, ...)]` | `q3: IN_TEXT` |
| `q1: OPEN_IDLE` | $\sigma$ has `tool_call` $(k, \text{id}, \text{name}, \text{frag})$ | $r_{\text{idx}} \mathrel{+}= 1$, $r_{\text{id\_map}}[k] := (\text{id}, \text{name}, r_{\text{idx}})$ | `[content_block_start(r_idx, "tool_call"), tool_call_start(r_idx, id, name), tool_call_delta(r_idx, frag)]` | `q4: IN_TOOL` |
| `q2: IN_REASONING` | $\sigma$ has `reasoning` or `signature` delta | Append signature to $r_{\text{stash}}$ if needed | `[reasoning_delta(r_idx, ...)]` | `q2: IN_REASONING` |
| `q2: IN_REASONING` | $\sigma$ switches to `text` delta | $i_{\text{old}} := r_{\text{idx}}; \; r_{\text{idx}} \mathrel{+}= 1$ | `[content_block_end(i_old), content_block_start(r_idx, "text"), text_delta(r_idx, ...)]` | `q3: IN_TEXT` |
| `q2: IN_REASONING` | $\sigma$ switches to `tool_call` $(k, \text{id}, \text{name}, \text{frag})$ | $i_{\text{old}} := r_{\text{idx}}; \; r_{\text{idx}} \mathrel{+}= 1; \; r_{\text{id\_map}}[k] := (\text{id}, \text{name}, r_{\text{idx}})$ | `[content_block_end(i_old), content_block_start(r_idx, "tool_call"), tool_call_start(...), tool_call_delta(...)]` | `q4: IN_TOOL` |
| `q3: IN_TEXT` | $\sigma$ has `text` delta | No register change | `[text_delta(r_idx, ...)]` | `q3: IN_TEXT` |
| `q3: IN_TEXT` | $\sigma$ switches to `tool_call` $(k, \text{id}, \text{name}, \text{frag})$ | $i_{\text{old}} := r_{\text{idx}}; \; r_{\text{idx}} \mathrel{+}= 1; \; r_{\text{id\_map}}[k] := (\text{id}, \text{name}, r_{\text{idx}})$ | `[content_block_end(i_old), content_block_start(r_idx, "tool_call"), tool_call_start(...), tool_call_delta(...)]` | `q4: IN_TOOL` |
| `q4: IN_TOOL` | $\sigma$ has `tool_call` delta for same key $k$ | $r_{\text{arg\_buf}}[k] \mathrel{+}= \text{frag}$ | `[tool_call_delta(r_idx, frag)]` | `q4: IN_TOOL` |
| `q4: IN_TOOL` | $\sigma$ has `tool_call` delta for new key $k' \neq k$ | $i_{\text{old}} := r_{\text{idx}}; \; r_{\text{idx}} \mathrel{+}= 1; \; r_{\text{id\_map}}[k'] := (\text{id}', \text{name}', r_{\text{idx}})$ | `[content_block_end(i_old), content_block_start(r_idx, "tool_call"), tool_call_start(r_idx, id', name'), tool_call_delta(r_idx, frag)]` | `q4: IN_TOOL` |
| `q2, q3, q4` | $\sigma$ contains `finish_reason` or `usage` alongside content | Push `finish` / `usage` into $r_{\text{deferred}}$ after content | Content delta events first; if $\sigma$ is also EOF, transition to `q5: DRAINING` | `q2, q3, q4` (or `q5`) |
| `q1..q4` | Stream EOF / `[DONE]` / `message_stop` / `response.completed` | If in `q4` and $r_{\text{arg\_buf}}[k] == \text{""}$, synthesize `"{}"`. Drain $r_{\text{deferred}}$ | `[content_block_end(r_idx)]` (if in `q2..q4`) + `[finish(reason), usage(u), stream_end()]` | `q6: CLOSED` |

#### Reference Egress Lowering (`StreamContext` + `CanonicalStreamTransducer`)

```python
@dataclass
class StreamContext:
    phase: str = "IDLE"
    response_id: str = "msg_rosetta_0"
    model: str = "unknown"
    active_block_index: int = -1
    active_block_type: Optional[str] = None
    active_tool_key: Optional[int | str] = None
    tool_key_to_meta: dict[int | str, tuple[str, str, int]] = field(default_factory=dict)
    ir_block_to_openai_tool_ordinal: dict[int, int] = field(default_factory=dict)
    next_openai_tool_ordinal: int = 0
    tool_arg_bytes_emitted: dict[int, int] = field(default_factory=dict)
    deferred_finish: Optional[IRStreamEvent] = None
    deferred_usage: Optional[IRStreamEvent] = None
    ledger: FidelityLedger = field(default_factory=FidelityLedger)

class CanonicalStreamTransducer:
    def __init__(self, target_profile: CapabilityProfile, ctx: StreamContext):
        self.profile = target_profile
        self.ctx = ctx

    def lower_to_openai_chat_sse(self, event: IRStreamEvent) -> list[dict]:
        chunks: list[dict] = []
        base_chunk = {
            "id": self.ctx.response_id,
            "object": "chat.completion.chunk",
            "model": self.ctx.model,
        }

        if event.type == StreamEventType.STREAM_START:
            self.ctx.response_id = event.response_id or self.ctx.response_id
            self.ctx.model = event.model or self.ctx.model
            base_chunk["id"] = self.ctx.response_id
            base_chunk["model"] = self.ctx.model
            chunks.append({**base_chunk, "choices": [{"index": 0, "delta": {"role": "assistant", "content": ""}, "finish_reason": None}]})

        elif event.type == StreamEventType.TEXT_DELTA:
            delta_field = "refusal" if (event.is_refusal_delta and self.profile.supports_refusal_block) else "content"
            if event.is_refusal_delta and not self.profile.supports_refusal_block:
                self.ctx.ledger.record(
                    "D04_REFUSAL_COERCED_TO_TEXT", FidelityLevel.L3_TARGET_DEGRADATION,
                    avoidable=False, path="delta.refusal", msg="Target lacks refusal field; coerced to content delta."
                )
            chunks.append({**base_chunk, "choices": [{"index": 0, "delta": {delta_field: event.text_delta or ""}, "finish_reason": None}]})

        elif event.type == StreamEventType.REASONING_DELTA:
            if self.profile.supports_thinking_blocks:
                chunks.append({**base_chunk, "choices": [{"index": 0, "delta": {"reasoning_content": event.reasoning_delta or ""}, "finish_reason": None}]})
            else:
                self.ctx.ledger.record(
                    "D02_THINKING_DROPPED_ON_TARGET", FidelityLevel.L3_TARGET_DEGRADATION,
                    avoidable=False, path="delta.reasoning", msg="Target OpenAI Chat dialect does not support reasoning deltas."
                )

        elif event.type == StreamEventType.TOOL_CALL_START:
            b_idx = event.block_index if event.block_index is not None else 0
            ordinal = self.ctx.next_openai_tool_ordinal
            self.ctx.ir_block_to_openai_tool_ordinal[b_idx] = ordinal
            self.ctx.next_openai_tool_ordinal += 1
            self.ctx.tool_arg_bytes_emitted[b_idx] = 0
            chunks.append({
                **base_chunk,
                "choices": [{
                    "index": 0,
                    "delta": {
                        "tool_calls": [{
                            "index": ordinal,
                            "id": event.tool_call_id,
                            "type": "function",
                            "function": {"name": event.tool_name, "arguments": ""}
                        }]
                    },
                    "finish_reason": None
                }]
            })

        elif event.type == StreamEventType.TOOL_CALL_DELTA:
            b_idx = event.block_index if event.block_index is not None else 0
            ordinal = self.ctx.ir_block_to_openai_tool_ordinal.get(b_idx, 0)
            frag = event.tool_args_delta or ""
            self.ctx.tool_arg_bytes_emitted[b_idx] = self.ctx.tool_arg_bytes_emitted.get(b_idx, 0) + len(frag)
            chunks.append({
                **base_chunk,
                "choices": [{
                    "index": 0,
                    "delta": {"tool_calls": [{"index": ordinal, "function": {"arguments": frag}}]},
                    "finish_reason": None
                }]
            })

        elif event.type == StreamEventType.CONTENT_BLOCK_END:
            b_idx = event.block_index if event.block_index is not None else -1
            if b_idx in self.ctx.ir_block_to_openai_tool_ordinal and self.ctx.tool_arg_bytes_emitted.get(b_idx, 0) == 0:
                ordinal = self.ctx.ir_block_to_openai_tool_ordinal[b_idx]
                chunks.append({
                    **base_chunk,
                    "choices": [{
                        "index": 0,
                        "delta": {"tool_calls": [{"index": ordinal, "function": {"arguments": "{}"}}]},
                        "finish_reason": None
                    }]
                })

        elif event.type == StreamEventType.FINISH:
            reason = event.finish_reason or "stop"
            if self.ctx.next_openai_tool_ordinal > 0 and reason == "stop":
                reason = "tool_calls"
            chunks.append({**base_chunk, "choices": [{"index": 0, "delta": {}, "finish_reason": reason}]})

        elif event.type == StreamEventType.USAGE and event.usage:
            chunks.append({**base_chunk, "choices": [], "usage": event.usage})

        return chunks
```

---

## 4. The 27-Rule `isloss` / `ConvIR` Preservation Catalogue

This taxonomy from `arXiv:2611.15543` is useful as a behavioral checklist regardless of how the translator is structured internally.

### Category T1: Critical Preservation Rules (`S01`–`S13`)

Violating any of `S01`–`S13` when the target `CapabilityProfile` supports the feature is an avoidable defect (`L4`); if it breaks SDK accumulators or multi-turn requests, it is fatal (`L5`).

| Rule | Protected Property | Invariant ($\text{ConvIR}(S)$ vs. $\text{ConvIR}(T)$) | Common Failure Mode |
| :--- | :--- | :--- | :--- |
| **`S01`** | Speaker Authority | Roles match across `{system, developer, user, assistant, tool}`. | Silently deleting mid-turn `system` or demoting it to `user` when target supports mid-turn `system`. |
| **`S02`** | Tool Error Flags | `is_error(e_S) == is_error(e_T)` for all tool results. | Stripping `is_error: true` without encoding an error marker when lowering to Chat/Gemini. |
| **`S03`** | Reasoning Content | Thinking text and `redacted` status preserved. | Dropping `thinking` / `redacted_thinking` blocks in multi-turn tool loops. |
| **`S04`** | Multimodal Payload | Image/Audio/File data, MIME type, and `detail` preserved. | Stringifying structured tool-result arrays or dropping `detail: "high"`. |
| **`S05`** | Event Presence | No silent turn/part drops (modulo permitted T3 coalescing). | Dropping empty-text assistant turns that carry metadata or trailing turns. |
| **`S06`** | Call $\leftrightarrow$ Result Bijection | Bipartite call-ID graph is isomorphic. | Regenerating a `tool_call_id` without updating the matching `tool_result`. |
| **`S07`** | Causal Order | Cross-role and boundary ordering preserved ($e_1 \prec_S e_2 \implies e_1 \prec_T e_2$). | Hoisting a mid-conversation `system` instruction to index 0, changing its scope. |
| **`S08`** | Generation Controls | `temperature`, `max_tokens`, `stop_sequences`, `tool_choice`, `parallel_tool_calls`, `response_format` match. | Overriding `tool_choice: "required"` to `"auto"` or replacing caller `max_tokens`. |
| **`S09`** | Cryptographic Tokens | `thinking.signature`, `thoughtSignature`, `encrypted_content` preserved verbatim (no truncation/forgery). | Forging placeholder signatures upstream or coalescing Gemini parts before checking `thoughtSignature`. |
| **`S10`** | Tool Names & Identity | Declared tool names and called names match (or use bijective reversible mapping). | Truncating/flattening namespaced tool names without reverse-mapping streamed calls. |
| **`S11`** | Tool JSON Schema | `properties`, `required`, `enum`, `additionalProperties`, and types preserved. | Dropping `required` or `enum` when converting schemas to Gemini `Schema`. |
| **`S12`** | Stop / Finish Reason | Bijective mapping (`end_turn`/`stop`, `max_tokens`/`length`, `tool_use`/`tool_calls`, `refusal`/`content_filter`). | Mapping `max_tokens` or `content_filter` to generic `"stop"`. |
| **`S13`** | Stream Reassembly | `ConvIR(Assemble(Stream_T)) == ConvIR(Reply_S)` and official SDK accumulators succeed with 0 exceptions. | Sparse `tool_calls[].index`, missing `"{}"` on empty tool args, or unread `delta.refusal`. |

### Category T2: Target Capability Degradations (`D01`–`D06`)

Recorded as `L3` when the target lacks native support; must be surfaced rather than silently dropped:
- **`D01`**: Prompt caching directives (`cache_control`) dropped.
- **`D02`**: Unsupported reasoning configuration (thinking budget/effort or reasoning blocks).
- **`D03`**: System-to-user role demotion (when target forbids mid-turn system roles, kept at its exact causal position).
- **`D04`**: Sampling/format parameter approximation (`top_k`, `frequency_penalty`, `logprobs`, or structured output fallback).
- **`D05`**: Usage accounting granularity (missing cache or reasoning token sub-breakdowns).
- **`D06`**: Model/service-tier/citation metadata loss.

### Category T3: Permitted Syntactic Normalizations (`R01`–`R08`)

Representations that change wire syntax while preserving 100% of `ConvIR` semantics (`L1`):
- **`R01`**: Bijective role renaming (`assistant` $\leftrightarrow$ `model`, leading `system` $\leftrightarrow$ top-level `system`).
- **`R02`**: Splitting Chat `assistant` (`content` + `tool_calls`) into ordered parts.
- **`R03`**: Merging adjacent same-speaker messages (`user` + `user`, or multiple `role: "tool"` into one `user` turn of `tool_result` blocks) while preserving order.
- **`R04`**: Deterministic bijective re-indexing of `tool_call_id` for regex-restricted targets (preserving `S06`).
- **`R05`**: Wrapping string content into single-element text arrays.
- **`R06`**: Normalizing JSON key ordering/whitespace in tool arguments.
- **`R07`**: Converting between Responses item lists and Chat/Messages block containers.
- **`R08`**: Synthesizing required wire defaults (e.g., `max_tokens` when omitted by a source dialect), recorded observably.

---

## 5. Formal Verification, Model-Based Testing, & Active Automata Learning

The literature organizes verification into a **Four-Layer Verification Pyramid** to check completeness, deadlock freedom, and semantic preservation without relying solely on live LLM calls:

```
                  +---------------------------------------------------+
                  | Layer 4: Active Automata Learning (AALpy / L*)    |
                  | Infers black-box vendor Mealy models & diffs spec |
                  +---------------------------------------------------+
                +-------------------------------------------------------+
                | Layer 3: Differential Official SDK Stream Oracle      |
                | Replays translated SSE through openai-python/node &   |
                | anthropic SDK accumulators + checks ConvIR S13        |
                +-------------------------------------------------------+
              +-----------------------------------------------------------+
              | Layer 2: Stateful Property-Based & Model-Based Testing    |
              | Hypothesis RuleBasedStateMachine / proptest-state-machine |
              | + isloss 27-Rule Translation Validation Oracle            |
              +-----------------------------------------------------------+
            +---------------------------------------------------------------+
            | Layer 1: Formal Spec in Quint / TLA+ & Z3 Guard Totality      |
            | Proves FTS deadlock freedom & bracketed grammar invariants    |
            +---------------------------------------------------------------+
```

### 5.1 Layer 1: Formal Specification in Quint / TLA+

Following the Quint / TLA+ workflow (`HoldSpec`, Oct 2026), the canonical stream state machine can be specified in **Quint** (an executable, typed specification language over TLA+ logic) parameterized by `CapabilityProfile`:

```quint
module ProtocolStreamMediator {
  type BlockType = NoneBlock | TextBlock | ReasoningBlock | ToolBlock
  type Phase = Idle | OpenIdle | InBlock | Draining | Closed

  type State = {
    phase: Phase,
    active_block: BlockType,
    block_idx: int,
    open_brackets: Set[int],
    closed_brackets: Set[int],
    deferred_finish: bool,
    emitted_finish: bool,
    loss_level: int
  }

  var s: State

  action init = all {
    s' = {
      phase: Idle, active_block: NoneBlock, block_idx: -1,
      open_brackets: Set(), closed_brackets: Set(),
      deferred_finish: false, emitted_finish: false, loss_level: 0
    }
  }

  action enter_block(target_type: BlockType): bool = all {
    s.phase == OpenIdle or s.phase == InBlock,
    not(s.emitted_finish),
    val next_idx = s.block_idx + 1
    val newly_closed = if (s.phase == InBlock) s.closed_brackets.union(Set(s.block_idx)) else s.closed_brackets
    s' = {
      ...s,
      phase: InBlock,
      active_block: target_type,
      block_idx: next_idx,
      open_brackets: s.open_brackets.union(Set(next_idx)),
      closed_brackets: newly_closed
    }
  }

  action receive_premature_finish: bool = all {
    s.phase == InBlock,
    s' = { ...s, deferred_finish: true }
  }

  action close_stream: bool = all {
    s.phase == OpenIdle or s.phase == InBlock,
    val final_closed = if (s.phase == InBlock) s.closed_brackets.union(Set(s.block_idx)) else s.closed_brackets
    s' = {
      ...s,
      phase: Closed,
      active_block: NoneBlock,
      closed_brackets: final_closed,
      deferred_finish: false,
      emitted_finish: true
    }
  }

  // Every closed bracket was opened, and at most 1 bracket is unclosed
  val BracketBalanceInvariant =
    s.closed_brackets.subseteq(s.open_brackets) and
    (s.phase == Closed implies s.open_brackets == s.closed_brackets)

  // Finish event is never emitted while a content block remains open
  val NoFinishWhileBlockOpenInvariant =
    s.emitted_finish implies (s.open_brackets == s.closed_brackets)
}
```

Running `quint verify --invariant=BracketBalanceInvariant,NoFinishWhileBlockOpenInvariant` model-checks interleavings of content deltas, tool calls, and out-of-order finish chunks, and `quint test --output=traces.itf.json` exports traces to drive model-based tests against production code.

### 5.2 Layer 2 & Layer 3: Stateful Property-Based Testing + Differential SDK Oracle

In Python (`hypothesis.stateful.RuleBasedStateMachine`) or Go (`testing.F` / rapid), a stateful harness generates arbitrary valid sequences of multi-block streaming events (reasoning, text, parallel tool calls, refusals, premature finish chunks) and asserts:
1. **Bracket balance** on the emitted stream.
2. **Stream-reassembly equivalence (`S13`):** `ConvIR(Assemble(TranslateStream(S))) == ConvIR(TranslateBatch(Assemble(S)))`.
3. **Official SDK accumulator survival:** Replaying emitted SSE chunks through `openai-python`, `openai-node`, and `@anthropic-ai/sdk` accumulators produces zero unhandled exceptions and valid JSON tool arguments.
4. **Fidelity bounds:** Zero unwarned or avoidable `T1` (`L4`/`L5`) losses in the ledger.

```python
import json
from hypothesis.stateful import RuleBasedStateMachine, rule, invariant, initialize
from hypothesis import strategies as st

class TranscoderVerificationStateMachine(RuleBasedStateMachine):
    @initialize()
    def init_transcoder(self):
        self.profile = CapabilityProfile(
            provider_id="openai_chat",
            supports_thinking_blocks=False,
            supports_refusal_block=True,
        )
        self.ctx = StreamContext(response_id="chatcmpl_test", model="gpt-4o")
        self.transducer = CanonicalStreamTransducer(self.profile, self.ctx)
        self.emitted_sse_chunks: list[dict] = []
        self.current_block_idx = -1
        self.in_block = False
        self._step(IRStreamEvent(type=StreamEventType.STREAM_START, response_id="chatcmpl_test", model="gpt-4o"))

    def _step(self, ev: IRStreamEvent):
        self.emitted_sse_chunks.extend(self.transducer.lower_to_openai_chat_sse(ev))

    @rule(text=st.text(min_size=1, max_size=50))
    def emit_text_block(self, text: str):
        if self.ctx.phase == "CLOSED":
            return
        if self.in_block:
            self._step(IRStreamEvent(type=StreamEventType.CONTENT_BLOCK_END, block_index=self.current_block_idx))
        self.current_block_idx += 1
        self.in_block = True
        self._step(IRStreamEvent(type=StreamEventType.CONTENT_BLOCK_START, block_index=self.current_block_idx, block_type="text"))
        self._step(IRStreamEvent(type=StreamEventType.TEXT_DELTA, block_index=self.current_block_idx, text_delta=text))

    @rule(tool_name=st.sampled_from(["get_weather", "run_cmd", "search_db"]),
          args_dict=st.dictionaries(st.text(min_size=1, max_size=8), st.integers()))
    def emit_tool_call_block(self, tool_name: str, args_dict: dict):
        if self.ctx.phase == "CLOSED":
            return
        if self.in_block:
            self._step(IRStreamEvent(type=StreamEventType.CONTENT_BLOCK_END, block_index=self.current_block_idx))
        self.current_block_idx += 1
        self.in_block = True
        call_id = f"call_{self.current_block_idx}"
        self._step(IRStreamEvent(type=StreamEventType.CONTENT_BLOCK_START, block_index=self.current_block_idx, block_type="tool_call"))
        self._step(IRStreamEvent(type=StreamEventType.TOOL_CALL_START, block_index=self.current_block_idx, tool_call_id=call_id, tool_name=tool_name))
        raw_json = json.dumps(args_dict) if args_dict else ""
        if raw_json:
            mid = len(raw_json) // 2
            self._step(IRStreamEvent(type=StreamEventType.TOOL_CALL_DELTA, block_index=self.current_block_idx, tool_args_delta=raw_json[:mid]))
            self._step(IRStreamEvent(type=StreamEventType.TOOL_CALL_DELTA, block_index=self.current_block_idx, tool_args_delta=raw_json[mid:]))

    @invariant()
    def verify_sdk_accumulator_and_isloss_invariants(self):
        assembled_tools: dict[int, dict] = {}
        for chunk in self.emitted_sse_chunks:
            for choice in chunk.get("choices", []):
                delta = choice.get("delta", {})
                for tc in delta.get("tool_calls", []):
                    idx = tc["index"]
                    assert idx <= len(assembled_tools), f"Sparse tool_call index {idx} crashes SDK accumulators"
                    if idx not in assembled_tools:
                        assembled_tools[idx] = {"id": tc.get("id"), "name": tc["function"].get("name"), "arguments": ""}
                    assembled_tools[idx]["arguments"] += tc["function"].get("arguments", "")

        if not self.in_block:
            for tc_data in assembled_tools.values():
                parsed = json.loads(tc_data["arguments"])
                assert isinstance(parsed, dict)

        assert self.ctx.ledger.max_level < FidelityLevel.L4_AVOIDABLE_LOSS
```

### 5.3 Layer 4: Active Automata Learning (`AALpy` / $L^*$)

Vendor documentation often omits edge-case transitions (e.g., refusal mid-tool-call, or hosted compatibility layers handling mid-conversation developer turns). Using **`AALpy`** (Angluin's $L^*$ / KV algorithms for Mealy machines), a verification harness treats a black-box endpoint or SDK accumulator as a System Under Learning:
1. Define an abstract input alphabet $\Sigma_{\text{abstract}} = \{\texttt{SysLead}, \texttt{SysMid}, \texttt{UserText}, \texttt{AsstThink}, \texttt{AsstToolCall}, \texttt{ToolResultOk}, \texttt{ToolResultErr}\}$.
2. Issue membership queries and classify responses as `HTTP 200 (Preserved)`, `HTTP 200 (Silent Loss)` via distinguishing conversation probes (`arXiv:2611.15543` Sec. 6.5), or `HTTP 400 (Rejected)`.
3. Diff the learned Mealy machine $\mathcal{M}_{\text{learned}}$ against the spoke's `CapabilityProfile` projection $\mathcal{T}|_{p_{\text{vendor}}}$ to detect capability drift between vendor behavior and proxy assumptions.

---

## 6. Engineering Tradeoffs vs. the Academic Blueprint

Applying the research directly without adaptation creates friction with production proxy requirements. The table below contrasts the academic ideal with the pragmatic choices made in this codebase (and the tradeoffs of each):

| Dimension | Academic Blueprint | Pragmatic Proxy Choice & Tradeoff |
| :--- | :--- | :--- |
| **Streaming Architecture: Hub vs. Pairwise** | **Strict Hub-and-Spoke:** Forbid all pairwise stream translators; route every chunk through a 10-event `IRStreamEvent` hub ($2N$ spokes). | **Canonical IR for Requests/Turns + Pairwise Stream FSMs:** [ir.go](file:///Users/joeyc/dev/ai-concurrency-shaper/internal/transcode/ir.go) centralizes requests and turns, while streaming uses direct dialect-to-dialect converters (e.g. [chatstreamstate.go](file:///Users/joeyc/dev/ai-concurrency-shaper/internal/transcode/chatstreamstate.go)). For $N=3\text{–}4$ dialects, a lowest-common-denominator stream hub adds buffering latency, double allocations, and obscures dialect-specific invariants (Responses' item/part hierarchy, Gemini's candidate chunks). Pairwise stream FSMs sharing common SSE/budget/lifecycle primitives are faster and simpler to bound, at the cost of $O(N^2)$ stream state machines if $N$ grows large. |
| **Schema Strictness: Uniform vs. Role-Split** | **Closed-World `FidelityLedger`:** Every unmodeled field is routed through the capability profile and ledger. | **Role 1 (Strict Client) vs. Role 2 (Tolerant Upstream):** Real upstreams freely inject envelope extensions (`completion_cost`, `cache_cost`, `native_finish_reason`). Rejecting or logging every unknown upstream field breaks availability. Per [AGENTS.md](file:///Users/joeyc/dev/ai-concurrency-shaper/AGENTS.md), client request decode is strict (`DisallowUnknownFields`), while upstream envelope decode is tolerant (`wire.DecodeTolerant`) with strict union arms on content-bearing blocks. |
| **Upstream Defects: Reject vs. Absorb-and-Note** | **Pure `ConvIR` Equivalence:** Any discrepancy in usage totals or missing fields is a violation. | **Absorb Truthfully + Record `Note`:** Real gateways routinely emit `total_tokens != input + output` or omit one component. Failing the exchange triggers client retry storms; [losses.go](file:///Users/joeyc/dev/ai-concurrency-shaper/internal/transcode/losses.go) absorbs/derives arithmetic inconsistencies (`UsageTotalMismatch`, `UsageTotalDerived`) as non-policy `Note`s so the client receives truthful values with full log visibility. |
| **Complement Stashing: Idealized vs. Bounded** | **Unbounded Lens Complement $X_B$:** Assumes `PutGet` can always rehydrate prior-turn opaque state. | **Bounded In-Memory Stores + Graceful Miss:** Unbounded caches are OOM/DoS vectors. [ContinuityStore](file:///Users/joeyc/dev/ai-concurrency-shaper/internal/transcode/continuity.go) (`1024` chains, `30m` TTL, max depth `32`) and [GeminiSignatureStore](file:///Users/joeyc/dev/ai-concurrency-shaper/internal/transcode/geminisignatures.go) (`4096` entries, `1h` TTL, `64 KiB` cap) bound memory and degrade explicitly on miss (via `previous_response_id` loss or the documented `skip_thought_signature_validator` placeholder). |
| **Signature Forgery (`S09`) vs. Client UX** | **Strict No-Forgery:** Never synthesize a signature. | **Marker + Scrubbing Contract:** Under `provider_reasoning_thinking`, the proxy may synthesize Anthropic `thinking` blocks using `SyntheticThinkingSignature` for the client, then scrubs marker-signature blocks out of replayed history before any upstream render—never sending a synthetic signature upstream. |
| **Verification Tooling: Polyglot Formal vs. Native Go** | **4-Layer Pyramid:** Quint/TLA+, Z3, Hypothesis/Proptest, `AALpy` ($L^*$), plus live Python/Node SDKs in CI. | **Go Fuzzing + Scenario Guard Rails + Offline SDK Checks:** Pulling Z3, Quint, Python, and Node SDKs into Go CI adds heavy polyglot friction (and `AGENTS.md` forbids official SDKs as committed dependencies). Instead, native Go `testing.F` fuzzers ([state_machine_fuzz_test.go](file:///Users/joeyc/dev/ai-concurrency-shaper/internal/transcode/state_machine_fuzz_test.go)), FSM regression tests ([stream_fsm_test.go](file:///Users/joeyc/dev/ai-concurrency-shaper/internal/transcode/stream_fsm_test.go)), and loss-reachability tests ([loss_scenarios_registry_test.go](file:///Users/joeyc/dev/ai-concurrency-shaper/internal/transcode/loss_scenarios_registry_test.go)) run in milliseconds; `S13` stream-vs-batch equivalence can be added directly to the Go fuzzers, while official SDK replay stays a manual/scratch conformance check. |

---

## 7. Proxy & Gateway Concerns Beyond the Literature

The academic model treats protocol transcoding as a **stateless, header-agnostic payload transformation**. A stealth reverse proxy sitting between arbitrary clients and real LLM providers must also handle:

- **Client Impersonation & Sticky Session Emulation ([opencode.go](file:///Users/joeyc/dev/ai-concurrency-shaper/internal/transcode/opencode.go)):** Upstream providers such as `opencode` fingerprint clients via `User-Agent`, `x-opencode-client`, `x-opencode-session`, and foreign SDK headers (`x-stainless-*`, `x-app`). `OpencodePreset` normalizes outbound headers to a single consistent branch (deleting `x-session-affinity` / `X-Session-Id` so both branches never appear together) and strips foreign SDK headers only when they originated from the client. When an arbitrary client sends no session header, `DeriveConversationKey` hashes a domain-separated immutable prefix (`provider`, `model`, `firstUserText`) so multi-turn tool loops remain sticky to the same upstream session without server-side session state.
- **Concurrency Shaping, Circuit Breaking, & Dialect-Safe Keepalives:** Admission holds route and global concurrency slots across the full exchange (including accounting-lag holds and `HALF_OPEN` probes). SSE queue comments (`-queue-comments`) keep clients alive while waiting, but must be suppressed on Gemini paths because the Gemini Python SDK raises on SSE comment lines.
- **Tool Namespace Flattening & Replay Adjudication ([namespacetools.go](file:///Users/joeyc/dev/ai-concurrency-shaper/internal/transcode/namespacetools.go), [requestfeatures.go](file:///Users/joeyc/dev/ai-concurrency-shaper/internal/transcode/requestfeatures.go)):** Mapping Responses namespaced tools onto flat dialects requires collision-free flattening, `tool_choice` disambiguation (`tool_choice_namespace_bind`), and handling undeclared-namespace history replays (`namespace_replay_undeclared`, e.g., during client auto-compaction).
- **Adversarial Resource Bounds ([stream_budget.go](file:///Users/joeyc/dev/ai-concurrency-shaper/internal/transcode/stream_budget.go)):** Enforcing 7-dimension stream budgets (defending against empty-part allocation attacks and oversized frames) and keeping accepted-request, retry-replay, decoded-request, response, and error-body limits separate.
- **Registration-Order-Independent Multi-Mapping Dispatch:** Routing a single client route (e.g., `POST /v1/messages`) across multiple upstream dialects (`chat`, `responses`, `gemini`) deterministically regardless of flag registration order.
