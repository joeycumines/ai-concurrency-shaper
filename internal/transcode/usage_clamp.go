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
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Upstream usage is a subject-to-change provider value: real gateways emit
// cached breakdowns that exceed the input total and, occasionally, negative
// counts (observed 2026-09-09: a gateway reporting cached tokens above the
// prompt total failed the exchange with a 502 reading "source usage is
// arithmetically inconsistent"). The counts are CLAMPED into the client
// dialect's invariants (nonnegative counts; a cached breakdown no larger than
// the input total) and the correction is recorded as an ungated note naming
// the source numbers. The clamp is a sanctioned
// encoding, never a policy decision: it cannot be rejected by a strict loss
// profile, and it never silently drops a source fact (the note carries it).
//
// The clamp also owns the total-vs-components comparison, because only the
// clamped values are the ones the conversion emits: a total that is not the
// exact sum of the emitted input and output is recorded as a
// usage_total_mismatch note (a clamp that zeroes a negative component can
// create such a mismatch out of a source that was internally consistent).
//
// The target dialects' arithmetic identities are preserved by construction:
// the Anthropic render derives the uncached input as input - cache-read -
// cache-creation, which the clamp keeps nonnegative; the Responses render
// emits the source total when known and the checked input + output sum
// otherwise (a derived sum that cannot be represented is saturated and the
// saturation recorded, never wrapped). A clamp detail names an absent source
// count as absent rather than presenting the target's zero as a source fact
// (absent-vs-zero fidelity).

// usageClamp describes the arithmetic corrections applied to a source usage
// value, with one detail string per correction naming the source numbers.
type usageClamp struct {
	cacheExceedsInput bool
	negativeCounts    bool
	totalMismatch     bool
	cacheDetail       string
	negativeDetail    string
	totalDetail       string
}

// empty reports whether no correction was applied.
func (c usageClamp) empty() bool {
	return !c.cacheExceedsInput && !c.negativeCounts && !c.totalMismatch
}

// record notes every correction on the report at path. The notes are ungated
// (Note, never Lose): the encoding is sanctioned by the lossless-transcoding
// invariant's sanctioned-encoding arm, and the source numbers stay
// observable.
func (c usageClamp) record(report *ConversionReport, path string) error {
	if c.cacheExceedsInput {
		if err := report.Note(FeatureUsageCacheExceedsInput, path, c.cacheDetail); err != nil {
			return err
		}
	}
	if c.negativeCounts {
		if err := report.Note(FeatureUsageNegativeCounts, path, c.negativeDetail); err != nil {
			return err
		}
	}
	if c.totalMismatch {
		if err := report.Note(FeatureUsageTotalMismatch, path, c.totalDetail); err != nil {
			return err
		}
	}
	return nil
}

// usageClampNotes gates the usage-clamp notes to once per stream per key: a
// long stream may carry the same arithmetic inconsistency in several chunks,
// and the fact is already observable from the first recording.
type usageClampNotes struct {
	cacheExceedsInput bool
	negativeCounts    bool
	totalMismatch     bool
}

// note records the clamp's corrections on the report, at most once per key.
func (n *usageClampNotes) note(report *ConversionReport, path string, clamp usageClamp) error {
	if clamp.cacheExceedsInput && !n.cacheExceedsInput {
		n.cacheExceedsInput = true
		if err := report.Note(FeatureUsageCacheExceedsInput, path, clamp.cacheDetail); err != nil {
			return err
		}
	}
	if clamp.negativeCounts && !n.negativeCounts {
		n.negativeCounts = true
		if err := report.Note(FeatureUsageNegativeCounts, path, clamp.negativeDetail); err != nil {
			return err
		}
	}
	if clamp.totalMismatch && !n.totalMismatch {
		n.totalMismatch = true
		if err := report.Note(FeatureUsageTotalMismatch, path, clamp.totalDetail); err != nil {
			return err
		}
	}
	return nil
}

// usagePresence records which counts the source actually reported, so a clamp
// detail never presents a defaulted zero as a source fact.
type usagePresence struct {
	input      bool
	output     bool
	total      bool
	cacheRead  bool
	cacheWrite bool
	reasoning  bool
}

// usageTotals is the (input, output, total) triple a mismatch decision is made
// on: the emitted values for the rendered side, the source values for the
// comparison.
type usageTotals struct {
	input  int64
	output int64
	total  int64
}

// describeCount renders a source count, or "absent" when the source did not
// report it (the target's zero is then not a source fact).
func describeCount(value int64, known bool) string {
	if !known {
		return "absent"
	}
	return strconv.FormatInt(value, 10)
}

// cacheExceedsInputDetail describes a cached breakdown the clamp bounded by
// the input total, naming the bound the clamp applied. An input total the
// source did not report is named as unreported: the emitted input is then the
// target's zero default, not a source fact, so the detail must not present the
// clamp as bounding the breakdown by a source count that does not exist. An
// input the clamp itself corrected is named with both numbers, so the detail
// never states a bound the source did not report and the clamp did not apply.
func cacheExceedsInputDetail(read, write int64, readKnown, writeKnown bool, bound int64, inputKnown bool, sourceInput int64) string {
	if !inputKnown {
		return fmt.Sprintf(
			"the source usage's cached breakdown (%s read + %s creation) has no reported input total to bound it; the cached components are clamped to zero",
			describeCount(read, readKnown),
			describeCount(write, writeKnown),
		)
	}
	detail := fmt.Sprintf(
		"the source usage's cached breakdown (%s read + %s creation) exceeds the input total %d; the cached components are clamped to the input total",
		describeCount(read, readKnown),
		describeCount(write, writeKnown),
		bound,
	)
	if sourceInput != bound {
		detail += fmt.Sprintf(
			" (the source reported input %d, which the clamp corrected to %d)",
			sourceInput,
			bound,
		)
	}
	return detail
}

// checkedUsageSum adds two counts (nonnegative after clamping) and reports
// false when the sum overflows int64. The overflowed sum is never rendered:
// the caller either fails the exchange (a count that cannot be represented) or
// saturates the derived total and records the saturation.
func checkedUsageSum(a, b int64) (int64, bool) {
	if a > math.MaxInt64-b {
		return 0, false
	}
	return a + b, true
}

// derivedUsageTotal returns the total derived from input + output (the source
// reported none). A sum that cannot be represented is saturated to the maximum
// and a saturation detail is returned for the caller to record as a note: the
// emitted total is never a silent wrap into a negative count.
func derivedUsageTotal(input, output int64) (int64, string) {
	sum, ok := checkedUsageSum(input, output)
	if ok {
		return sum, ""
	}
	return math.MaxInt64, fmt.Sprintf(
		"the derived usage total (input %d + output %d) exceeds the representable range; the total is saturated to %d",
		input, output, math.MaxInt64,
	)
}

// totalMismatchDetail builds the usage_total_mismatch detail from the emitted
// counts and the source counts, naming the source numbers only where the clamp
// changed them. ok is false when the emitted total is the exact sum of the
// emitted input and output, or when the source reported no total at all (an
// absent total is derived by the renderer, never a mismatch).
func totalMismatchDetail(emitted, source usageTotals, presence usagePresence) (string, bool) {
	if !presence.total {
		return "", false
	}
	if sum, ok := checkedUsageSum(emitted.input, emitted.output); ok && sum == emitted.total {
		return "", false
	}
	if emitted == source {
		// The triple is unchanged, so no correction is claimed. A component
		// the source did not report is named absent: its emitted zero is the
		// target's default, never a source fact, and the sibling usage_unknown
		// note says the same.
		if absent := absentTotalComponents(presence); absent != "" {
			return fmt.Sprintf(
				"the usage total %d is not the exact sum of input %d + output %d; the source did not report %s, and the values it did report are relayed as-is",
				emitted.total, emitted.input, emitted.output, absent,
			), true
		}
		return fmt.Sprintf(
			"the usage total %d is not the exact sum of input %d + output %d; the source total, input, and output are relayed as-is",
			emitted.total, emitted.input, emitted.output,
		), true
	}
	return fmt.Sprintf(
		"the usage total %d is not the exact sum of input %d + output %d (the clamp corrected the source's %s); the mismatch is recorded",
		emitted.total, emitted.input, emitted.output, describeTotalCorrections(emitted, source),
	), true
}

// describeTotalCorrections names the source values the clamp actually changed,
// with both the source and the emitted numbers, or "" when the triple is
// unchanged. A detail that named every source value as corrected would claim
// corrections the clamp did not apply — the same rule the cache detail follows
// when it names the corrected input with both numbers.
func describeTotalCorrections(emitted, source usageTotals) string {
	parts := make([]string, 0, 3)
	if emitted.input != source.input {
		parts = append(parts, fmt.Sprintf("input %d to %d", source.input, emitted.input))
	}
	if emitted.output != source.output {
		parts = append(parts, fmt.Sprintf("output %d to %d", source.output, emitted.output))
	}
	if emitted.total != source.total {
		parts = append(parts, fmt.Sprintf("total %d to %d", source.total, emitted.total))
	}
	return strings.Join(parts, " and ")
}

// absentTotalComponents names the input/output components the source did not
// report, or "" when it reported both. The emitted zero for an unreported
// component is the target's default, so a detail must name it absent rather
// than present it as a source value (absent-vs-zero fidelity).
func absentTotalComponents(presence usagePresence) string {
	switch {
	case !presence.input && !presence.output:
		return "input or output"
	case !presence.input:
		return "input"
	case !presence.output:
		return "output"
	}
	return ""
}

// clampNegative returns v, or zero when v is negative, recording the change.
func clampNegative(v int64, changed *bool) int64 {
	if v < 0 {
		*changed = true
		return 0
	}
	return v
}

// clampCanonicalUsage clamps a canonical (decoded) usage value in place so
// the renderers can emit it without failing the exchange, and describes the
// corrections.
func clampCanonicalUsage(u *CanonicalUsage) usageClamp {
	var clamp usageClamp
	source := *u
	presence := usagePresence{
		input:      u.InputKnown,
		output:     u.OutputKnown,
		total:      u.TotalKnown,
		cacheRead:  u.CacheReadKnown,
		cacheWrite: u.CacheWriteKnown,
		reasoning:  u.ReasoningKnown,
	}
	negative := false
	u.InputTokens = clampNegative(u.InputTokens, &negative)
	u.CacheReadTokens = clampNegative(u.CacheReadTokens, &negative)
	u.CacheWriteTokens = clampNegative(u.CacheWriteTokens, &negative)
	u.OutputTokens = clampNegative(u.OutputTokens, &negative)
	u.ReasoningTokens = clampNegative(u.ReasoningTokens, &negative)
	u.TotalTokens = clampNegative(u.TotalTokens, &negative)
	if negative {
		clamp.negativeCounts = true
		clamp.negativeDetail = fmt.Sprintf(
			"the source usage reported negative token counts (input %s, cache-read %s, cache-creation %s, output %s, reasoning %s, total %s); each negative count is clamped to zero",
			describeCount(source.InputTokens, presence.input),
			describeCount(source.CacheReadTokens, presence.cacheRead),
			describeCount(source.CacheWriteTokens, presence.cacheWrite),
			describeCount(source.OutputTokens, presence.output),
			describeCount(source.ReasoningTokens, presence.reasoning),
			describeCount(source.TotalTokens, presence.total),
		)
	}
	if u.CacheReadTokens > u.InputTokens {
		clamp.cacheExceedsInput = true
		u.CacheReadTokens = u.InputTokens
	}
	if u.CacheWriteTokens > u.InputTokens-u.CacheReadTokens {
		clamp.cacheExceedsInput = true
		u.CacheWriteTokens = u.InputTokens - u.CacheReadTokens
	}
	if clamp.cacheExceedsInput {
		clamp.cacheDetail = cacheExceedsInputDetail(
			source.CacheReadTokens, source.CacheWriteTokens, presence.cacheRead, presence.cacheWrite,
			u.InputTokens, presence.input, source.InputTokens,
		)
	}
	clamp.totalDetail, clamp.totalMismatch = totalMismatchDetail(
		usageTotals{input: u.InputTokens, output: u.OutputTokens, total: u.TotalTokens},
		usageTotals{input: source.InputTokens, output: source.OutputTokens, total: source.TotalTokens},
		presence,
	)
	return clamp
}

// clampResponsesUsage clamps a Responses-shaped usage value in place (the
// streaming paths and the Chat→Responses conversion carry this shape) so the
// rendered usage stays arithmetically valid, and describes the corrections.
// The optional detail objects are never allocated: an absent breakdown stays
// absent (its loss decision belongs to the caller). presence reports which
// counts the source actually provided, for absent-vs-zero-accurate details.
func clampResponsesUsage(u *ResponsesUsage, presence usagePresence) usageClamp {
	var clamp usageClamp
	source := *u
	sourceCached := int64(0)
	if u.InputTokensDetails != nil {
		sourceCached = u.InputTokensDetails.CachedTokens
	}
	sourceReasoning := int64(0)
	if u.OutputTokensDetails != nil {
		sourceReasoning = u.OutputTokensDetails.ReasoningTokens
	}
	sourceCreated := int64(0)
	if u.CreatedCacheTokens != nil {
		sourceCreated = *u.CreatedCacheTokens
	}
	negative := false
	u.InputTokens = clampNegative(u.InputTokens, &negative)
	u.OutputTokens = clampNegative(u.OutputTokens, &negative)
	u.TotalTokens = clampNegative(u.TotalTokens, &negative)
	if u.InputTokensDetails != nil {
		u.InputTokensDetails.CachedTokens = clampNegative(u.InputTokensDetails.CachedTokens, &negative)
	}
	if u.OutputTokensDetails != nil {
		u.OutputTokensDetails.ReasoningTokens = clampNegative(u.OutputTokensDetails.ReasoningTokens, &negative)
	}
	if u.CreatedCacheTokens != nil {
		*u.CreatedCacheTokens = clampNegative(*u.CreatedCacheTokens, &negative)
	}
	if negative {
		clamp.negativeCounts = true
		clamp.negativeDetail = fmt.Sprintf(
			"the source usage reported negative token counts (input %s, output %s, total %s, cached %s, reasoning %s, cache-creation %s); each negative count is clamped to zero",
			describeCount(source.InputTokens, presence.input),
			describeCount(source.OutputTokens, presence.output),
			describeCount(source.TotalTokens, presence.total),
			describeCount(sourceCached, presence.cacheRead),
			describeCount(sourceReasoning, presence.reasoning),
			describeCount(sourceCreated, presence.cacheWrite),
		)
	}
	cached := int64(0)
	if u.InputTokensDetails != nil {
		cached = u.InputTokensDetails.CachedTokens
	}
	if u.InputTokensDetails != nil && cached > u.InputTokens {
		clamp.cacheExceedsInput = true
		u.InputTokensDetails.CachedTokens = u.InputTokens
		cached = u.InputTokens
	}
	if u.CreatedCacheTokens != nil && *u.CreatedCacheTokens > u.InputTokens-cached {
		clamp.cacheExceedsInput = true
		*u.CreatedCacheTokens = u.InputTokens - cached
	}
	if clamp.cacheExceedsInput {
		clamp.cacheDetail = cacheExceedsInputDetail(
			sourceCached, sourceCreated, presence.cacheRead, presence.cacheWrite,
			u.InputTokens, presence.input, source.InputTokens,
		)
	}
	clamp.totalDetail, clamp.totalMismatch = totalMismatchDetail(
		usageTotals{input: u.InputTokens, output: u.OutputTokens, total: u.TotalTokens},
		usageTotals{input: source.InputTokens, output: source.OutputTokens, total: source.TotalTokens},
		presence,
	)
	return clamp
}
