package autostart

import (
	"strings"
)

// winQuote quotes one argument for a Windows command line by the rules CommandLineToArgvW and
// the C runtime parse (the same as syscall.EscapeArg; written out so it is tested on every OS).
func winQuote(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, slashes+1))
			slashes = 0
		default:
			slashes = 0
		}
		b.WriteByte(c)
	}
	b.WriteString(strings.Repeat(`\`, slashes))
	b.WriteByte('"')
	return b.String()
}

func winJoin(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = winQuote(a)
	}
	return strings.Join(q, " ")
}

// splitWin splits a Windows command line the way CommandLineToArgvW does for the arguments
// (2n backslashes + quote = n backslashes and a quote toggle; 2n+1 = n and a literal quote).
func splitWin(s string) []string {
	var out []string
	var cur strings.Builder
	inQ, have := false, false
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\\':
			n := 0
			for i < len(s) && s[i] == '\\' {
				n++
				i++
			}
			if i < len(s) && s[i] == '"' {
				cur.WriteString(strings.Repeat(`\`, n/2))
				if n%2 == 1 {
					cur.WriteByte('"')
					i++
				}
			} else {
				cur.WriteString(strings.Repeat(`\`, n))
			}
			have = true
			continue
		case c == '"':
			if inQ && i+1 < len(s) && s[i+1] == '"' {
				cur.WriteByte('"')
				i += 2
				continue
			}
			inQ = !inQ
			have = true
		case (c == ' ' || c == '\t') && !inQ:
			if have {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteByte(c)
			have = true
		}
		i++
	}
	if have {
		out = append(out, cur.String())
	}
	return out
}

// displayArgv is a command line for messages and logs.
func displayArgv(a []string) string { return winJoin(a) }

// sdQuote quotes one argument of a systemd ExecStart= line: specifiers (%) and variables ($)
// doubled, backslash and double quote escaped, the whole in double quotes when it is not plain.
func sdQuote(s string) string {
	plain := s != ""
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+=:,@", r)) {
			plain = false
			break
		}
	}
	if plain {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`)
	return `"` + r.Replace(s) + `"`
}

func sdJoin(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = sdQuote(a)
	}
	return strings.Join(q, " ")
}

// sdSplit undoes sdJoin (only what sdJoin writes: plain words and double-quoted words).
func sdSplit(s string) []string {
	var out []string
	var cur strings.Builder
	inQ, have := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && inQ && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
		case (c == '%' || c == '$') && i+1 < len(s) && s[i+1] == c:
			cur.WriteByte(c)
			i++
			have = true
		case c == '"':
			inQ = !inQ
			have = true
		case c == ' ' && !inQ:
			if have {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteByte(c)
			have = true
		}
	}
	if have {
		out = append(out, cur.String())
	}
	return out
}

// sdPath escapes a path for a systemd setting that takes one path (WorkingDirectory=).
func sdPath(p string) string { return strings.ReplaceAll(p, "%", "%%") }

// isWinAbs: X:\... or \\server\share\...
func isWinAbs(p string) bool {
	return len(p) >= 3 && (p[1] == ':' && (p[2] == '\\' || p[2] == '/') && (p[0]|0x20 >= 'a' && p[0]|0x20 <= 'z')) || strings.HasPrefix(p, `\\`)
}
