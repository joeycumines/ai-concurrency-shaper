package transcode

// The wire-contract pin registry.
//
// contracts.lock.json is the AUTHORITATIVE source of truth for the pinned
// protocol contracts: every schema decision in this package must trace to
// the pinned source and version. pins.md is generated from it — the
// "Pinned revisions" table is emitted by this file's generator (go
// generate) and must never be maintained by hand — while the schema
// inventories in pins.md remain the checked-in schema detail, their
// integrity covered by the lock's snapshot hashes (verified by the drift
// test in contracts_test.go).

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

//go:generate go run ./gen/pinsgen

// ContractPin pins one wire contract to a source, a version, and a snapshot
// hash covering the checked-in schema inventory for that contract.
type ContractPin struct {
	// Source is the authoritative schema source, e.g. "openai-go".
	Source string `json:"source"`
	// Version is the pinned source or protocol version, e.g. "v1.12.0" or
	// "2023-06-01".
	Version string `json:"version"`
	// SnapshotSHA256 is the sha256 of the contract's schema inventory
	// section in pins.md (from the inventory header line through the byte
	// before the next top-level section header). An inventory edited
	// without a deliberate pin bump breaks the drift test.
	SnapshotSHA256 string `json:"snapshot_sha256"`
}

// ContractsLock is the authoritative wire-contract pin registry. The JSON
// keys are stable identifiers; the order of the struct fields is the
// canonical table order.
type ContractsLock struct {
	OpenAIResponses       ContractPin `json:"openai_responses"`
	OpenAIChatCompletions ContractPin `json:"openai_chat_completions"`
	AnthropicMessages     ContractPin `json:"anthropic_messages"`
}

// namedPin pairs a pin with its registry key and protocol display name.
type namedPin struct {
	Key  string
	Name string
	Pin  ContractPin
}

// pins returns the registry entries in canonical order.
func (l ContractsLock) pins() []namedPin {
	return []namedPin{
		{"openai_responses", "OpenAI Responses", l.OpenAIResponses},
		{"openai_chat_completions", "OpenAI Chat Completions", l.OpenAIChatCompletions},
		{"anthropic_messages", "Anthropic Messages", l.AnthropicMessages},
	}
}

// inventoryHeader returns the pins.md top-level section header that starts
// the contract's schema inventory.
func inventoryHeader(protocol string) string {
	switch protocol {
	case "openai_responses":
		return "## OpenAI Responses inventory"
	case "openai_chat_completions":
		return "## OpenAI Chat Completions inventory"
	case "anthropic_messages":
		return "## Anthropic Messages inventory"
	default:
		return ""
	}
}

// LoadContractsLock reads and validates contracts.lock.json.
func LoadContractsLock() (ContractsLock, error) {
	return LoadContractsLockFile("contracts.lock.json")
}

// LoadContractsLockFile reads and validates a contracts lock file.
func LoadContractsLockFile(path string) (ContractsLock, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ContractsLock{}, fmt.Errorf("contracts lock: %w", err)
	}
	var lock ContractsLock
	if err := json.Unmarshal(data, &lock); err != nil {
		return ContractsLock{}, fmt.Errorf("contracts lock: %w", err)
	}
	for _, p := range lock.pins() {
		if p.Pin.Source == "" {
			return ContractsLock{}, fmt.Errorf("contracts lock: %s has no source", p.Name)
		}
		if p.Pin.Version == "" {
			return ContractsLock{}, fmt.Errorf("contracts lock: %s has no version", p.Name)
		}
		if _, err := hex.DecodeString(p.Pin.SnapshotSHA256); err != nil ||
			len(p.Pin.SnapshotSHA256) != sha256.Size*2 {
			return ContractsLock{}, fmt.Errorf(
				"contracts lock: %s has an invalid snapshot_sha256", p.Name,
			)
		}
	}
	return lock, nil
}

// revisionTableSection renders the "Pinned revisions" section of pins.md
// from the lock.
func revisionTableSection(lock ContractsLock) string {
	var b strings.Builder
	b.WriteString("## Pinned revisions\n\n")
	b.WriteString("| Protocol | Source | Version | Snapshot SHA256 |\n")
	b.WriteString("| --- | --- | --- | --- |\n")
	for _, p := range lock.pins() {
		fmt.Fprintf(
			&b,
			"| %s | %s | %s | `%s` |\n",
			p.Name,
			p.Pin.Source,
			p.Pin.Version,
			p.Pin.SnapshotSHA256,
		)
	}
	b.WriteString("\n")
	b.WriteString("Generated from `contracts.lock.json` (authoritative) by `go generate` — see\n")
	b.WriteString("contracts.go. Do not edit this table by hand. The schema inventories below\n")
	b.WriteString("are the checked-in schema detail; their integrity is covered by the snapshot\n")
	b.WriteString("hashes (drift test in contracts_test.go).\n\n")
	return b.String()
}

// GeneratePinsMarkdown regenerates pins.md from the lock: the "Pinned
// revisions" section is replaced with the table derived from the lock; all
// other sections (governance, inventories, deltas) are preserved verbatim.
// The result is byte-deterministic for a given lock and input.
func GeneratePinsMarkdown(lock ContractsLock, pins []byte) ([]byte, error) {
	start, end, found := findTopLevelSection(pins, "## Pinned revisions")
	if !found {
		return nil, errors.New("pins.md has no \"## Pinned revisions\" section")
	}
	out := make([]byte, 0, len(pins))
	out = append(out, pins[:start]...)
	out = append(out, revisionTableSection(lock)...)
	out = append(out, pins[end:]...)
	return out, nil
}

// VerifyContractsLock checks pins.md against the lock:
//   - regenerating pins.md from the lock reproduces the checked-in file
//     byte-for-byte (the revision table is never maintained by hand), and
//   - every schema inventory section hashes to the lock's snapshot_sha256.
//
// Any mismatch is an error naming the failing protocol.
func VerifyContractsLock(lock ContractsLock, pins []byte) error {
	regenerated, err := GeneratePinsMarkdown(lock, pins)
	if err != nil {
		return err
	}
	if !bytes.Equal(regenerated, pins) {
		return errors.New(
			"pins.md is not generated from contracts.lock.json; run `go generate ./internal/transcode`",
		)
	}
	for _, p := range lock.pins() {
		section, found := inventorySection(pins, p.Key)
		if !found {
			return fmt.Errorf(
				"pins.md has no inventory section for %s",
				p.Name,
			)
		}
		sum := sha256.Sum256(section)
		got := hex.EncodeToString(sum[:])
		if got != p.Pin.SnapshotSHA256 {
			return fmt.Errorf(
				"pins.md inventory section for %s hashes to %s, want %s",
				p.Name,
				got,
				p.Pin.SnapshotSHA256,
			)
		}
	}
	return nil
}

// findTopLevelSection locates the byte range of a top-level "## " section by
// its header prefix: from the start of the header line through the byte
// before the next top-level section header (or EOF).
func findTopLevelSection(pins []byte, headerPrefix string) (start, end int, found bool) {
	starts := topLevelHeaderStarts(pins)
	for i, s := range starts {
		lineEnd := len(pins)
		if j := bytes.IndexByte(pins[s:], '\n'); j >= 0 {
			lineEnd = s + j
		}
		if bytes.HasPrefix(pins[s:lineEnd], []byte(headerPrefix)) {
			end = len(pins)
			if i+1 < len(starts) {
				end = starts[i+1]
			}
			return s, end, true
		}
	}
	return 0, 0, false
}

// inventorySection returns the schema inventory section for the protocol
// key (the section starting at its inventory header).
func inventorySection(pins []byte, protocol string) ([]byte, bool) {
	header := inventoryHeader(protocol)
	if header == "" {
		return nil, false
	}
	start, end, found := findTopLevelSection(pins, header)
	if !found {
		return nil, false
	}
	return pins[start:end], true
}

// topLevelHeaderStarts returns the byte offsets of every line that starts a
// top-level "## " section (subsections use "### " and do not match).
func topLevelHeaderStarts(pins []byte) []int {
	var out []int
	start := 0
	for start < len(pins) {
		lineEnd := len(pins)
		if j := bytes.IndexByte(pins[start:], '\n'); j >= 0 {
			lineEnd = start + j
		}
		if bytes.HasPrefix(pins[start:lineEnd], []byte("## ")) {
			out = append(out, start)
		}
		if lineEnd == len(pins) {
			break
		}
		start = lineEnd + 1
	}
	return out
}
