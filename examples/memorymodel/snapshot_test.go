package memorymodel

import (
	"sync"
	"testing"
)

func TestStorePublishesImmutableSnapshots(t *testing.T) {
	input := []int{1, 2, 3}
	store := NewStore(input)
	input[0] = 99

	if got := store.Load().Values[0]; got != 1 {
		t.Fatalf("published value = %d, want 1", got)
	}

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			for range 100 {
				_ = store.Load()
			}
		})
	}
	store.Publish([]int{4, 5, 6})
	wg.Wait()
}
