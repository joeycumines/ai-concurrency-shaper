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
	"testing"
	"time"
)

func TestGeminiSignatureStorePutGet(t *testing.T) {
	store := NewGeminiSignatureStore(GeminiSignatureConfig{Capacity: 8, TTL: time.Minute})
	if _, ok := store.Get("prov", "call_1"); ok {
		t.Fatal("empty store returned a signature")
	}
	store.Put("prov", "call_1", "sig-1")
	got, ok := store.Get("prov", "call_1")
	if !ok || got != "sig-1" {
		t.Fatalf("Get = %q, %v; want sig-1, true", got, ok)
	}
	// Overwrite refreshes.
	store.Put("prov", "call_1", "sig-2")
	if got, _ := store.Get("prov", "call_1"); got != "sig-2" {
		t.Fatalf("after overwrite Get = %q, want sig-2", got)
	}
}

func TestGeminiSignatureStoreScopeIsolation(t *testing.T) {
	store := NewGeminiSignatureStore(GeminiSignatureConfig{})
	store.Put("prov-a", "call_1", "sig-a")
	store.Put("prov-b", "call_1", "sig-b")
	if got, ok := store.Get("prov-a", "call_1"); !ok || got != "sig-a" {
		t.Fatalf("prov-a = %q, %v; want sig-a, true", got, ok)
	}
	if got, ok := store.Get("prov-b", "call_1"); !ok || got != "sig-b" {
		t.Fatalf("prov-b = %q, %v; want sig-b, true", got, ok)
	}
	if _, ok := store.Get("prov-c", "call_1"); ok {
		t.Fatal("foreign scope returned a signature")
	}
}

func TestGeminiSignatureStoreTTLExpiry(t *testing.T) {
	store := NewGeminiSignatureStore(GeminiSignatureConfig{Capacity: 8, TTL: time.Minute})
	now := time.Now()
	store.now = func() time.Time { return now }
	store.Put("prov", "call_1", "sig")
	if _, ok := store.Get("prov", "call_1"); !ok {
		t.Fatal("fresh signature expired immediately")
	}
	now = now.Add(2 * time.Minute)
	if _, ok := store.Get("prov", "call_1"); ok {
		t.Fatal("expired signature was returned")
	}
	// An expired entry is also evicted on the next Put.
	store.Put("prov", "call_2", "sig2")
	if store.Len() != 1 {
		t.Fatalf("Len = %d, want 1 (expired entry evicted)", store.Len())
	}
}

func TestGeminiSignatureStoreCapacityEvictionOrder(t *testing.T) {
	store := NewGeminiSignatureStore(GeminiSignatureConfig{Capacity: 2, TTL: time.Hour})
	store.Put("prov", "a", "1")
	store.Put("prov", "b", "2")
	store.Put("prov", "c", "3") // evicts a (oldest)
	if _, ok := store.Get("prov", "a"); ok {
		t.Fatal("oldest entry a survived capacity eviction")
	}
	if _, ok := store.Get("prov", "b"); !ok {
		t.Fatal("entry b was evicted too early")
	}
	if _, ok := store.Get("prov", "c"); !ok {
		t.Fatal("newest entry c missing")
	}
	// Re-putting b does not refresh its insertion order (eviction is
	// oldest-inserted-first, exactly like the continuity store), so b is
	// still the oldest and d's insert evicts b, not c.
	store.Put("prov", "b", "2b")
	store.Put("prov", "d", "4") // evicts b
	if _, ok := store.Get("prov", "b"); ok {
		t.Fatal("oldest entry b survived capacity eviction after re-put")
	}
	if _, ok := store.Get("prov", "c"); !ok {
		t.Fatal("entry c was evicted instead of the oldest b")
	}
	if got, ok := store.Get("prov", "d"); !ok || got != "4" {
		t.Fatalf("newest entry d = %q, %v; want 4, true", got, ok)
	}
}

func TestGeminiSignatureStoreOversizeAndEmptyNotStored(t *testing.T) {
	store := NewGeminiSignatureStore(GeminiSignatureConfig{Capacity: 4, TTL: time.Hour})
	store.Put("prov", "big", string(make([]byte, maxGeminiSignatureBytes+1)))
	store.Put("prov", "empty", "")
	store.Put("prov", "", "sig")
	if store.Len() != 0 {
		t.Fatalf("Len = %d, want 0 (oversize/empty/empty-id not stored)", store.Len())
	}
	store.Put("prov", "edge", string(make([]byte, maxGeminiSignatureBytes)))
	if store.Len() != 1 {
		t.Fatalf("Len = %d, want 1 (exactly-at-bound signature stored)", store.Len())
	}
}

func TestGeminiSignatureStoreDefaults(t *testing.T) {
	store := NewGeminiSignatureStore(GeminiSignatureConfig{})
	if store.Capacity() != DefaultGeminiSignatureCapacity {
		t.Fatalf("Capacity = %d, want %d", store.Capacity(), DefaultGeminiSignatureCapacity)
	}
	if store.TTL() != DefaultGeminiSignatureTTL {
		t.Fatalf("TTL = %s, want %s", store.TTL(), DefaultGeminiSignatureTTL)
	}
}

func TestGeminiSignatureStoreNilSafe(t *testing.T) {
	var store *GeminiSignatureStore
	store.Put("prov", "call", "sig") // must not panic
	if _, ok := store.Get("prov", "call"); ok {
		t.Fatal("nil store returned a signature")
	}
	if store.Len() != 0 || store.Capacity() != 0 || store.TTL() != 0 {
		t.Fatal("nil store reported non-zero stats")
	}
}

func TestGeminiSignatureStoreConcurrent(t *testing.T) {
	store := NewGeminiSignatureStore(GeminiSignatureConfig{Capacity: 64, TTL: time.Hour})
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := string(rune('a'+i%26)) + "-call"
			for range 100 {
				store.Put("prov", id, "sig")
				_, _ = store.Get("prov", id)
			}
		}(i)
	}
	wg.Wait()
	if store.Len() > 64 {
		t.Fatalf("Len = %d exceeds capacity 64", store.Len())
	}
}
