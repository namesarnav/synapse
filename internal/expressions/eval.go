package expressions

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Limits bound evaluation cost so a hostile expression cannot exhaust the process.
type Limits struct {
	MaxSteps int // AST nodes evaluated
	MaxBytes int // approximate size of any single produced value
	MaxRange int
}

var DefaultLimits = Limits{MaxSteps: 200_000, MaxBytes: 4 << 20, MaxRange: 10_000}

// Env is the evaluation context.
type Env struct {
	// Vars are the root variables (trigger, nodes, item, index, execution...).
	Vars map[string]any
	// Secret resolves secrets.NAME. Nil means no secrets are available.
	Secret func(name string) (string, bool)
	// Now supplies the clock for now(); defaults to time.Now.
	Now    func() time.Time
	Limits Limits
	// SecretsUsed collects the secret names read during evaluation.
	SecretsUsed map[string]bool
}

type evaluator struct {
	env   *Env
	steps int
	lim   Limits
}

// Eval evaluates the expression against env.
func (e *Expr) Eval(env *Env) (out any, err error) {
	if env == nil {
		env = &Env{}
	}
	lim := env.Limits
	if lim.MaxSteps == 0 {
		lim = DefaultLimits
	}
	ev := &evaluator{env: env, lim: lim}
	defer func() {
		if r := recover(); r != nil {
			if ee, ok := r.(*Error); ok {
				out, err = nil, ee
				return
			}
			panic(r)
		}
	}()
	return ev.eval(e.root), nil
}

// EvalString parses and evaluates src in one step.
func EvalString(src string, env *Env) (any, error) {
	e, err := Parse(src)
	if err != nil {
		return nil, err
	}
	return e.Eval(env)
}

func (ev *evaluator) fail(pos int, f string, a ...any) {
	panic(&Error{"runtime", pos, fmt.Sprintf(f, a...)})
}

func (ev *evaluator) tick(pos int) {
	ev.steps++
	if ev.steps > ev.lim.MaxSteps {
		panic(&Error{"limit", pos, "expression exceeded the evaluation step limit"})
	}
}

func (ev *evaluator) checkSize(pos int, v any) any {
	if s, ok := v.(string); ok && len(s) > ev.lim.MaxBytes {
		panic(&Error{"limit", pos, "value exceeds the size limit"})
	}
	if _, ok := v.([]any); ok && size(v) > ev.lim.MaxBytes {
		panic(&Error{"limit", pos, "value exceeds the size limit"})
	}
	return v
}

func (ev *evaluator) eval(n node) any {
	ev.tick(n.position())
	switch t := n.(type) {
	case litNode:
		return t.val
	case identNode:
		if t.name == "secrets" {
			ev.fail(t.pos, "secrets must be accessed as secrets.NAME")
		}
		return ev.env.Vars[t.name]
	case memberNode:
		if id, ok := t.obj.(identNode); ok && id.name == "secrets" {
			return ev.secret(t.pos, t.name)
		}
		obj := ev.eval(t.obj)
		return ev.field(t.pos, obj, t.name)
	case indexNode:
		if id, ok := t.obj.(identNode); ok && id.name == "secrets" {
			k, ok := ev.eval(t.idx).(string)
			if !ok {
				ev.fail(t.pos, "secret name must be a string")
			}
			return ev.secret(t.pos, k)
		}
		obj := ev.eval(t.obj)
		idx := ev.eval(t.idx)
		return ev.index(t.pos, obj, idx)
	case unaryNode:
		x := ev.eval(t.x)
		if t.op == "!" {
			return !Truthy(x)
		}
		f, ok := x.(float64)
		if !ok {
			ev.fail(t.pos, "cannot negate %s", typeName(x))
		}
		return -f
	case ternaryNode:
		if Truthy(ev.eval(t.cond)) {
			return ev.eval(t.a)
		}
		return ev.eval(t.b)
	case binaryNode:
		return ev.binary(t)
	case arrayNode:
		out := make([]any, len(t.elems))
		for i, e := range t.elems {
			out[i] = ev.eval(e)
		}
		return ev.checkSize(t.pos, out)
	case objectNode:
		out := make(map[string]any, len(t.keys))
		for i, k := range t.keys {
			out[k] = ev.eval(t.vals[i])
		}
		return out
	case callNode:
		return ev.call(t)
	}
	ev.fail(n.position(), "unsupported expression")
	return nil
}

func (ev *evaluator) secret(pos int, name string) any {
	if ev.env.Secret == nil {
		ev.fail(pos, "secrets are not available in this context")
	}
	v, ok := ev.env.Secret(name)
	if !ok {
		ev.fail(pos, "secret %q is not defined", name)
	}
	if ev.env.SecretsUsed != nil {
		ev.env.SecretsUsed[name] = true
	}
	return v
}

// field reads obj.name; missing fields and null receivers yield null.
func (ev *evaluator) field(pos int, obj any, name string) any {
	switch t := obj.(type) {
	case nil:
		return nil
	case map[string]any:
		return t[name]
	case []any:
		if name == "length" {
			return float64(len(t))
		}
		return nil
	case string:
		if name == "length" {
			return float64(len([]rune(t)))
		}
		return nil
	}
	return nil
}

func (ev *evaluator) index(pos int, obj, idx any) any {
	switch t := obj.(type) {
	case nil:
		return nil
	case []any:
		f, ok := idx.(float64)
		if !ok {
			ev.fail(pos, "array index must be a number, got %s", typeName(idx))
		}
		i := int(f)
		if float64(i) != f {
			ev.fail(pos, "array index must be an integer")
		}
		if i < 0 {
			i += len(t)
		}
		if i < 0 || i >= len(t) {
			return nil
		}
		return t[i]
	case map[string]any:
		k, ok := idx.(string)
		if !ok {
			ev.fail(pos, "object key must be a string, got %s", typeName(idx))
		}
		return t[k]
	case string:
		f, ok := idx.(float64)
		if !ok {
			ev.fail(pos, "string index must be a number")
		}
		r := []rune(t)
		i := int(f)
		if i < 0 {
			i += len(r)
		}
		if i < 0 || i >= len(r) {
			return nil
		}
		return string(r[i])
	}
	ev.fail(pos, "cannot index %s", typeName(obj))
	return nil
}

func (ev *evaluator) binary(t binaryNode) any {
	switch t.op {
	case "&&":
		l := ev.eval(t.l)
		if !Truthy(l) {
			return false
		}
		return Truthy(ev.eval(t.r))
	case "||":
		l := ev.eval(t.l)
		if Truthy(l) {
			return true
		}
		return Truthy(ev.eval(t.r))
	case "??":
		if l := ev.eval(t.l); l != nil {
			return l
		}
		return ev.eval(t.r)
	}
	l, r := ev.eval(t.l), ev.eval(t.r)
	switch t.op {
	case "==":
		return equal(l, r)
	case "!=":
		return !equal(l, r)
	case "in":
		switch c := r.(type) {
		case []any:
			for _, e := range c {
				if equal(l, e) {
					return true
				}
			}
			return false
		case map[string]any:
			k, ok := l.(string)
			if !ok {
				return false
			}
			_, has := c[k]
			return has
		case string:
			s, ok := l.(string)
			if !ok {
				ev.fail(t.pos, "'in' on a string needs a string on the left")
			}
			return strings.Contains(c, s)
		case nil:
			return false
		}
		ev.fail(t.pos, "'in' needs an array, object or string on the right, got %s", typeName(r))
	case "<", "<=", ">", ">=":
		return ev.compare(t.pos, t.op, l, r)
	case "+":
		switch a := l.(type) {
		case float64:
			if b, ok := r.(float64); ok {
				return a + b
			}
		case string:
			if b, ok := r.(string); ok {
				return ev.checkSize(t.pos, a+b)
			}
		case []any:
			if b, ok := r.([]any); ok {
				return ev.checkSize(t.pos, append(append(make([]any, 0, len(a)+len(b)), a...), b...))
			}
		}
		ev.fail(t.pos, "cannot add %s and %s (use to_string for text)", typeName(l), typeName(r))
	case "-", "*", "/", "%":
		a, ok1 := l.(float64)
		b, ok2 := r.(float64)
		if !ok1 || !ok2 {
			ev.fail(t.pos, "operator %s needs numbers, got %s and %s", t.op, typeName(l), typeName(r))
		}
		switch t.op {
		case "-":
			return a - b
		case "*":
			return a * b
		case "/":
			if b == 0 {
				ev.fail(t.pos, "division by zero")
			}
			return a / b
		default:
			if b == 0 {
				ev.fail(t.pos, "modulo by zero")
			}
			return math.Mod(a, b)
		}
	}
	ev.fail(t.pos, "unsupported operator %s", t.op)
	return nil
}

func (ev *evaluator) compare(pos int, op string, l, r any) any {
	var c int
	switch a := l.(type) {
	case float64:
		b, ok := r.(float64)
		if !ok {
			ev.fail(pos, "cannot compare number with %s", typeName(r))
		}
		switch {
		case a < b:
			c = -1
		case a > b:
			c = 1
		case a != b: // NaN
			return false
		}
	case string:
		b, ok := r.(string)
		if !ok {
			ev.fail(pos, "cannot compare string with %s", typeName(r))
		}
		c = strings.Compare(a, b)
	default:
		ev.fail(pos, "cannot compare %s with %s", typeName(l), typeName(r))
	}
	switch op {
	case "<":
		return c < 0
	case "<=":
		return c <= 0
	case ">":
		return c > 0
	}
	return c >= 0
}
