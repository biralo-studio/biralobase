package tests

import "strings"

// CanonicalSQL compares stored SQL across SQLite and Turso, which reprints
// its parsed schema. Quoted values/identifiers and all punctuation are kept.
// This is only a test assertion helper, never an input validator.
func CanonicalSQL(query string) string {
	var out strings.Builder
	var quote rune
	for _, c := range query {
		if quote != 0 {
			out.WriteRune(c)
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' || c == '`' {
			quote = c
			out.WriteRune(c)
			continue
		}
		switch c {
		case ' ', '\t', '\r', '\n':
			continue
		}
		out.WriteString(strings.ToLower(string(c)))
	}
	return out.String()
}
