package transcode

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// ExchangeIDs is an exchange-local ID allocator. IDs are generated once per
// exchange and reused throughout the stream so item ID, call ID, and block
// index stay consistent for the whole response.
//
// Every allocator draws a collision-resistant random prefix so the API
// object identifiers it emits are unique across unrelated exchanges: clients
// key response stores, tool-call correlation, logs and traces, and
// retry/dedup systems by these IDs, and the old per-exchange counter alone
// (resp_1/msg_1/fc_1 on every exchange) made collisions certain (review-08
// blocker 6).
//
// The prefix is 16 lowercase hex characters (64 bits of entropy from
// crypto/rand): both provider ID shapes are opaque lowercase-alphanumeric
// strings (e.g. Anthropic msg_01X...), so the emitted IDs stay within the
// shape clients already accept. The local counter is appended only for
// ordering within one exchange.
//
// https://platform.openai.com/docs/api-reference/responses
// https://platform.claude.com/docs/en/api/messages
type ExchangeIDs struct {
	prefix string
	next   uint64
}

// NewExchangeIDs returns an exchange-scoped allocator with a random
// collision-resistant prefix.
func NewExchangeIDs() *ExchangeIDs {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand.Read never fails on supported platforms; a failure
		// means the process cannot produce unique identifiers, so the
		// exchange must not proceed.
		panic(fmt.Sprintf("transcode: crypto/rand failed: %v", err))
	}
	return &ExchangeIDs{
		prefix: hex.EncodeToString(raw[:]),
	}
}

// New returns the next identifier with the given prefix, e.g. "msg_<prefix>_1".
func (i *ExchangeIDs) New(prefix string) string {
	i.next++
	return fmt.Sprintf("%s%s_%d", prefix, i.prefix, i.next)
}
