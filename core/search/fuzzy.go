package search

import "unicode/utf8"

func Levenshtein(a, b string) int {
	la := utf8.RuneCountInString(a)
	lb := utf8.RuneCountInString(b)
	
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}

	// Early exit if length difference is too large (optimization for threshold 2)
	diff := la - lb
	if diff < -2 || diff > 2 {
		return 3 // Return something > 2
	}

	ra := []rune(a)
	rb := []rune(b)

	prev := make([]int, lb+1)
	curr := make([]int, lb+1)

	for j := 0; j <= lb; j++ {
		prev[j] = j
	}

	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := curr[j-1] + 1
			sub := prev[j-1] + cost
			
			m := del
			if ins < m {
				m = ins
			}
			if sub < m {
				m = sub
			}
			curr[j] = m
		}
		prev, curr = curr, prev
	}

	return prev[lb]
}
