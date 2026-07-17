package generics

import (
	"slices"
	"testing"
)

type Scores []int

func TestSumPreservesNamedSliceElementType(t *testing.T) {
	if got := Sum(Scores{1, 2, 3}); got != 6 {
		t.Fatalf("Sum() = %d, want 6", got)
	}
}

func TestCountdownStopsWhenConsumerBreaks(t *testing.T) {
	var got []int
	for value := range Countdown(3) {
		got = append(got, value)
		if value == 1 {
			break
		}
	}

	want := []int{3, 2, 1}
	if !slices.Equal(got, want) {
		t.Fatalf("Countdown() = %v, want %v", got, want)
	}
}
