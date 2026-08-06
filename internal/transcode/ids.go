package transcode

import (
	"fmt"
)

// ExchangeIDs is an exchange-local ID allocator. IDs are generated once per
// exchange and reused throughout the stream so item ID, call ID, and block
// index stay consistent for the whole response.
//
// https://platform.openai.com/docs/api-reference/responses
// https://platform.claude.com/docs/en/api/messages
type ExchangeIDs struct {
	next uint64
}

// NewExchangeIDs returns an empty exchange-scoped allocator.
func NewExchangeIDs() *ExchangeIDs {
	return &ExchangeIDs{}
}

// New returns the next identifier with the given prefix, e.g. "msg_1".
func (i *ExchangeIDs) New(prefix string) string {
	i.next++
	return fmt.Sprintf("%s%d", prefix, i.next)
}
