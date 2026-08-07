package expressions

import (
	"fmt"
	"strings"
)

// Template is a string with embedded {{ expression }} segments.
type Template struct {
	src   string
	parts []tmplPart
}

type tmplPart struct {
	lit  string
	expr *Expr
}

// ParseTemplate splits src into literal text and expressions. `\{{` renders a
// literal "{{".
func ParseTemplate(src string) (*Template, error) {
	if len(src) > 64<<10 {
		return nil, &Error{"limit", 0, "template too large"}
	}
	t := &Template{src: src}
	var lit strings.Builder
	i := 0
	for i < len(src) {
		if strings.HasPrefix(src[i:], `\{{`) {
			lit.WriteString("{{")
			i += 3
			continue
		}
		if !strings.HasPrefix(src[i:], "{{") {
			lit.WriteByte(src[i])
			i++
			continue
		}
		end, err := findClose(src, i+2)
		if err != nil {
			return nil, err
		}
		body := src[i+2 : end]
		e, err := Parse(body)
		if err != nil {
			if ee, ok := err.(*Error); ok {
				return nil, &Error{ee.Kind, ee.Pos + i + 2, ee.Msg}
			}
			return nil, err
		}
		if lit.Len() > 0 {
			t.parts = append(t.parts, tmplPart{lit: lit.String()})
			lit.Reset()
		}
		t.parts = append(t.parts, tmplPart{expr: e})
		i = end + 2
	}
	if lit.Len() > 0 {
		t.parts = append(t.parts, tmplPart{lit: lit.String()})
	}
	return t, nil
}

// findClose returns the index of the "}}" that ends a template expression
// starting at from, skipping quoted strings and nested braces.
func findClose(src string, from int) (int, error) {
	depth := 0
	for i := from; i < len(src); i++ {
		switch c := src[i]; c {
		case '"', '\'':
			for i++; i < len(src) && src[i] != c; i++ {
				if src[i] == '\\' {
					i++
				}
			}
		case '{':
			depth++
		case '}':
			if depth > 0 {
				depth--
			} else if i+1 < len(src) && src[i+1] == '}' {
				return i, nil
			}
		}
	}
	return 0, synErr(from-2, "unterminated {{ expression")
}

// IsPure reports whether the template has no expressions.
func (t *Template) IsPure() bool {
	for _, p := range t.parts {
		if p.expr != nil {
			return false
		}
	}
	return true
}

// Render evaluates the template. A template that is exactly one expression
// returns that expression's typed value; otherwise a string is produced.
func (t *Template) Render(env *Env) (any, error) {
	if len(t.parts) == 1 && t.parts[0].expr != nil {
		return t.parts[0].expr.Eval(env)
	}
	var b strings.Builder
	for _, p := range t.parts {
		if p.expr == nil {
			b.WriteString(p.lit)
			continue
		}
		v, err := p.expr.Eval(env)
		if err != nil {
			return nil, err
		}
		b.WriteString(ToString(v))
		if b.Len() > DefaultLimits.MaxBytes {
			return nil, &Error{"limit", 0, "rendered template exceeds the size limit"}
		}
	}
	return b.String(), nil
}

// RenderString renders and stringifies the result.
func (t *Template) RenderString(env *Env) (string, error) {
	v, err := t.Render(env)
	if err != nil {
		return "", err
	}
	return ToString(v), nil
}

// RenderTemplate parses and renders src.
func RenderTemplate(src string, env *Env) (any, error) {
	t, err := ParseTemplate(src)
	if err != nil {
		return nil, err
	}
	return t.Render(env)
}

// Resolve walks a JSON-like value and renders every string containing "{{".
func Resolve(v any, env *Env) (any, error) {
	switch t := v.(type) {
	case string:
		if !strings.Contains(t, "{{") && !strings.Contains(t, `\{{`) {
			return t, nil
		}
		return RenderTemplate(t, env)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			r, err := Resolve(e, env)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out[i] = r
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			r, err := Resolve(e, env)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = r
		}
		return out, nil
	}
	return v, nil
}
