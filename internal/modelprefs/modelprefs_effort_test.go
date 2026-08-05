package modelprefs

import "testing"

func TestEffortRankOrdersCheapestFirst(t *testing.T) {
	if EffortLow.Rank() >= EffortMedium.Rank() || EffortMedium.Rank() >= EffortHigh.Rank() {
		t.Fatalf("effort ranks are not ordered: low=%d medium=%d high=%d", EffortLow.Rank(), EffortMedium.Rank(), EffortHigh.Rank())
	}
	if Effort("xhigh").Rank() != 0 {
		t.Fatalf("unknown effort rank = %d, want 0", Effort("xhigh").Rank())
	}
}

func TestMinEffort(t *testing.T) {
	tests := []struct {
		name        string
		left, right Effort
		want        Effort
	}{
		{name: "ceiling lowers", left: EffortHigh, right: EffortMedium, want: EffortMedium},
		{name: "ceiling does not raise", left: EffortLow, right: EffortHigh, want: EffortLow},
		{name: "equal", left: EffortMedium, right: EffortMedium, want: EffortMedium},
		{name: "invalid left ignored", left: "", right: EffortHigh, want: EffortHigh},
		{name: "invalid right ignored", left: EffortHigh, right: "xhigh", want: EffortHigh},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MinEffort(tt.left, tt.right); got != tt.want {
				t.Fatalf("MinEffort(%q, %q) = %q, want %q", tt.left, tt.right, got, tt.want)
			}
		})
	}
}
