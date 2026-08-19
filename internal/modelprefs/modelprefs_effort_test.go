package modelprefs

import "testing"

func TestEffortRankOrdersCheapestFirst(t *testing.T) {
	ordered := []Effort{EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax}
	for i := 1; i < len(ordered); i++ {
		if ordered[i-1].Rank() >= ordered[i].Rank() {
			t.Fatalf("effort ranks are not ordered: %q=%d %q=%d", ordered[i-1], ordered[i-1].Rank(), ordered[i], ordered[i].Rank())
		}
	}
	if Effort("ultra").Rank() != 0 {
		t.Fatalf("unknown effort rank = %d, want 0", Effort("ultra").Rank())
	}
}

func TestMinEffort(t *testing.T) {
	tests := []struct {
		name        string
		left, right Effort
		want        Effort
	}{
		{name: "ceiling lowers", left: EffortHigh, right: EffortMedium, want: EffortMedium},
		{name: "extended ceiling lowers", left: EffortMax, right: EffortXHigh, want: EffortXHigh},
		{name: "ceiling does not raise", left: EffortLow, right: EffortHigh, want: EffortLow},
		{name: "equal", left: EffortMedium, right: EffortMedium, want: EffortMedium},
		{name: "invalid left ignored", left: "", right: EffortHigh, want: EffortHigh},
		{name: "invalid right ignored", left: EffortHigh, right: "ultra", want: EffortHigh},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MinEffort(tt.left, tt.right); got != tt.want {
				t.Fatalf("MinEffort(%q, %q) = %q, want %q", tt.left, tt.right, got, tt.want)
			}
		})
	}
}
