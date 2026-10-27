// Package datarevision preserves the exact versions of facts used by an answer.
// Wall clocks and the order of transaction starts are not revision evidence.
package datarevision

import (
	"encoding/json"
	"errors"
	"regexp"
	"slices"
)

const (
	// Categories identifies manual and rule-based category changes.
	Categories = "categories"
	// Uploads identifies imported or removed statement facts, including no-data imports.
	Uploads = "uploads"
	// TimeZone identifies calendar-setting changes.
	TimeZone = "time_zone"
	// Unverified marks missing or incomparable provenance without claiming a change.
	Unverified = "unverified"
)

// ErrInvalid contains no peer data or storage contents.
var ErrInvalid = errors.New("invalid data revisions")
var codeShape = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var required = [...]string{Categories, Uploads, TimeZone}

// Current is one coherent service response. Missing and known zero differ.
type Current map[string]int64

// Baseline retains every observed revision for every used source. Incomplete is
// sticky: a later overview must never make an earlier unknown source provable.
type Baseline struct {
	Values     map[string][]int64 `json:"values"`
	Incomplete bool               `json:"incomplete"`
}

// DecodeBaseline accepts only the private persisted shape. JSON null must never
// become a known zero revision, and a missing completeness bit is not proof.
func DecodeBaseline(raw []byte) (Baseline, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 2 {
		return Baseline{}, ErrInvalid
	}
	valuesRaw, hasValues := fields["values"]
	incompleteRaw, hasIncomplete := fields["incomplete"]
	if !hasValues || !hasIncomplete {
		return Baseline{}, ErrInvalid
	}
	var wire struct {
		Values     map[string][]*int64
		Incomplete *bool
	}
	if json.Unmarshal(valuesRaw, &wire.Values) != nil || json.Unmarshal(incompleteRaw, &wire.Incomplete) != nil || wire.Values == nil || wire.Incomplete == nil {
		return Baseline{}, ErrInvalid
	}

	result := Baseline{Values: make(map[string][]int64, len(wire.Values)), Incomplete: *wire.Incomplete}
	for code, values := range wire.Values {
		for _, value := range values {
			if value == nil {
				return Baseline{}, ErrInvalid
			}
			result.Values[code] = append(result.Values[code], *value)
		}
		if len(values) == 0 {
			return Baseline{}, ErrInvalid
		}
	}
	if !ValidBaseline(result) {
		return Baseline{}, ErrInvalid
	}
	return result, nil
}

// ValidCurrent enforces bounded open codes and nonnegative exact int64 values.
func ValidCurrent(v Current) bool {
	if len(v) > 32 {
		return false
	}
	for code, value := range v {
		if !codeShape.MatchString(code) || value < 0 {
			return false
		}
	}
	return true
}

// Complete reports presence of the known source dependencies, including zero.
func Complete(v Current) bool {
	for _, code := range required {
		if _, ok := v[code]; !ok {
			return false
		}
	}
	return true
}

// ValidBaseline validates the durable shape. The larger code bound accounts for
// up to 16 calls observing different open-code sets, not an unbounded JSON payload.
func ValidBaseline(b Baseline) bool {
	if len(b.Values) > 512 {
		return false
	}
	for code, values := range b.Values {
		if !codeShape.MatchString(code) || len(values) == 0 || len(values) > 32 {
			return false
		}
		seen := map[int64]bool{}
		for _, v := range values {
			if v < 0 || seen[v] {
				return false
			}
			seen[v] = true
		}
	}
	return true
}

// Merge unions exact observations without mutating earlier snapshots. Sets avoid
// relying on monotonic-current assumptions when mixed/stale results are consumed.
func Merge(previous Baseline, next Current) (Baseline, error) {
	if !ValidCurrent(next) || !ValidBaseline(previous) {
		return Baseline{}, ErrInvalid
	}
	// Open kinds are also provenance: a value observed by only some of the
	// consumed sources cannot certify sources whose vector omitted that kind.
	incomplete := previous.Incomplete || !Complete(next)
	if len(previous.Values) > 0 {
		for code := range previous.Values {
			if _, ok := next[code]; !ok {
				incomplete = true
			}
		}
		for code := range next {
			if len(previous.Values[code]) == 0 {
				incomplete = true
			}
		}
	}
	result := Baseline{Values: make(map[string][]int64, len(previous.Values)+len(next)), Incomplete: incomplete}
	for code, values := range previous.Values {
		result.Values[code] = slices.Clone(values)
	}
	for code, value := range next {
		if !slices.Contains(result.Values[code], value) {
			result.Values[code] = append(result.Values[code], value)
			slices.Sort(result.Values[code])
		}
	}
	if !ValidBaseline(result) {
		return Baseline{}, ErrInvalid
	}
	return result, nil
}

// EqualKnown is suitable only for combining two complete coherent result sets.
func EqualKnown(a, b Current) bool {
	if !ValidCurrent(a) || !ValidCurrent(b) || !Complete(a) || !Complete(b) || len(a) != len(b) {
		return false
	}
	for code, value := range a {
		if other, ok := b[code]; !ok || other != value {
			return false
		}
	}
	return true
}

// Changes returns exact known changes plus explicit uncertainty. Equality is
// claimed only when every observed baseline value exists and equals current.
func Changes(b Baseline, current Current) []string {
	if !ValidBaseline(b) || !ValidCurrent(current) {
		return []string{Unverified}
	}
	unknown := b.Incomplete || !Complete(current)
	changes := make([]string, 0, 4)
	for _, code := range required {
		values, known := b.Values[code]
		now, available := current[code]
		if !known || len(values) == 0 || !available {
			unknown = true
			continue
		}
		for _, value := range values {
			if value != now {
				changes = append(changes, code)
				break
			}
		}
	}
	for code, values := range b.Values {
		if slices.Contains(required[:], code) {
			continue
		}
		now, ok := current[code]
		if !ok {
			unknown = true
			continue
		}
		for _, value := range values {
			if value != now {
				unknown = true
			}
		}
	}
	for code := range current {
		if len(b.Values[code]) == 0 {
			unknown = true
		}
	}
	if unknown {
		changes = append(changes, Unverified)
	}
	return changes
}
