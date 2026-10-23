package image

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// ManifestValue is a parsed manifest value: a string, a tuple or a vector.
type ManifestValue struct {
	String string
	Tuple  []ManifestEntry
	Vector []ManifestValue
	Kind   ManifestKind
}

// ManifestEntry is one name:value pair of a tuple, in input order.
type ManifestEntry struct {
	Name  string
	Value ManifestValue
}

// ManifestKind tells which field of a ManifestValue is set.
type ManifestKind int

// Manifest value kinds.
const (
	ManifestString ManifestKind = iota
	ManifestTuple
	ManifestVector
)

// ParseManifest parses an mkfs manifest the way mkfs reads it
// (kernel/src/runtime/tuple_parser.c), so callers can check what mkfs will
// see rather than what the text looks like. It is stricter than mkfs: the
// input must be valid UTF-8 without control characters other than tab, CR
// and LF, and hold exactly one top-level tuple.
func ParseManifest(s string) (ManifestValue, error) {
	if !utf8.ValidString(s) {
		return ManifestValue{}, fmt.Errorf("manifest: not valid UTF-8")
	}
	for _, r := range s {
		if (r < 0x20 && r != '\t' && r != '\n' && r != '\r') || r == 0x7f {
			return ManifestValue{}, fmt.Errorf("manifest: control character %U", r)
		}
	}
	p := &tupleParser{}
	var roots []ManifestValue
	var start parseFn
	start = p.ignoreWhitespace(p.parseTuple(func(v ManifestValue) parseFn {
		roots = append(roots, v)
		return start
	}))
	next := start
	for _, r := range s {
		next = next(r)
		if p.err != nil {
			return ManifestValue{}, p.err
		}
		if len(roots) > 1 {
			return ManifestValue{}, fmt.Errorf("manifest: more than one top-level tuple")
		}
	}
	if len(roots) != 1 {
		return ManifestValue{}, fmt.Errorf("manifest: incomplete")
	}
	return roots[0], nil
}

// ManifestHosts returns every value of a "host" key in the manifest, at any
// depth: the host files mkfs reads. A host key whose value is not a string is
// an error, since mkfs would read it as a path anyway.
func ManifestHosts(s string) ([]string, error) {
	root, err := ParseManifest(s)
	if err != nil {
		return nil, err
	}
	var hosts []string
	var walk func(v ManifestValue) error
	walk = func(v ManifestValue) error {
		for _, e := range v.Tuple {
			if e.Name == "host" {
				if e.Value.Kind != ManifestString {
					return fmt.Errorf("manifest: host is not a path")
				}
				hosts = append(hosts, e.Value.String)
				continue
			}
			if err := walk(e.Value); err != nil {
				return err
			}
		}
		for _, x := range v.Vector {
			if err := walk(x); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root); err != nil {
		return nil, err
	}
	return hosts, nil
}

// parseFn consumes one character and returns the parser for the next, like
// the closures of the C parser.
type parseFn func(r rune) parseFn

type tupleParser struct {
	err error
}

const (
	manifestWhitespace = " \n\r\t"
	nameTerminals      = "()[]:|/" + manifestWhitespace
	valueTerminals     = "()[]" + manifestWhitespace
)

func (p *tupleParser) fail(format string, args ...any) parseFn {
	p.err = fmt.Errorf("manifest: "+format, args...)
	var dead parseFn
	dead = func(rune) parseFn { return dead }
	return dead
}

func (p *tupleParser) ignoreWhitespace(next parseFn) parseFn {
	var eat parseFn
	eat = func(r rune) parseFn {
		if r == '#' {
			var comment parseFn
			comment = func(r rune) parseFn {
				if r == '\n' {
					return eat
				}
				return comment
			}
			return comment
		}
		if strings.ContainsRune(manifestWhitespace, r) {
			return eat
		}
		return next(r)
	}
	return eat
}

func (p *tupleParser) quotedString(done func(string) parseFn) parseFn {
	var b strings.Builder
	var read parseFn
	read = func(r rune) parseFn {
		switch r {
		case '"':
			return done(b.String())
		case '\\':
			return func(r rune) parseFn {
				b.WriteRune(r)
				return read
			}
		}
		b.WriteRune(r)
		return read
	}
	return read
}

// terminal reads until a terminal character, which the returned parser then
// receives again.
func (p *tupleParser) terminal(terminals string, done func(string) parseFn) parseFn {
	var b strings.Builder
	var read parseFn
	read = func(r rune) parseFn {
		if strings.ContainsRune(terminals, r) {
			return done(b.String())(r)
		}
		b.WriteRune(r)
		return read
	}
	return read
}

func (p *tupleParser) parseTuple(done func(ManifestValue) parseFn) parseFn {
	return func(r rune) parseFn {
		if r != '(' {
			return p.fail("expected '(' but found %q", r)
		}
		return p.ignoreWhitespace(p.endOfTuple(nil, done))
	}
}

func (p *tupleParser) endOfTuple(entries []ManifestEntry, done func(ManifestValue) parseFn) parseFn {
	var self parseFn
	self = func(r rune) parseFn {
		if r == ')' {
			return done(ManifestValue{Kind: ManifestTuple, Tuple: entries})
		}
		check := p.ignoreWhitespace(self)
		nameDone := func(name string) parseFn {
			valueDone := func(v ManifestValue) parseFn {
				entries = setEntry(entries, name, v)
				return check
			}
			pv := p.parseValue(valueDone)
			return p.ignoreWhitespace(func(r rune) parseFn {
				if r != ':' {
					return p.fail("unknown property discriminator %q", r)
				}
				return pv
			})
		}
		return p.ignoreWhitespace(p.parseName(nameDone))(r)
	}
	return self
}

// setEntry replaces an existing name like mkfs's table set, keeping the
// original position, or appends a new one.
func setEntry(entries []ManifestEntry, name string, v ManifestValue) []ManifestEntry {
	for i := range entries {
		if entries[i].Name == name {
			entries[i].Value = v
			return entries
		}
	}
	return append(entries, ManifestEntry{Name: name, Value: v})
}

func (p *tupleParser) parseName(done func(string) parseFn) parseFn {
	return func(r rune) parseFn {
		if r == '"' {
			return p.quotedString(done)
		}
		return p.terminal(nameTerminals, done)(r)
	}
}

func (p *tupleParser) parseValue(done func(ManifestValue) parseFn) parseFn {
	return func(r rune) parseFn {
		switch r {
		case '(':
			return p.ignoreWhitespace(p.endOfTuple(nil, done))
		case '[':
			return p.ignoreWhitespace(p.endOfVector(nil, done))
		}
		str := func(s string) parseFn { return done(ManifestValue{Kind: ManifestString, String: s}) }
		return p.ignoreWhitespace(func(r rune) parseFn {
			if r == '"' {
				return p.quotedString(str)
			}
			return p.terminal(valueTerminals, str)(r)
		})(r)
	}
}

func (p *tupleParser) endOfVector(items []ManifestValue, done func(ManifestValue) parseFn) parseFn {
	var self parseFn
	self = func(r rune) parseFn {
		if r == ']' {
			return done(ManifestValue{Kind: ManifestVector, Vector: items})
		}
		valueDone := func(v ManifestValue) parseFn {
			items = append(items, v)
			return self
		}
		return p.ignoreWhitespace(p.parseValue(valueDone))(r)
	}
	return self
}
