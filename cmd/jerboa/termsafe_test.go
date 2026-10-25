package main

import (
	"bytes"
	"testing"
)

func TestSanitizeTerminal(t *testing.T) {
	for in, want := range map[string]string{
		"plain line\n":                   "plain line\n",
		"tab\there\n":                    "tab\there\n",
		"\x1b[31mred\x1b[0m\n":           "red\n",
		"\x1b]0;title\x07after":          "after",
		"\x1b]52;c;ZXZpbA==\x1b\\after":  "after",
		"line\r\x1b[2Kfake\n":            "linefake\n",
		"bell\x07 back\x08 del\x7f":      "bell back del",
		"c1\u009b31m":                    "c131m",
		"héllo ✓\n":                      "héllo ✓\n",
		"bad\xffbyte":                    "badbyte",
		"\x1bc reset":                    " reset",
		"trailing\x1b":                   "trailing",
		"\x1bP1$r0m\x1b\\dcs":            "dcs",
		"unterminated \x1b]0;never ends": "unterminated ",
	} {
		if got := sanitizeTerminal(in); got != want {
			t.Errorf("sanitizeTerminal(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGuestOutputRaw(t *testing.T) {
	var b bytes.Buffer
	_, _ = guestOutput(&b, true).Write([]byte("\x1b[31m"))
	if b.String() != "\x1b[31m" {
		t.Fatalf("raw output changed: %q", b.String())
	}
	b.Reset()
	n, err := guestOutput(&b, false).Write([]byte("\x1b[31mx"))
	if err != nil || n != 6 || b.String() != "x" {
		t.Fatalf("sanitized write = %d, %v, %q", n, err, b.String())
	}
}
