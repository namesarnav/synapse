package expressions

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type fn struct {
	min, max int // max -1 means variadic
	impl     func(ev *evaluator, pos int, a []any) any
}

func (ev *evaluator) call(t callNode) any {
	f, ok := funcs[t.name]
	if !ok {
		ev.fail(t.pos, "unknown function %s()", t.name)
	}
	if len(t.args) < f.min || (f.max >= 0 && len(t.args) > f.max) {
		ev.fail(t.pos, "%s() takes %s arguments, got %d", t.name, arity(f), len(t.args))
	}
	args := make([]any, len(t.args))
	// default() and coalesce() are lazy-friendly only via ??; all args are evaluated.
	for i, a := range t.args {
		args[i] = ev.eval(a)
	}
	return ev.checkSize(t.pos, f.impl(ev, t.pos, args))
}

func arity(f fn) string {
	switch {
	case f.max < 0:
		return "at least " + itoa(f.min)
	case f.min == f.max:
		return itoa(f.min)
	}
	return itoa(f.min) + "-" + itoa(f.max)
}

func itoa(n int) string { return formatNumber(float64(n)) }

func (ev *evaluator) str(pos int, name string, v any) string {
	s, ok := v.(string)
	if !ok {
		ev.fail(pos, "%s() expects a string, got %s", name, typeName(v))
	}
	return s
}

func (ev *evaluator) num(pos int, name string, v any) float64 {
	f, ok := v.(float64)
	if !ok {
		ev.fail(pos, "%s() expects a number, got %s", name, typeName(v))
	}
	return f
}

func (ev *evaluator) arr(pos int, name string, v any) []any {
	a, ok := v.([]any)
	if !ok {
		ev.fail(pos, "%s() expects an array, got %s", name, typeName(v))
	}
	return a
}

func (ev *evaluator) obj(pos int, name string, v any) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		ev.fail(pos, "%s() expects an object, got %s", name, typeName(v))
	}
	return m
}

var (
	reMu    sync.Mutex
	reCache = map[string]*regexp.Regexp{}
)

// compileRegex uses RE2 (linear time) and caches a bounded number of patterns.
func (ev *evaluator) compileRegex(pos int, pat string) *regexp.Regexp {
	if len(pat) > 512 {
		ev.fail(pos, "regex pattern too long")
	}
	reMu.Lock()
	defer reMu.Unlock()
	if re, ok := reCache[pat]; ok {
		return re
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		ev.fail(pos, "invalid regex: %v", err)
	}
	if len(reCache) > 256 {
		reCache = map[string]*regexp.Regexp{}
	}
	reCache[pat] = re
	return re
}

func numFn(name string, f func(float64) float64) fn {
	return fn{1, 1, func(ev *evaluator, pos int, a []any) any { return f(ev.num(pos, name, a[0])) }}
}

var funcs map[string]fn

func init() {
	funcs = map[string]fn{
		// generic
		"length": {1, 1, func(ev *evaluator, pos int, a []any) any {
			switch t := a[0].(type) {
			case nil:
				return 0.0
			case string:
				return float64(utf8.RuneCountInString(t))
			case []any:
				return float64(len(t))
			case map[string]any:
				return float64(len(t))
			}
			ev.fail(pos, "length() expects a string, array or object, got %s", typeName(a[0]))
			return nil
		}},
		"type_of": {1, 1, func(_ *evaluator, _ int, a []any) any { return typeName(a[0]) }},
		"is_null": {1, 1, func(_ *evaluator, _ int, a []any) any { return a[0] == nil }},
		"is_empty": {1, 1, func(_ *evaluator, _ int, a []any) any {
			switch t := a[0].(type) {
			case nil:
				return true
			case string:
				return t == ""
			case []any:
				return len(t) == 0
			case map[string]any:
				return len(t) == 0
			}
			return false
		}},
		"default": {2, 2, func(_ *evaluator, _ int, a []any) any {
			if a[0] == nil {
				return a[1]
			}
			return a[0]
		}},
		"coalesce": {1, -1, func(_ *evaluator, _ int, a []any) any {
			for _, v := range a {
				if v != nil {
					return v
				}
			}
			return nil
		}},
		"now": {0, 0, func(ev *evaluator, _ int, _ []any) any {
			now := time.Now
			if ev.env.Now != nil {
				now = ev.env.Now
			}
			return now().UTC().Format(time.RFC3339Nano)
		}},

		// conversion
		"to_string": {1, 1, func(_ *evaluator, _ int, a []any) any { return ToString(a[0]) }},
		"to_number": {1, 1, func(ev *evaluator, pos int, a []any) any {
			switch t := a[0].(type) {
			case float64:
				return t
			case bool:
				if t {
					return 1.0
				}
				return 0.0
			case string:
				var f float64
				if err := json.Unmarshal([]byte(strings.TrimSpace(t)), &f); err != nil {
					ev.fail(pos, "to_number(): %q is not a number", t)
				}
				return f
			}
			ev.fail(pos, "to_number() cannot convert %s", typeName(a[0]))
			return nil
		}},
		"to_bool": {1, 1, func(_ *evaluator, _ int, a []any) any { return Truthy(a[0]) }},
		"json_parse": {1, 1, func(ev *evaluator, pos int, a []any) any {
			var v any
			if err := json.Unmarshal([]byte(ev.str(pos, "json_parse", a[0])), &v); err != nil {
				ev.fail(pos, "json_parse(): %v", err)
			}
			return Normalize(v)
		}},
		"json_stringify": {1, 1, func(_ *evaluator, _ int, a []any) any { return jsonString(a[0]) }},

		// strings
		"upper": {1, 1, func(ev *evaluator, pos int, a []any) any { return strings.ToUpper(ev.str(pos, "upper", a[0])) }},
		"lower": {1, 1, func(ev *evaluator, pos int, a []any) any { return strings.ToLower(ev.str(pos, "lower", a[0])) }},
		"trim":  {1, 1, func(ev *evaluator, pos int, a []any) any { return strings.TrimSpace(ev.str(pos, "trim", a[0])) }},
		"contains": {2, 2, func(ev *evaluator, pos int, a []any) any {
			switch c := a[0].(type) {
			case string:
				return strings.Contains(c, ev.str(pos, "contains", a[1]))
			case []any:
				for _, e := range c {
					if equal(e, a[1]) {
						return true
					}
				}
				return false
			case map[string]any:
				k, ok := a[1].(string)
				_, has := c[k]
				return ok && has
			}
			ev.fail(pos, "contains() expects a string, array or object, got %s", typeName(a[0]))
			return nil
		}},
		"starts_with": {2, 2, func(ev *evaluator, pos int, a []any) any {
			return strings.HasPrefix(ev.str(pos, "starts_with", a[0]), ev.str(pos, "starts_with", a[1]))
		}},
		"ends_with": {2, 2, func(ev *evaluator, pos int, a []any) any {
			return strings.HasSuffix(ev.str(pos, "ends_with", a[0]), ev.str(pos, "ends_with", a[1]))
		}},
		"split": {2, 2, func(ev *evaluator, pos int, a []any) any {
			parts := strings.Split(ev.str(pos, "split", a[0]), ev.str(pos, "split", a[1]))
			out := make([]any, len(parts))
			for i, p := range parts {
				out[i] = p
			}
			return out
		}},
		"join": {2, 2, func(ev *evaluator, pos int, a []any) any {
			arr := ev.arr(pos, "join", a[0])
			parts := make([]string, len(arr))
			for i, e := range arr {
				parts[i] = ToString(e)
			}
			return strings.Join(parts, ev.str(pos, "join", a[1]))
		}},
		"replace": {3, 3, func(ev *evaluator, pos int, a []any) any {
			return strings.ReplaceAll(ev.str(pos, "replace", a[0]), ev.str(pos, "replace", a[1]), ev.str(pos, "replace", a[2]))
		}},
		"substring": {2, 3, func(ev *evaluator, pos int, a []any) any {
			r := []rune(ev.str(pos, "substring", a[0]))
			start := int(ev.num(pos, "substring", a[1]))
			end := len(r)
			if len(a) == 3 {
				end = int(ev.num(pos, "substring", a[2]))
			}
			start, end = clamp(start, len(r)), clamp(end, len(r))
			if start > end {
				return ""
			}
			return string(r[start:end])
		}},
		"regex_match": {2, 2, func(ev *evaluator, pos int, a []any) any {
			return ev.compileRegex(pos, ev.str(pos, "regex_match", a[1])).MatchString(ev.str(pos, "regex_match", a[0]))
		}},
		"regex_replace": {3, 3, func(ev *evaluator, pos int, a []any) any {
			return ev.compileRegex(pos, ev.str(pos, "regex_replace", a[1])).ReplaceAllString(ev.str(pos, "regex_replace", a[0]), ev.str(pos, "regex_replace", a[2]))
		}},
		"base64_encode": {1, 1, func(ev *evaluator, pos int, a []any) any {
			return base64.StdEncoding.EncodeToString([]byte(ev.str(pos, "base64_encode", a[0])))
		}},
		"base64_decode": {1, 1, func(ev *evaluator, pos int, a []any) any {
			b, err := base64.StdEncoding.DecodeString(ev.str(pos, "base64_decode", a[0]))
			if err != nil {
				ev.fail(pos, "base64_decode(): %v", err)
			}
			return string(b)
		}},
		"url_encode": {1, 1, func(ev *evaluator, pos int, a []any) any { return url.QueryEscape(ev.str(pos, "url_encode", a[0])) }},

		// numbers
		"abs":   numFn("abs", math.Abs),
		"floor": numFn("floor", math.Floor),
		"ceil":  numFn("ceil", math.Ceil),
		"round": {1, 2, func(ev *evaluator, pos int, a []any) any {
			f := ev.num(pos, "round", a[0])
			digits := 0.0
			if len(a) == 2 {
				digits = ev.num(pos, "round", a[1])
			}
			p := math.Pow(10, digits)
			return math.Round(f*p) / p
		}},
		"min": {1, -1, func(ev *evaluator, pos int, a []any) any { return reduceNum(ev, pos, "min", a, math.Min) }},
		"max": {1, -1, func(ev *evaluator, pos int, a []any) any { return reduceNum(ev, pos, "max", a, math.Max) }},
		"sum": {1, 1, func(ev *evaluator, pos int, a []any) any {
			total := 0.0
			for _, e := range ev.arr(pos, "sum", a[0]) {
				total += ev.num(pos, "sum", e)
			}
			return total
		}},

		// collections
		"keys": {1, 1, func(ev *evaluator, pos int, a []any) any {
			m := ev.obj(pos, "keys", a[0])
			out := []any{}
			for _, k := range sortedKeys(m) {
				out = append(out, k)
			}
			return out
		}},
		"values": {1, 1, func(ev *evaluator, pos int, a []any) any {
			m := ev.obj(pos, "values", a[0])
			out := []any{}
			for _, k := range sortedKeys(m) {
				out = append(out, m[k])
			}
			return out
		}},
		"has": {2, 2, func(ev *evaluator, pos int, a []any) any {
			_, ok := ev.obj(pos, "has", a[0])[ev.str(pos, "has", a[1])]
			return ok
		}},
		"first": {1, 1, func(ev *evaluator, pos int, a []any) any {
			if arr := ev.arr(pos, "first", a[0]); len(arr) > 0 {
				return arr[0]
			}
			return nil
		}},
		"last": {1, 1, func(ev *evaluator, pos int, a []any) any {
			if arr := ev.arr(pos, "last", a[0]); len(arr) > 0 {
				return arr[len(arr)-1]
			}
			return nil
		}},
		"reverse": {1, 1, func(ev *evaluator, pos int, a []any) any {
			arr := ev.arr(pos, "reverse", a[0])
			out := make([]any, len(arr))
			for i, e := range arr {
				out[len(arr)-1-i] = e
			}
			return out
		}},
		"slice": {2, 3, func(ev *evaluator, pos int, a []any) any {
			arr := ev.arr(pos, "slice", a[0])
			start, end := int(ev.num(pos, "slice", a[1])), len(arr)
			if len(a) == 3 {
				end = int(ev.num(pos, "slice", a[2]))
			}
			start, end = clamp(start, len(arr)), clamp(end, len(arr))
			if start > end {
				return []any{}
			}
			return append([]any{}, arr[start:end]...)
		}},
		"sort": {1, 1, func(ev *evaluator, pos int, a []any) any {
			arr := append([]any{}, ev.arr(pos, "sort", a[0])...)
			var bad bool
			sort.SliceStable(arr, func(i, j int) bool {
				switch x := arr[i].(type) {
				case float64:
					y, ok := arr[j].(float64)
					bad = bad || !ok
					return ok && x < y
				case string:
					y, ok := arr[j].(string)
					bad = bad || !ok
					return ok && x < y
				}
				bad = true
				return false
			})
			if bad {
				ev.fail(pos, "sort() needs an array of only numbers or only strings")
			}
			return arr
		}},
		"unique": {1, 1, func(ev *evaluator, pos int, a []any) any {
			out := []any{}
			for _, e := range ev.arr(pos, "unique", a[0]) {
				dup := false
				for _, o := range out {
					if equal(o, e) {
						dup = true
						break
					}
				}
				if !dup {
					out = append(out, e)
				}
			}
			return out
		}},
		"range": {1, 2, func(ev *evaluator, pos int, a []any) any {
			start, end := 0, int(ev.num(pos, "range", a[0]))
			if len(a) == 2 {
				start, end = end, int(ev.num(pos, "range", a[1]))
			}
			if end-start > ev.lim.MaxRange {
				ev.fail(pos, "range() is limited to %d elements", ev.lim.MaxRange)
			}
			out := []any{}
			for i := start; i < end; i++ {
				out = append(out, float64(i))
			}
			return out
		}},
		"concat": {1, -1, func(ev *evaluator, pos int, a []any) any {
			out := []any{}
			for _, x := range a {
				out = append(out, ev.arr(pos, "concat", x)...)
			}
			return out
		}},
		"merge": {1, -1, func(ev *evaluator, pos int, a []any) any {
			out := map[string]any{}
			for _, x := range a {
				for k, v := range ev.obj(pos, "merge", x) {
					out[k] = v
				}
			}
			return out
		}},
		"pluck": {2, 2, func(ev *evaluator, pos int, a []any) any {
			key := ev.str(pos, "pluck", a[1])
			out := []any{}
			for _, e := range ev.arr(pos, "pluck", a[0]) {
				if m, ok := e.(map[string]any); ok {
					out = append(out, m[key])
				} else {
					out = append(out, nil)
				}
			}
			return out
		}},

		// time
		"format_time": {2, 2, func(ev *evaluator, pos int, a []any) any {
			t, err := time.Parse(time.RFC3339Nano, ev.str(pos, "format_time", a[0]))
			if err != nil {
				ev.fail(pos, "format_time(): %v", err)
			}
			return t.UTC().Format(ev.str(pos, "format_time", a[1]))
		}},
		"add_seconds": {2, 2, func(ev *evaluator, pos int, a []any) any {
			t, err := time.Parse(time.RFC3339Nano, ev.str(pos, "add_seconds", a[0]))
			if err != nil {
				ev.fail(pos, "add_seconds(): %v", err)
			}
			return t.Add(time.Duration(ev.num(pos, "add_seconds", a[1]) * float64(time.Second))).UTC().Format(time.RFC3339Nano)
		}},
	}
}

func clamp(i, n int) int {
	if i < 0 {
		i += n
	}
	if i < 0 {
		return 0
	}
	if i > n {
		return n
	}
	return i
}

func reduceNum(ev *evaluator, pos int, name string, a []any, f func(x, y float64) float64) any {
	vals := a
	if len(a) == 1 {
		vals = ev.arr(pos, name, a[0])
	}
	if len(vals) == 0 {
		return nil
	}
	acc := ev.num(pos, name, vals[0])
	for _, v := range vals[1:] {
		acc = f(acc, ev.num(pos, name, v))
	}
	return acc
}

// FunctionNames lists built-in functions (for docs and the editor).
func FunctionNames() []string {
	out := make([]string, 0, len(funcs))
	for k := range funcs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
