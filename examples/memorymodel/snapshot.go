package memorymodel

import (
	"slices"
	"sync/atomic"
)

type Snapshot struct {
	Values []int
}

type Store struct {
	current atomic.Pointer[Snapshot]
}

func NewStore(values []int) *Store {
	store := &Store{}
	store.Publish(values)
	return store
}

func (s *Store) Publish(values []int) {
	s.current.Store(&Snapshot{Values: slices.Clone(values)})
}

func (s *Store) Load() Snapshot {
	current := s.current.Load()
	if current == nil {
		return Snapshot{}
	}
	return Snapshot{Values: slices.Clone(current.Values)}
}
