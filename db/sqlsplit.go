package db

import "strings"

// splitSQLStatements cuts a migration script into individual
// statements on top-level semicolons. pgx's extended protocol (and a
// multiStatements-less MySQL DSN) executes one statement per Exec, so
// the runner feeds statements one at a time.
//
// The scanner understands the constructs that legitimately contain
// semicolons: line comments (--), block comments (/* */),
// single-quoted strings (a contained quote is doubled; backslash
// escapes handled), double-quoted and MySQL-backtick identifiers, and
// Postgres dollar-quoting ($tag$ ... $tag$ — how plpgsql bodies
// survive). Anything fancier
// belongs in application tooling, not a migration file.
func splitSQLStatements(script string) []string {
	var (
		out        []string
		buf        strings.Builder
		i          = 0
		n          = len(script)
		mode       = modeNormal
		tag        string // active dollar-quote tag, delimiters included
		meaningful bool   // statement content beyond comments/whitespace seen
	)

	flush := func() {
		stmt := strings.TrimSpace(buf.String())
		buf.Reset()
		if stmt != "" && meaningful {
			out = append(out, stmt)
		}
		meaningful = false
	}

	for i < n {
		c := script[i]
		switch mode {
		case modeNormal:
			switch {
			case c == '-' && i+1 < n && script[i+1] == '-':
				mode = modeLineComment
				buf.WriteByte(c)
			case c == '/' && i+1 < n && script[i+1] == '*':
				mode = modeBlockComment
				buf.WriteByte(c)
			case c == '\'':
				mode = modeSingleQuote
				meaningful = true
				buf.WriteByte(c)
			case c == '"':
				mode = modeDoubleQuote
				meaningful = true
				buf.WriteByte(c)
			case c == '`':
				mode = modeBacktick
				meaningful = true
				buf.WriteByte(c)
			case c == '$':
				if t, ok := dollarTagAt(script[i:]); ok {
					mode = modeDollar
					meaningful = true
					tag = t
					buf.WriteString(t)
					i += len(t)
					continue
				}
				meaningful = true
				buf.WriteByte(c)
			case c == ';':
				flush()
			default:
				if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
					meaningful = true
				}
				buf.WriteByte(c)
			}
		case modeLineComment:
			buf.WriteByte(c)
			if c == '\n' {
				mode = modeNormal
			}
		case modeBlockComment:
			buf.WriteByte(c)
			if c == '*' && i+1 < n && script[i+1] == '/' {
				buf.WriteByte('/')
				i++
				mode = modeNormal
			}
		case modeSingleQuote:
			buf.WriteByte(c)
			switch c {
			case '\\':
				if i+1 < n { // MySQL-style escape: consume the next char verbatim
					buf.WriteByte(script[i+1])
					i++
				}
			case '\'':
				if i+1 < n && script[i+1] == '\'' { // '' doubling stays inside
					buf.WriteByte('\'')
					i++
				} else {
					mode = modeNormal
				}
			}
		case modeDoubleQuote:
			buf.WriteByte(c)
			if c == '"' {
				if i+1 < n && script[i+1] == '"' {
					buf.WriteByte('"')
					i++
				} else {
					mode = modeNormal
				}
			}
		case modeBacktick:
			buf.WriteByte(c)
			if c == '`' {
				mode = modeNormal
			}
		case modeDollar:
			if strings.HasPrefix(script[i:], tag) {
				buf.WriteString(tag)
				i += len(tag)
				mode = modeNormal
				continue
			}
			buf.WriteByte(c)
		}
		i++
	}
	flush()
	return out
}

const (
	modeNormal = iota
	modeLineComment
	modeBlockComment
	modeSingleQuote
	modeDoubleQuote
	modeBacktick
	modeDollar
)

// dollarTagAt reports whether s starts a dollar-quote delimiter ($$ or
// $tag$ with an identifier-shaped tag) and returns it including both
// dollar signs.
func dollarTagAt(s string) (string, bool) {
	if len(s) < 2 || s[0] != '$' {
		return "", false
	}
	for j := 1; j < len(s); j++ {
		c := s[j]
		if c == '$' {
			return s[:j+1], true
		}
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return "", false
		}
	}
	return "", false
}
