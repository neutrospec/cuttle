package config

import (
	"strconv"
	"strings"
)

// lineIndex maps key paths to the 1-based line where the key or its table
// header appears. Paths use dots between keys and 1-based indexes for array
// elements and arrays of tables, as in "rule[2].action" and
// "proxy.listen[1]".
//
// The TOML library reports lines only for syntax errors, so this scanner
// walks the already-parsed document again to place every key. It only runs
// on input that toml.Decode accepted, so it can be lenient.
type lineIndex map[string]int

func indexLines(src string) lineIndex {
	s := &lineScanner{src: src, line: 1, idx: lineIndex{}, arrays: map[string]int{}}
	s.document()
	return s.idx
}

// line returns the line of path, or of its nearest recorded parent, or 0.
func (ix lineIndex) line(path string) int {
	for path != "" {
		if n, ok := ix[path]; ok {
			return n
		}
		path = parentPath(path)
	}
	return 0
}

func parentPath(path string) string {
	if strings.HasSuffix(path, "]") {
		if i := strings.LastIndexByte(path, '['); i >= 0 {
			return path[:i]
		}
	}
	if i := strings.LastIndexByte(path, '.'); i >= 0 {
		return path[:i]
	}
	return ""
}

type lineScanner struct {
	src    string
	pos    int
	line   int
	idx    lineIndex
	arrays map[string]int // array-of-tables path -> elements seen
}

func (s *lineScanner) set(path string, line int) {
	if _, ok := s.idx[path]; !ok {
		s.idx[path] = line
	}
}

func (s *lineScanner) eof() bool { return s.pos >= len(s.src) }

func (s *lineScanner) peek(off int) byte {
	if s.pos+off < len(s.src) {
		return s.src[s.pos+off]
	}
	return 0
}

// skipSpace skips blanks, comments, and, with newlines, line breaks.
func (s *lineScanner) skipSpace(newlines bool) {
	for !s.eof() {
		switch c := s.src[s.pos]; {
		case c == ' ' || c == '\t' || c == '\r':
			s.pos++
		case c == '\n':
			if !newlines {
				return
			}
			s.line++
			s.pos++
		case c == '#':
			for !s.eof() && s.src[s.pos] != '\n' {
				s.pos++
			}
		default:
			return
		}
	}
}

func (s *lineScanner) skipLine() {
	for !s.eof() && s.src[s.pos] != '\n' {
		s.pos++
	}
}

func (s *lineScanner) document() {
	table := ""
	for {
		s.skipSpace(true)
		if s.eof() {
			return
		}
		if s.peek(0) == '[' {
			line := s.line
			double := s.peek(1) == '['
			if double {
				s.pos += 2
			} else {
				s.pos++
			}
			keys := s.key()
			if double {
				base := strings.Join(keys, ".")
				s.arrays[base]++
				table = base + "[" + strconv.Itoa(s.arrays[base]) + "]"
				s.set(base, line)
			} else {
				table = strings.Join(keys, ".")
			}
			s.setParents(table, line)
			s.skipLine()
			continue
		}
		before := s.pos
		s.keyValue(table)
		s.advancedFrom(before)
	}
}

// advancedFrom makes sure the scan moved past before, so unexpected input
// can never make it loop.
func (s *lineScanner) advancedFrom(before int) {
	if s.pos == before {
		s.pos++
	}
}

func (s *lineScanner) setParents(path string, line int) {
	for p := path; p != ""; p = parentPath(p) {
		s.set(p, line)
	}
}

func join(prefix string, keys []string) string {
	k := strings.Join(keys, ".")
	if prefix == "" {
		return k
	}
	return prefix + "." + k
}

// keyValue reads "key = value" inside table.
func (s *lineScanner) keyValue(table string) {
	line := s.line
	keys := s.key()
	path := join(table, keys)
	s.setParents(path, line)
	s.skipSpace(false)
	if s.peek(0) == '=' {
		s.pos++
	}
	s.skipSpace(false)
	s.value(path)
}

// key reads a possibly dotted, possibly quoted key and stops before "=" or
// "]".
func (s *lineScanner) key() []string {
	var keys []string
	for {
		s.skipSpace(false)
		switch c := s.peek(0); c {
		case '"', '\'':
			keys = append(keys, s.quotedKey(c))
		default:
			start := s.pos
			for !s.eof() {
				c := s.src[s.pos]
				if c == '.' || c == '=' || c == ']' || c == ' ' || c == '\t' || c == '\n' || c == '#' {
					break
				}
				s.pos++
			}
			keys = append(keys, s.src[start:s.pos])
		}
		s.skipSpace(false)
		if s.peek(0) == '.' {
			s.pos++
			continue
		}
		return keys
	}
}

func (s *lineScanner) quotedKey(q byte) string {
	s.pos++
	var b strings.Builder
	for !s.eof() && s.src[s.pos] != q {
		if q == '"' && s.src[s.pos] == '\\' && s.pos+1 < len(s.src) {
			s.pos++
		}
		b.WriteByte(s.src[s.pos])
		s.pos++
	}
	s.pos++
	return b.String()
}

// value skips one value, recording keys inside inline tables and elements
// of arrays.
func (s *lineScanner) value(path string) {
	switch c := s.peek(0); {
	case c == '"' || c == '\'':
		s.str(c)
	case c == '[':
		s.pos++
		for i := 1; ; i++ {
			s.skipSpace(true)
			if s.peek(0) == ']' || s.eof() {
				s.pos++
				return
			}
			elem := path + "[" + strconv.Itoa(i) + "]"
			s.set(elem, s.line)
			before := s.pos
			s.value(elem)
			s.advancedFrom(before)
			s.skipSpace(true)
			if s.peek(0) == ',' {
				s.pos++
			}
		}
	case c == '{':
		s.pos++
		for {
			s.skipSpace(true)
			if s.peek(0) == '}' || s.eof() {
				s.pos++
				return
			}
			before := s.pos
			s.keyValue(path)
			s.advancedFrom(before)
			s.skipSpace(true)
			if s.peek(0) == ',' {
				s.pos++
			}
		}
	default:
		for !s.eof() {
			c := s.src[s.pos]
			if c == ',' || c == ']' || c == '}' || c == '\n' || c == '#' {
				return
			}
			s.pos++
		}
	}
}

func (s *lineScanner) str(q byte) {
	if s.peek(1) == q && s.peek(2) == q {
		s.pos += 3
		for !s.eof() {
			if s.src[s.pos] == '\n' {
				s.line++
			}
			if q == '"' && s.src[s.pos] == '\\' {
				if s.peek(1) == '\n' {
					s.line++
				}
				s.pos += 2
				continue
			}
			if s.peek(0) == q && s.peek(1) == q && s.peek(2) == q {
				s.pos += 3
				// A closing delimiter may be followed by up to two quotes.
				for s.peek(0) == q {
					s.pos++
				}
				return
			}
			s.pos++
		}
		return
	}
	s.pos++
	for !s.eof() && s.src[s.pos] != q && s.src[s.pos] != '\n' {
		if q == '"' && s.src[s.pos] == '\\' {
			s.pos++
		}
		s.pos++
	}
	s.pos++
}
