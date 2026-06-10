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
