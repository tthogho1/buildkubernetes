package main

import "strings"

// unifiedDiff is a small LCS line diff; the files edited here are tiny.
func unifiedDiff(name, a, b string) string {
	if a == b {
		return ""
	}
	x, y := strings.Split(a, "\n"), strings.Split(b, "\n")
	n, m := len(x), len(y)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var sb strings.Builder
	sb.WriteString("--- " + name + "\n+++ " + name + "\n")
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && x[i] == y[j]:
			i, j = i+1, j+1
		case i < n && (j == m || lcs[i+1][j] >= lcs[i][j+1]):
			sb.WriteString("-" + x[i] + "\n")
			i++
		default:
			sb.WriteString("+" + y[j] + "\n")
			j++
		}
	}
	return sb.String()
}
