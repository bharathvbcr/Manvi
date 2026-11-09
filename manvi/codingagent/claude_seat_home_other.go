//go:build !unix

package codingagent

// homeDirProblem is root-only off Unix: there is no uid or mode-bit ownership
// model to test, so the filesystem-root refusal in resolveClaudeHome is the bound.
func homeDirProblem(string) string { return "" }
