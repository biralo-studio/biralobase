package schema

import "strings"

// SQLite folds only ASCII identifier characters; Unicode case changes can
// name different objects and must remain visible to the drift check.
func sqlFold(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// sqlTokens ignores SQL presentation changes made by Turso VACUUM INTO while
// retaining token boundaries and every byte inside quoted literals/identifiers.
// It is a fingerprint tokenizer, not a validator or SQL execution parser.
func sqlTokens(sql string) []string {
	var tokens []string
	word := func(b byte) bool {
		return b >= 128 || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '$'
	}
	for i := 0; i < len(sql); {
		start := i
		switch sql[i] {
		case ' ', '\t', '\n', '\r', '\f':
			i++
			continue
		case '\'', '"', '`', '[':
			end := sql[i]
			if end == '[' {
				end = ']'
			}
			i++
			for i < len(sql) {
				if sql[i] != end {
					i++
					continue
				}
				i++
				if end != ']' && i < len(sql) && sql[i] == end {
					i++
					continue
				}
				break
			}
			tokens = append(tokens, sql[start:i])
			continue
		}
		if strings.HasPrefix(sql[i:], "--") {
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			continue
		}
		if strings.HasPrefix(sql[i:], "/*") {
			if end := strings.Index(sql[i+2:], "*/"); end >= 0 {
				i += end + 4
				continue
			}
			// Invalid/unclosed comments remain significant instead of hiding SQL.
			tokens = append(tokens, sql[i:])
			break
		}
		if word(sql[i]) {
			for i < len(sql) && word(sql[i]) {
				i++
			}
			tokens = append(tokens, sqlFold(sql[start:i]))
		} else {
			i++
			tokens = append(tokens, sql[start:i])
		}
	}
	return tokens
}
