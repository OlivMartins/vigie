package policy

// Match reports whether name matches pattern, where * stands for any
// sequence of characters (including none) and ? for exactly one character.
// Nothing else is special: tool names are matched literally otherwise.
func Match(pattern, name string) bool {
	p, n := []rune(pattern), []rune(name)
	// Iterative wildcard matching with backtracking on the last star.
	pi, ni := 0, 0
	star, mark := -1, 0
	for ni < len(n) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == n[ni]):
			pi++
			ni++
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, ni
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			ni = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// MatchAny returns the first pattern that matches name.
func MatchAny(patterns []string, name string) (string, bool) {
	for _, p := range patterns {
		if Match(p, name) {
			return p, true
		}
	}
	return "", false
}
