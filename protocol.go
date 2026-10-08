package main

import (
	"strings"
	"unicode"
)

// One JSON request and one JSON response per socket connection.

type request struct {
	Op        string `json:"op"` // "query" or "status"
	SQL       string `json:"sql,omitempty"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
	MaxRows   int    `json:"max_rows,omitempty"`
}

type response struct {
	Columns   []string    `json:"columns,omitempty"`
	Rows      [][]*string `json:"rows,omitempty"`
	Truncated bool        `json:"truncated,omitempty"`
	ElapsedMS int64       `json:"elapsed_ms"`
	Status    string      `json:"status,omitempty"`
	Error     string      `json:"error,omitempty"`
}

var allowedKeywords = map[string]bool{
	"select":  true,
	"with":    true,
	"explain": true,
	"show":    true,
	"table":   true,
	"values":  true,
}

// leadingKeyword returns the first SQL keyword, lowercased, after leading
// whitespace, opening parentheses, and comments. Block comments nest in Postgres,
// so this counts depth to see the same keyword the server sees.
func leadingKeyword(sql string) string {
	s := sql
	for {
		s = strings.TrimLeftFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r == '(' })
		switch {
		case strings.HasPrefix(s, "--"):
			i := strings.IndexByte(s, '\n')
			if i < 0 {
				return ""
			}
			s = s[i+1:]
		case strings.HasPrefix(s, "/*"):
			rest, ok := skipBlockComment(s)
			if !ok {
				return ""
			}
			s = rest
		default:
			end := strings.IndexFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && r != '_' })
			if end < 0 {
				end = len(s)
			}
			return strings.ToLower(s[:end])
		}
	}
}

func skipBlockComment(s string) (string, bool) {
	depth := 0
	for i := 0; i < len(s)-1; i++ {
		switch {
		case s[i] == '/' && s[i+1] == '*':
			depth++
			i++
		case s[i] == '*' && s[i+1] == '/':
			depth--
			i++
			if depth == 0 {
				return s[i+1:], true
			}
		}
	}
	return "", false
}
