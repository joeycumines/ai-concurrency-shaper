// Copyright (C) 2026 Joseph Cumines
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package transcode

import (
	"sync"
	"time"
)

// GeminiSignatureStore is the bounded per-provider store of thought
// signatures a Gemini upstream attached to function calls. Gemini 3 requires
// the signature to be sent back with the call in later turns, so without
// retention a multi-turn tool conversation breaks; the store is what keeps it
// working. Memory is bounded by capacity (max retained signatures) and TTL
// (max signature age); eviction drops the oldest entry first. All methods are
// safe for concurrent use by interleaved exchanges, and a nil store behaves
// as an empty store (programmatic callers never need to special-case it).
type GeminiSignatureStore struct {
	mu       sync.Mutex
	entries  map[string]*geminiSignatureEntry
	order    []string
	capacity int
	ttl      time.Duration
	now      func() time.Time
}

// geminiSignatureEntry is one retained signature.
type geminiSignatureEntry struct {
	signature string
	updated   time.Time
}

// GeminiSignatureConfig bounds the store. Zero values select the documented
// defaults; negative values are rejected at flag validation, never here.
type GeminiSignatureConfig struct {
	Capacity int
	TTL      time.Duration
}

// DefaultGeminiSignatureCapacity bounds the retained signatures when the
// store is created without an explicit capacity.
const DefaultGeminiSignatureCapacity = 4096

// DefaultGeminiSignatureTTL bounds signature age when the store is created
// without an explicit TTL.
const DefaultGeminiSignatureTTL = time.Hour

// maxGeminiSignatureBytes bounds one stored signature; an oversize signature
// is not stored (it would be a memory-exhaustion vector otherwise).
const maxGeminiSignatureBytes = 64 << 10

// NewGeminiSignatureStore returns an empty store with the given bounds. A
// non-positive capacity selects DefaultGeminiSignatureCapacity; a non-positive
// TTL selects DefaultGeminiSignatureTTL.
func NewGeminiSignatureStore(config GeminiSignatureConfig) *GeminiSignatureStore {
	capacity := config.Capacity
	if capacity <= 0 {
		capacity = DefaultGeminiSignatureCapacity
	}
	ttl := config.TTL
	if ttl <= 0 {
		ttl = DefaultGeminiSignatureTTL
	}
	return &GeminiSignatureStore{
		entries:  make(map[string]*geminiSignatureEntry),
		capacity: capacity,
		ttl:      ttl,
		now:      time.Now,
	}
}

// geminiSignatureKey scopes a call id to a provider, so ids from different
// providers never meet.
func geminiSignatureKey(scope, callID string) string {
	return scope + "\x00" + callID
}

// Put stores signature for callID under scope. An empty signature, an empty
// call id, or a signature longer than maxGeminiSignatureBytes is not stored.
func (s *GeminiSignatureStore) Put(scope, callID, signature string) {
	if s == nil || callID == "" || signature == "" || len(signature) > maxGeminiSignatureBytes {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := geminiSignatureKey(scope, callID)
	if _, ok := s.entries[key]; !ok {
		s.order = append(s.order, key)
	}
	s.entries[key] = &geminiSignatureEntry{signature: signature, updated: s.now()}
	s.evictLocked()
}

// Get returns the retained signature for callID under scope. ok is false for
// an unknown, expired, or foreign id — a miss, never an error.
func (s *GeminiSignatureStore) Get(scope, callID string) (string, bool) {
	if s == nil || callID == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := geminiSignatureKey(scope, callID)
	entry, found := s.entries[key]
	if !found {
		return "", false
	}
	if s.ttl > 0 && s.now().Sub(entry.updated) > s.ttl {
		s.removeLocked(key)
		return "", false
	}
	return entry.signature, true
}

// evictLocked enforces capacity (oldest-first) and TTL expiry. Callers hold
// s.mu.
func (s *GeminiSignatureStore) evictLocked() {
	now := s.now()
	for key, entry := range s.entries {
		if s.ttl > 0 && now.Sub(entry.updated) > s.ttl {
			s.removeLocked(key)
		}
	}
	for len(s.entries) > s.capacity {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.entries, oldest)
	}
}

// removeLocked deletes one entry and its order entry. Callers hold s.mu.
func (s *GeminiSignatureStore) removeLocked(key string) {
	delete(s.entries, key)
	for i, k := range s.order {
		if k == key {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// Capacity returns the configured signature bound, for startup logging.
func (s *GeminiSignatureStore) Capacity() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.capacity
}

// TTL returns the configured signature-age bound, for startup logging.
func (s *GeminiSignatureStore) TTL() time.Duration {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ttl
}

// Len returns the retained signature count, for tests and observability.
func (s *GeminiSignatureStore) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}
