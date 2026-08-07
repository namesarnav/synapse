package expressions

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
)

// Values are JSON-shaped: nil, bool, float64, string, []any, map[string]any.
// Normalize converts other numeric types and typed slices/maps to that shape.

func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

// Normalize converts arbitrary JSON-compatible Go values into the canonical
// value shape used by the evaluator.
func Normalize(v any) any {
	switch t := v.(type) {
	case nil, bool, float64, string:
		return t
	case int:
		return float64(t)
	case int32:
		return float64(t)
	case int64:
		return float64(t)
	case uint:
		return float64(t)
	case uint32:
		return float64(t)
	case uint64:
		return float64(t)
	case float32:
		return float64(t)
	case json.Number:
		f, _ := t.Float64()
		return f
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = Normalize(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = Normalize(e)
		}
		return out
	case json.RawMessage:
		var x any
		if json.Unmarshal(t, &x) == nil {
			return Normalize(x)
		}
		return string(t)
	case []string:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = e
		}
		return out
	}
	// fall back through JSON for structs and other types
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	var x any
	if err := json.Unmarshal(raw, &x); err != nil {
		return fmt.Sprint(v)
	}
	return Normalize(x)
}

// Truthy: null, false, 0 and "" are falsy; everything else is truthy.
func Truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0 && !math.IsNaN(t)
	case string:
		return t != ""
	}
	return true
}

func equal(a, b any) bool {
	switch x := a.(type) {
	case nil:
		return b == nil
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case float64:
		y, ok := b.(float64)
		return ok && x == y
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !equal(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, xv := range x {
			yv, ok := y[k]
			if !ok || !equal(xv, yv) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a, b)
}

func formatNumber(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// ToString renders a value for template interpolation: strings verbatim,
// null as the empty string, composites as compact JSON with sorted keys.
func ToString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return formatNumber(t)
	}
	return jsonString(v)
}

func jsonString(v any) string {
	raw, err := json.Marshal(sortKeys(v))
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(raw)
}

// sortKeys is a no-op for encoding/json (it already sorts map keys); kept as a
// seam so number formatting stays integral in JSON output.
func sortKeys(v any) any {
	switch t := v.(type) {
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e15 {
			return int64(t)
		}
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = sortKeys(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = sortKeys(e)
		}
		return out
	}
	return v
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// size approximates the memory footprint of a value; used to bound results.
func size(v any) int {
	switch t := v.(type) {
	case string:
		return len(t) + 16
	case []any:
		n := 24
		for _, e := range t {
			n += size(e)
		}
		return n
	case map[string]any:
		n := 48
		for k, e := range t {
			n += len(k) + size(e)
		}
		return n
	}
	return 16
}
