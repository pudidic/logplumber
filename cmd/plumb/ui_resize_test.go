package main

import "testing"

func TestRowsAfterResize(t *testing.T) {
	for _, c := range []struct {
		widths []int
		w, n   int
	}{
		{nil, 80, 0},
		{[]int{0, 10, 80}, 80, 3},
		{[]int{81}, 80, 2},
		{[]int{100, 40}, 40, 4},
		{[]int{5}, 0, 5},
	} {
		if got := rowsAfterResize(c.widths, c.w); got != c.n {
			t.Errorf("rowsAfterResize(%v, %d) = %d, want %d", c.widths, c.w, got, c.n)
		}
	}
}
