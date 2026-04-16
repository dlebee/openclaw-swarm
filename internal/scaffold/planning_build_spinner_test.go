package scaffold

import (
	"math"
	"testing"
)

func TestPhaseCompileFraction(t *testing.T) {
	tests := []struct {
		idx, total int
		want       float64
	}{
		{0, 3, 1.0 / 3.0},
		{1, 3, 2.0 / 3.0},
		{2, 3, 1.0},
		{0, 1, 1.0},
		{0, 0, 1.0},
	}
	for _, tt := range tests {
		got := phaseCompileFraction(tt.idx, tt.total)
		if math.Abs(got-tt.want) > 1e-9 {
			t.Fatalf("phaseCompileFraction(%d,%d) = %v, want %v", tt.idx, tt.total, got, tt.want)
		}
	}
}
