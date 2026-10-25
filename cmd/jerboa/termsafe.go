package main

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// sanitizeTerminal removes terminal control sequences from guest output so a
// guest cannot drive the user's terminal (move the cursor, rewrite earlier
// lines, set the window title, write to the clipboard). Escape sequences are
// dropped whole; other control characters except tab and newline are
// dropped.
func sanitizeTerminal(s string) string {
	if !needsSanitizing(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == 0x1b:
			i = skipEscape(s, i+1)
			continue
		case c == '\n' || c == '\t':
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f:
		case c < utf8.RuneSelf:
			b.WriteByte(c)
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			// C1 controls (U+0080-U+009F) act like ESC sequences on some
			// terminals; invalid bytes are dropped too.
			if r != utf8.RuneError && (r < 0x80 || r > 0x9f) {
				b.WriteString(s[i : i+size])
			}
			i += size
			continue
		}
		i++
	}
	return b.String()
}

func needsSanitizing(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 0x20 && c != '\n' && c != '\t') || c == 0x7f || c >= utf8.RuneSelf {
			return true
		}
	}
	return false
}

// skipEscape returns the index after the escape sequence whose ESC precedes
// s[i].
func skipEscape(s string, i int) int {
	if i >= len(s) {
		return i
	}
	switch s[i] {
	case '[': // CSI: parameters and intermediates, then a final byte.
		for i++; i < len(s); i++ {
			if s[i] >= 0x40 && s[i] <= 0x7e {
				return i + 1
			}
		}
		return i
	case ']', 'P', '_', '^', 'X': // OSC, DCS, APC, PM, SOS: until BEL or ST.
		for i++; i < len(s); i++ {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
		}
		return i
	default: // Two-byte sequence.
		return i + 1
	}
}

// sanitizingWriter applies sanitizeTerminal to everything written to w.
type sanitizingWriter struct{ w io.Writer }

func (s sanitizingWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(s.w, sanitizeTerminal(string(p))); err != nil {
		return 0, fmt.Errorf("write guest output: %w", err)
	}
	return len(p), nil
}

// guestOutput returns w, sanitized unless raw is set.
func guestOutput(w io.Writer, raw bool) io.Writer {
	if raw {
		return w
	}
	return sanitizingWriter{w}
}
