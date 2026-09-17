package transcode

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// ContinuityStore is the opt-in bounded per-conversation store behind the
// statefulness decision: OFF by default, enabled only by an explicit flag,
// and its miss path always degrades to the existing observable
// previous_response_id loss — never a hard failure, never fabricated
// history. It is keyed ONLY by response ids the proxy itself emitted under
// the same provider mapping (mapping key + response id); an unknown,
// evicted, or foreign id is a miss, not an error.
//
// Memory is bounded by capacity (max retained chains) and TTL (max chain
// age); eviction is observable through the per-request log lines the caller
// emits (the store reports what it evicted). All methods are safe for
// concurrent use by interleaved exchanges.
type ContinuityStore struct {
	mu       sync.Mutex
	chains   map[string]*continuityChain
	order    []string
	capacity int
	ttl      time.Duration
	now      func() time.Time
}

// continuityChain is one retained conversation: the canonical turns that
// produced the recorded response id, plus the id chain for cycle detection.
type continuityChain struct {
	// turns holds the full reconstructed conversation through the recorded
	// response: the resolved prior turns followed by the request's own
	// turns and the assistant turns the response rendered.
	turns []CanonicalTurn
	// depth is the number of store resolutions that built this chain.
	depth int
	// updated is the last write time, for TTL expiry.
	updated time.Time
}

// ContinuityConfig bounds the store. Zero values select the documented
// defaults; negative values are rejected at flag validation, never here.
type ContinuityConfig struct {
	Capacity int
	TTL      time.Duration
}

// DefaultContinuityCapacity bounds the retained chains when the operator
// enables the store without an explicit capacity.
const DefaultContinuityCapacity = 1024

// DefaultContinuityTTL bounds chain age when the operator enables the store
// without an explicit TTL.
const DefaultContinuityTTL = 30 * time.Minute

// MaxContinuityDepth bounds chain resolution: a longer chain is truncated at
// resolution with the truncation recorded, never followed unboundedly.
const MaxContinuityDepth = 32

// NewContinuityStore returns an empty store with the given bounds. A
// non-positive capacity selects DefaultContinuityCapacity; a non-positive
// TTL selects DefaultContinuityTTL.
func NewContinuityStore(config ContinuityConfig) *ContinuityStore {
	capacity := config.Capacity
	if capacity <= 0 {
		capacity = DefaultContinuityCapacity
	}
	ttl := config.TTL
	if ttl <= 0 {
		ttl = DefaultContinuityTTL
	}
	return &ContinuityStore{
		chains:   make(map[string]*continuityChain),
		capacity: capacity,
		ttl:      ttl,
		now:      time.Now,
	}
}

// continuityKey scopes a response id to the provider mapping that emitted
// it: the same id shape under a different mapping is a different chain (a
// foreign id is a miss, never a blend).
func continuityKey(mappingKey, responseID string) string {
	return mappingKey + "\x00" + responseID
}

// Record stores the canonical conversation that produced responseID under
// the emitting mapping. Turns are copied by slice header only: canonical
// parts are immutable after decode, and the store never mutates them.
// Recording an empty turn list is a no-op. When storeFalse is set (the
// client sent store:false) nothing is retained.
func (s *ContinuityStore) Record(mappingKey, responseID string, turns []CanonicalTurn, depth int, storeFalse bool) {
	if s == nil || responseID == "" || len(turns) == 0 || storeFalse {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := continuityKey(mappingKey, responseID)
	if _, ok := s.chains[key]; !ok {
		s.order = append(s.order, key)
	}
	copied := make([]CanonicalTurn, len(turns))
	copy(copied, turns)
	s.chains[key] = &continuityChain{turns: copied, depth: depth, updated: s.now()}
	s.evictLocked()
}

// RecordEvicted stores like Record and reports how many chains eviction
// dropped, so the handler can log the bound working on the exchange that
// triggered it. A nil store reports zero.
func (s *ContinuityStore) RecordEvicted(mappingKey, responseID string, turns []CanonicalTurn, depth int, storeFalse bool) int {
	if s == nil || responseID == "" || len(turns) == 0 || storeFalse {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := continuityKey(mappingKey, responseID)
	if _, ok := s.chains[key]; !ok {
		s.order = append(s.order, key)
	}
	copied := make([]CanonicalTurn, len(turns))
	copy(copied, turns)
	s.chains[key] = &continuityChain{turns: copied, depth: depth, updated: s.now()}
	return s.evictLocked()
}

// Resolve returns the retained turns for responseID under the same mapping,
// newest-to-oldest cycle-safe and depth-bounded. The second return is the
// recorded depth. ok is false for an unknown, expired, or foreign id — the
// caller must degrade to the existing observable loss, never fail.
func (s *ContinuityStore) Resolve(mappingKey, responseID string) (turns []CanonicalTurn, depth int, ok bool) {
	if s == nil || responseID == "" {
		return nil, 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := continuityKey(mappingKey, responseID)
	chain, found := s.chains[key]
	if !found {
		return nil, 0, false
	}
	if s.ttl > 0 && s.now().Sub(chain.updated) > s.ttl {
		s.removeLocked(key)
		return nil, 0, false
	}
	copied := make([]CanonicalTurn, len(chain.turns))
	copy(copied, chain.turns)
	return copied, chain.depth, true
}

// evictLocked enforces capacity (oldest-first) and TTL expiry. Callers hold
// s.mu. It returns the number of chains dropped, so the caller can surface
// eviction through the per-request log.
func (s *ContinuityStore) evictLocked() int {
	dropped := 0
	now := s.now()
	// TTL first: drop every expired chain.
	for key, chain := range s.chains {
		if s.ttl > 0 && now.Sub(chain.updated) > s.ttl {
			s.removeLocked(key)
			dropped++
		}
	}
	// Capacity: drop oldest-inserted chains first.
	for len(s.chains) > s.capacity {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.chains, oldest)
		dropped++
	}
	return dropped
}

// removeLocked deletes one chain and its order entry. Callers hold s.mu.
func (s *ContinuityStore) removeLocked(key string) {
	delete(s.chains, key)
	for i, k := range s.order {
		if k == key {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// Capacity returns the configured chain bound, for startup logging.
func (s *ContinuityStore) Capacity() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.capacity
}

// TTL returns the configured chain-age bound, for startup logging.
func (s *ContinuityStore) TTL() time.Duration {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ttl
}

// Len returns the retained chain count, for tests and observability.
func (s *ContinuityStore) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.chains)
}

// resolveContinuityChain resolves a previous_response_id against the store,
// following one link only: the recorded chain already holds the FULL
// reconstructed conversation through that response (Record stores the
// assembled turns), so depth cannot cycle and resolution is O(1). The
// returned depth is the recorded depth + 1, capped at MaxContinuityDepth;
// ok is false when the id is unknown, expired, or foreign.
func resolveContinuityChain(store *ContinuityStore, mappingKey, responseID string) ([]CanonicalTurn, int, bool) {
	turns, depth, ok := store.Resolve(mappingKey, responseID)
	if !ok {
		return nil, 0, false
	}
	if depth+1 > MaxContinuityDepth {
		trimmed := make([]CanonicalTurn, len(turns))
		copy(trimmed, turns)
		return trimmed, MaxContinuityDepth, true
	}
	return turns, depth + 1, true
}

// continuityNote records the resolution outcome: a hit Note naming the
// resolved depth, so the reconstructed history is observable on every
// exchange that receives it.
func continuityNote(report *ConversionReport, depth int, responseID string) error {
	return report.Note(
		FeaturePreviousResponseID,
		"request.previous_response_id",
		fmt.Sprintf(
			"previous_response_id %q resolved against the continuity store (depth %d); the reconstructed turns precede the request input",
			responseID,
			depth,
		),
	)
}

// responseItemsToTurns rebuilds the canonical request turns a response
// continues: the assistant turns the response rendered. Message items become
// assistant turns carrying their parts; function-call items fold into the
// adjacent assistant turn exactly the way request decode folds replayed
// calls (appendFunctionCallTurn); function-result items are conversation
// state for the NEXT request, not model output, and are skipped; reasoning
// items are provider artifacts, skipped the same way
// RequirePortableArtifacts treats them at render. Thinking parts are
// provider plaintext reasoning synthesized under the marker signature:
// replayed history must not carry them back upstream (the request path
// scrubs marker blocks, but only for Messages-sourced thinking). Model-
// generated thinking is ephemeral reasoning, not conversation content: drop
// it here so the retained chain renders on every target. A message left
// with no portable parts is skipped. A response with no portable turns
// yields nil.
func responseItemsToTurns(response CanonicalResponse) []CanonicalTurn {
	var turns []CanonicalTurn
	for _, item := range response.Items {
		switch value := item.(type) {
		case *CanonicalMessageItem:
			// Thinking parts are provider plaintext reasoning synthesized
			// under the marker signature: replayed history must not carry
			// them back upstream (the request path scrubs marker blocks,
			// but only for Messages-sourced thinking). Model-generated
			// thinking is ephemeral reasoning, not conversation content:
			// drop it here so the retained chain renders on every target.
			// A message left with no portable parts is skipped.
			var parts []CanonicalPart
			for _, part := range value.Parts {
				if _, ok := part.(CanonicalThinkingPart); ok {
					continue
				}
				parts = append(parts, part)
			}
			if len(parts) == 0 {
				continue
			}
			turns = append(turns, CanonicalTurn{Role: CanonicalAssistant, Parts: parts})
		case *CanonicalFunctionCallItem:
			part := CanonicalFunctionCall{
				CallID:    value.CallID,
				Name:      value.Name,
				Arguments: json.RawMessage(value.Arguments.Raw),
			}
			if len(turns) > 0 && turns[len(turns)-1].Role == CanonicalAssistant {
				turns[len(turns)-1].Parts = append(turns[len(turns)-1].Parts, part)
			} else {
				turns = append(turns, CanonicalTurn{Role: CanonicalAssistant, Parts: []CanonicalPart{part}})
			}
		case *CanonicalFunctionResultItem:
			continue
		case *CanonicalReasoningItem:
			continue
		default:
			continue
		}
	}
	return turns
}

// recordContinuityFromEnvelope retains the canonical conversation that
// produced the emitted Responses id, keyed by the client-visible id: the
// Responses envelope id minted inside RenderResponsesResponse (context.IDs),
// read back from the converted bytes. response.ID is the upstream chat id
// and must never key the store — the client only ever sends back the
// emitted resp_ id. A converted body whose envelope id cannot be read back
// retains nothing (fail-closed, never a wrong key). When eviction drops
// chains, the per-request log surfaces the count so the operator sees the
// bound working on the exchange that triggered it.
func (h *TranscodeHandler) recordContinuityFromEnvelope(r *http.Request, context *ExchangeContext, response CanonicalResponse, converted []byte) {
	store := h.cfg.Continuity
	if store == nil || context == nil || len(converted) == 0 {
		return
	}
	var envelope struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(converted, &envelope); err != nil || envelope.ID == "" {
		return
	}
	turns := make([]CanonicalTurn, 0, len(context.RequestTurns)+2)
	turns = append(turns, context.RequestTurns...)
	turns = append(turns, responseItemsToTurns(response)...)
	if len(turns) == 0 {
		return
	}
	storeFalse := false
	if echo := context.OriginalResponsesRequest; echo != nil && echo.Store != nil {
		storeFalse = !*echo.Store
	}
	if dropped := store.RecordEvicted(h.cfg.ContinuityKey, envelope.ID, turns, context.RequestDepth, storeFalse); dropped > 0 {
		h.logRequestError(r, fmt.Errorf("[local_response_conversion_error] continuity store evicted %d chain(s) at capacity/TTL bound", dropped))
	}
}
