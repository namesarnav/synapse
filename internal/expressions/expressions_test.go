package expressions

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func env() *Env {
	return &Env{
		Vars: map[string]any{
			"trigger": map[string]any{"body": map[string]any{"name": "Ada", "n": 3.0, "tags": []any{"a", "b"}}},
			"nodes":   map[string]any{"fetch": map[string]any{"status": 200.0, "data": []any{1.0, 2.0, 3.0}}},
			"item":    "x",
			"index":   2.0,
		},
		Secret: func(n string) (string, bool) {
			if n == "API_KEY" {
				return "s3cret", true
			}
			return "", false
		},
		Now: func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
	}
}

func eval(t *testing.T, src string) any {
	t.Helper()
	v, err := EvalString(src, env())
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	return v
}

func TestEvaluation(t *testing.T) {
	cases := []struct {
		src  string
		want any
	}{
		{`1 + 2 * 3`, 7.0},
		{`(1 + 2) * 3`, 9.0},
		{`10 % 4`, 2.0},
		{`10 / 4`, 2.5},
		{`-3 + 5`, 2.0},
		{`"a" + "b"`, "ab"},
		{`[1] + [2]`, []any{1.0, 2.0}},
		{`1 < 2 && 2 < 3`, true},
		{`1 > 2 || 3 > 2`, true},
		{`1 > 2 or 3 > 2`, true},
		{`not true`, false},
		{`!false`, true},
		{`1 == 1.0`, true},
		{`"a" != "b"`, true},
		{`[1,2] == [1,2]`, true},
		{`{a: 1} == {a: 1}`, true},
		{`null == null`, true},
		{`null ?? "d"`, "d"},
		{`0 ?? "d"`, 0.0},
		{`true ? "y" : "n"`, "y"},
		{`false ? "y" : true ? "z" : "n"`, "z"},
		{`2 in [1,2,3]`, true},
		{`"a" in {a: 1}`, true},
		{`"ell" in "hello"`, true},
		{`trigger.body.name`, "Ada"},
		{`trigger.body.missing.deeper`, nil},
		{`trigger.body?.name`, "Ada"},
		{`trigger.body.tags[1]`, "b"},
		{`trigger.body.tags[-1]`, "b"},
		{`trigger.body.tags[9]`, nil},
		{`nodes.fetch.data[0] + nodes["fetch"].status`, 201.0},
		{`item + to_string(index)`, "x2"},
		{`length(trigger.body.tags)`, 2.0},
		{`upper(trigger.body.name)`, "ADA"},
		{`join(pluck([{a:1},{a:2}], "a"), "-")`, "1-2"},
		{`sum(nodes.fetch.data)`, 6.0},
		{`max(1, 5, 3)`, 5.0},
		{`min([4, 2, 9])`, 2.0},
		{`round(2.567, 2)`, 2.57},
		{`substring("hello", 1, 3)`, "el"},
		{`range(3)`, []any{0.0, 1.0, 2.0}},
		{`sort([3,1,2])`, []any{1.0, 2.0, 3.0}},
		{`unique([1,1,2])`, []any{1.0, 2.0}},
		{`default(null, 5)`, 5.0},
		{`coalesce(null, null, 7)`, 7.0},
		{`regex_match("abc123", "^[a-z]+[0-9]+$")`, true},
		{`json_parse("{\"a\":[1,2]}").a[1]`, 2.0},
		{`json_stringify({b: 1, a: 2})`, `{"a":2,"b":1}`},
		{`base64_decode(base64_encode("hi"))`, "hi"},
		{`now()`, "2026-01-02T03:04:05Z"},
		{`type_of(null)`, "null"},
		{`"line\nbreak"`, "line\nbreak"},
		{`'single'`, "single"},
		{`1e3`, 1000.0},
		{`{"quoted key": 1}["quoted key"]`, 1.0},
	}
	for _, c := range cases {
		got := eval(t, c.src)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s = %#v, want %#v", c.src, got, c.want)
		}
	}
}

func TestShortCircuit(t *testing.T) {
	// The right side would fail if evaluated.
	for _, src := range []string{`false && (1/0 > 0)`, `true || (1/0 > 0)`, `1 ?? (1/0)`, `true ? 1 : (1/0)`} {
		if _, err := EvalString(src, env()); err != nil {
			t.Errorf("%s: %v", src, err)
		}
	}
}

func TestRuntimeErrors(t *testing.T) {
	for _, src := range []string{
		`1 / 0`, `1 % 0`, `"a" + 1`, `1 + "a"`, `"a" < 1`, `unknown_fn(1)`, `length()`, `upper(1)`,
		`to_number("abc")`, `-"a"`, `[1][ "a" ]`, `range(100000)`, `regex_match("a", "(")`, `unknownvar.x + 1`,
	} {
		if _, err := EvalString(src, env()); err == nil {
			t.Errorf("%s: expected error", src)
		}
	}
}

func TestSyntaxErrors(t *testing.T) {
	for _, src := range []string{``, `1 +`, `(1`, `[1,`, `{a:}`, `a.`, `1 2`, `"unterminated`, `@`, `a ? b`, `f(1,)x`, `1 = 1`, `\`} {
		_, err := Parse(src)
		var ee *Error
		if err == nil {
			// `a.` and similar must fail; a few others may be lenient by design
			t.Errorf("%q: expected syntax error", src)
			continue
		}
		if !errors.As(err, &ee) {
			t.Errorf("%q: error is %T, want *Error", src, err)
		}
	}
}

func TestPrecedence(t *testing.T) {
	cases := map[string]any{
		`1 + 2 == 3`:             true,
		`1 < 2 == true`:          true,
		`true || false && false`: true,
		`2 * 3 + 4 * 5`:          26.0,
		`-2 * -3`:                6.0,
		`1 ?? 2 ?? 3`:            1.0,
		`null ?? null ?? 3`:      3.0,
	}
	for src, want := range cases {
		if got := eval(t, src); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %#v, want %#v", src, got, want)
		}
	}
}

func TestLimits(t *testing.T) {
	// Source length
	if _, err := Parse(strings.Repeat("1+", MaxSourceLen) + "1"); err == nil {
		t.Error("oversized source accepted")
	}
	// Nesting depth
	if _, err := Parse(strings.Repeat("(", 500) + "1" + strings.Repeat(")", 500)); err == nil {
		t.Error("deep nesting accepted")
	}
	if _, err := Parse(strings.Repeat("[", 500) + strings.Repeat("]", 500)); err == nil {
		t.Error("deep array nesting accepted")
	}
	if _, err := Parse(strings.Repeat("!", 500) + "1"); err == nil {
		t.Error("deep unary chain accepted")
	}
	// Step limit
	e, err := Parse(strings.Repeat("1+", 1500) + "1")
	if err != nil {
		t.Fatal(err)
	}
	v := env()
	v.Limits = Limits{MaxSteps: 100, MaxBytes: 1 << 20, MaxRange: 100}
	if _, err := e.Eval(v); err == nil || !strings.Contains(err.Error(), "step") {
		t.Errorf("step limit not enforced: %v", err)
	}
	// Size limit via repeated doubling
	v = env()
	v.Limits = Limits{MaxSteps: 200000, MaxBytes: 1024, MaxRange: 100}
	e, _ = Parse(`join(range(100), "0123456789abcdef")`)
	if _, err := e.Eval(v); err == nil {
		t.Error("size limit not enforced for join")
	}
	e, _ = Parse(`[1,2,3] + [4,5,6]`)
	v.Limits = Limits{MaxSteps: 1000, MaxBytes: 16, MaxRange: 100}
	if _, err := e.Eval(v); err == nil {
		t.Error("size limit not enforced for array concat")
	}
	// Range cap
	if _, err := EvalString(`range(10001)`, env()); err == nil {
		t.Error("range cap not enforced")
	}
}

func TestNoHostAccess(t *testing.T) {
	// No function reaches the filesystem, env or network; unknown names fail.
	for _, src := range []string{`os.Getenv("HOME")`, `read_file("/etc/passwd")`, `exec("id")`, `eval("1")`, `__proto__`, `require("fs")`} {
		v, err := EvalString(src, env())
		if err == nil && v != nil {
			t.Errorf("%s produced %v", src, v)
		}
	}
	for _, n := range FunctionNames() {
		for _, bad := range []string{"exec", "eval", "read", "write", "open", "env", "http", "fetch", "system"} {
			if strings.Contains(n, bad) {
				t.Errorf("function %s looks dangerous", n)
			}
		}
	}
}

func TestRegexIsLinearTime(t *testing.T) {
	// Catastrophic backtracking pattern must return promptly under RE2.
	start := time.Now()
	_, _ = EvalString(`regex_match(repeat_a, "^(a+)+$")`, &Env{Vars: map[string]any{"repeat_a": strings.Repeat("a", 5000) + "b"}})
	if time.Since(start) > 2*time.Second {
		t.Errorf("regex took %v", time.Since(start))
	}
}

func TestSecrets(t *testing.T) {
	e := env()
	e.SecretsUsed = map[string]bool{}
	v, err := EvalString(`"Bearer " + secrets.API_KEY`, e)
	if err != nil || v != "Bearer s3cret" {
		t.Fatalf("got %v %v", v, err)
	}
	if !e.SecretsUsed["API_KEY"] {
		t.Error("secret use not tracked")
	}
	if _, err := EvalString(`secrets.MISSING`, env()); err == nil {
		t.Error("missing secret should error")
	}
	// Without a resolver no secret is readable.
	if _, err := EvalString(`secrets.API_KEY`, &Env{Vars: map[string]any{}}); err == nil {
		t.Error("secrets available without resolver")
	}
	// Secrets cannot be enumerated or passed around as a whole object.
	if v, err := EvalString(`keys(secrets)`, env()); err == nil {
		t.Errorf("secrets enumerable: %v", v)
	}
}

func TestTemplates(t *testing.T) {
	cases := []struct {
		src  string
		want any
	}{
		{`hello {{ trigger.body.name }}!`, "hello Ada!"},
		{`{{ trigger.body.n }}`, 3.0},
		{`{{ trigger.body.tags }}`, []any{"a", "b"}},
		{`n={{ trigger.body.n }} m={{ 1 + 1 }}`, "n=3 m=2"},
		{`plain text`, "plain text"},
		{`\{{ literal }}`, "{{ literal }}"},
		{`{{ "}}" }}`, "}}"},
		{`{{ {a: 1}.a }}`, 1.0},
		{`{{ null }}x`, "x"},
		{`{{ trigger.body.missing }}`, nil},
		{``, ""},
	}
	for _, c := range cases {
		got, err := RenderTemplate(c.src, env())
		if err != nil {
			t.Errorf("%q: %v", c.src, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q = %#v, want %#v", c.src, got, c.want)
		}
	}
	for _, bad := range []string{`{{ 1 + }}`, `{{ unterminated`, `a {{ }} b`} {
		if _, err := ParseTemplate(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestResolve(t *testing.T) {
	in := map[string]any{
		"url":  "https://x/{{ trigger.body.name }}",
		"list": []any{"{{ index }}", 5.0, true, nil},
		"deep": map[string]any{"v": "{{ item }}"},
	}
	got, err := Resolve(in, env())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"url":  "https://x/Ada",
		"list": []any{2.0, 5.0, true, nil},
		"deep": map[string]any{"v": "x"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v", got)
	}
	// input untouched
	if in["url"] != "https://x/{{ trigger.body.name }}" {
		t.Error("Resolve mutated its input")
	}
}

func TestReferences(t *testing.T) {
	e, _ := Parse(`nodes.a.x + nodes["b"].y + length(nodes.a.z) + secrets.KEY + trigger.q`)
	r := e.References()
	if !reflect.DeepEqual(r.Nodes, []string{"a", "b"}) || !reflect.DeepEqual(r.Secrets, []string{"KEY"}) || r.Dynamic {
		t.Errorf("refs = %+v", r)
	}
	e, _ = Parse(`nodes[trigger.which].x`)
	if !e.References().Dynamic {
		t.Error("dynamic node ref not flagged")
	}
	if _, err := (Checker{}).CheckExpression(`nodes[item].x`); err == nil {
		t.Error("dynamic reference should fail the checker")
	}
	refs, err := (Checker{}).CheckTemplate(`a {{ nodes.a.v }} b {{ nodes.c.v }}`)
	if err != nil || !reflect.DeepEqual(refs, []string{"a", "c"}) {
		t.Errorf("template refs = %v %v", refs, err)
	}
	if _, err := (Checker{}).CheckExpression(`nosuchfn(1)`); err == nil {
		t.Error("unknown function not caught statically")
	}
	if _, err := (Checker{}).CheckExpression(`length(1, 2)`); err == nil {
		t.Error("arity not caught statically")
	}
}

func TestNormalizeAndToString(t *testing.T) {
	if got := Normalize(map[string]any{"a": 1, "b": []int{1}, "c": int64(3)}); got == nil {
		t.Fatal("nil")
	}
	for in, want := range map[any]string{nil: "", true: "true", 1.0: "1", 1.5: "1.5", "s": "s", 1e21: "1e+21"} {
		if got := ToString(in); got != want {
			t.Errorf("ToString(%v) = %q, want %q", in, got, want)
		}
	}
	if got := ToString(map[string]any{"b": 1.0, "a": 2.0}); got != `{"a":2,"b":1}` {
		t.Errorf("object = %s", got)
	}
}

func TestTruthy(t *testing.T) {
	for _, v := range []any{nil, false, 0.0, ""} {
		if Truthy(v) {
			t.Errorf("%#v should be falsy", v)
		}
	}
	for _, v := range []any{true, 1.0, "a", []any{}, map[string]any{}} {
		if !Truthy(v) {
			t.Errorf("%#v should be truthy", v)
		}
	}
}

func FuzzParseEval(f *testing.F) {
	for _, s := range []string{`1+2`, `a.b[0]`, `f(1,[2])`, `{a:1}.a`, `x ? y : z`, `"sA"`, `nodes["a"]?.b ?? 1`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		e, err := Parse(src)
		if err != nil {
			return
		}
		_, _ = e.Eval(&Env{Vars: map[string]any{"a": map[string]any{"b": []any{1.0}}}, Limits: Limits{MaxSteps: 2000, MaxBytes: 1 << 16, MaxRange: 100}})
	})
}

func BenchmarkEval(b *testing.B) {
	e, _ := Parse(`trigger.body.n * 2 + length(trigger.body.tags) > 5 ? upper(trigger.body.name) : "no"`)
	en := env()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := e.Eval(en); err != nil {
			b.Fatal(err)
		}
	}
}
