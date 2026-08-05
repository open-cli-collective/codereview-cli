// Package modelprefs defines provider-neutral model preference values.
package modelprefs

// Effort is a provider-neutral reasoning/work-effort preference.
type Effort string

// Effort values.
const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
)

// Valid reports whether e is a known effort value.
func (e Effort) Valid() bool {
	switch e {
	case EffortLow, EffortMedium, EffortHigh:
		return true
	default:
		return false
	}
}

// Rank orders effort values from cheapest to most expensive. Unknown values
// rank 0 so they never win a comparison against a valid effort.
func (e Effort) Rank() int {
	switch e {
	case EffortLow:
		return 1
	case EffortMedium:
		return 2
	case EffortHigh:
		return 3
	default:
		return 0
	}
}

// MinEffort returns the cheaper of left and right. Invalid values are ignored
// so a missing ceiling leaves the requested effort untouched.
func MinEffort(left, right Effort) Effort {
	if !left.Valid() {
		return right
	}
	if !right.Valid() {
		return left
	}
	if left.Rank() <= right.Rank() {
		return left
	}
	return right
}
