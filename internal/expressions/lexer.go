// Package expressions implements Synapse's expression language: a small,
// side-effect-free, non-Turing-complete language for conditions, transforms
// and {{ }} templates. It is parsed by a hand-written Pratt parser and
// evaluated by a tree walker with hard resource limits. There is no eval,
// reflection or host access.
package expressions

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Error is a syntax or runtime failure with a byte offset into the source.
type Error struct {
	Kind string // "syntax" | "runtime" | "limit"
	Pos  int
	Msg  string
}

func (e *Error) Error() string { return fmt.Sprintf("%s error at %d: %s", e.Kind, e.Pos, e.Msg) }

func synErr(pos int, f string, a ...any) *Error { return &Error{"syntax", pos, fmt.Sprintf(f, a...)} }

type tokKind int

const (
	tEOF tokKind = iota
	tNum
	tStr
	tIdent
	tPunct // operators and delimiters; text holds the lexeme
)

type token struct {
	kind tokKind
	text string
	num  float64
	pos  int
}

const (
	MaxSourceLen = 8192
	maxTokens    = 4000
)

func lex(src string) ([]token, error) {
	if len(src) > MaxSourceLen {
		return nil, &Error{"limit", 0, fmt.Sprintf("expression longer than %d bytes", MaxSourceLen)}
	}
	var toks []token
	i := 0
	for i < len(src) {
		if len(toks) > maxTokens {
			return nil, &Error{"limit", i, "too many tokens"}
		}
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c >= '0' && c <= '9' || (c == '.' && i+1 < len(src) && src[i+1] >= '0' && src[i+1] <= '9'):
			start := i
			for i < len(src) && (src[i] >= '0' && src[i] <= '9' || src[i] == '.') {
				i++
			}
			if i < len(src) && (src[i] == 'e' || src[i] == 'E') {
				j := i + 1
				if j < len(src) && (src[j] == '+' || src[j] == '-') {
					j++
				}
				if j < len(src) && src[j] >= '0' && src[j] <= '9' {
					for j < len(src) && src[j] >= '0' && src[j] <= '9' {
						j++
					}
					i = j
				}
			}
			f, err := strconv.ParseFloat(src[start:i], 64)
			if err != nil {
				return nil, synErr(start, "invalid number %q", src[start:i])
			}
			toks = append(toks, token{kind: tNum, num: f, text: src[start:i], pos: start})
		case c == '"' || c == '\'':
			s, n, err := lexString(src, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{kind: tStr, text: s, pos: i})
			i = n
		case isIdentStart(c):
			start := i
			for i < len(src) && isIdentPart(src[i]) {
				i++
			}
			toks = append(toks, token{kind: tIdent, text: src[start:i], pos: start})
		default:
			if p := punct(src[i:]); p != "" {
				toks = append(toks, token{kind: tPunct, text: p, pos: i})
				i += len(p)
			} else {
				r, _ := utf8.DecodeRuneInString(src[i:])
				return nil, synErr(i, "unexpected character %q", r)
			}
		}
	}
	toks = append(toks, token{kind: tEOF, pos: len(src)})
	return toks, nil
}

func isIdentStart(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isIdentPart(c byte) bool  { return isIdentStart(c) || c >= '0' && c <= '9' }

var puncts = []string{"?.", "??", "==", "!=", "<=", ">=", "&&", "||",
	"+", "-", "*", "/", "%", "<", ">", "!", "?", ":", ".", ",", "(", ")", "[", "]", "{", "}"}

func punct(s string) string {
	for _, p := range puncts {
		if strings.HasPrefix(s, p) {
			return p
		}
	}
	return ""
}

func lexString(src string, start int) (string, int, error) {
	q := src[start]
	var b strings.Builder
	i := start + 1
	for i < len(src) {
		c := src[i]
		switch {
		case c == q:
			return b.String(), i + 1, nil
		case c == '\\':
			i++
			if i >= len(src) {
				return "", 0, synErr(start, "unterminated string")
			}
			switch e := src[i]; e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '\\', '"', '\'', '/':
				b.WriteByte(e)
			case 'u':
				if i+4 >= len(src) {
					return "", 0, synErr(i, "invalid unicode escape")
				}
				n, err := strconv.ParseUint(src[i+1:i+5], 16, 32)
				if err != nil {
					return "", 0, synErr(i, "invalid unicode escape")
				}
				b.WriteRune(rune(n))
				i += 4
			default:
				return "", 0, synErr(i, "unknown escape \\%c", e)
			}
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return "", 0, synErr(start, "unterminated string")
}
