package generics

import "iter"

type Number interface {
	~int | ~int64 | ~float64
}

func Sum[S ~[]E, E Number](values S) E {
	var total E
	for _, value := range values {
		total += value
	}
	return total
}

func Countdown(from int) iter.Seq[int] {
	return func(yield func(int) bool) {
		for value := from; value >= 0; value-- {
			if !yield(value) {
				return
			}
		}
	}
}
