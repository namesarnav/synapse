# Expression language

Synapse evaluates expressions with its own lexer, Pratt parser and tree-walking evaluator
(`internal/expressions`). There is no `eval`, no reflection and no access to the host: an
expression can only read the variables it is given and call the built-in functions below.

## Where expressions appear

- **Expressions** (bare): condition `expression`, foreach `items`, transform `expression`, delay `until`.
- **Templates**: any other string config value. `{{ expr }}` segments are evaluated and spliced in.
  A value that is exactly one `{{ expr }}` keeps its type (number, array, object); mixed text
  becomes a string. Use `\{{` for a literal `{{`.

## Variables

| Root        | Meaning                                                    |
|-------------|------------------------------------------------------------|
| `trigger`   | Payload of the trigger that started the execution          |
| `nodes`     | Outputs of upstream nodes: `nodes.fetch.body`, `nodes["fetch"]` |
| `item`, `index` | Current element and position inside a foreach body     |
| `execution` | `id`, `workflow_id`, `version`                              |
| `secrets`   | `secrets.NAME`, resolved at run time; values are redacted from stored data |

Node references must use a literal id so the validator can check that the node exists and is
upstream. `nodes[some_expression]` is rejected at publish time.

## Semantics

- Field access is lenient: a missing field or a `null` receiver yields `null`. `a?.b` is accepted as an alias.
- Arithmetic is strict. `+` works on number+number, string+string and array+array; use
  `to_string(x)` for mixed text. `/` and `%` by zero are errors.
- Truthiness: `null`, `false`, `0` and `""` are falsy; everything else is truthy.
- `&&`, `||`, `??` and `?:` short-circuit. `and`, `or`, `not` are keyword aliases.
- `in` tests membership in arrays, keys in objects and substrings in strings.
- Equality is deep for arrays and objects; numbers compare by value.
- Precedence (low to high): `?:`, `??`, `||`, `&&`, `== != in`, `< <= > >=`, `+ -`, `* / %`, unary.

## Limits

| Limit                         | Value       |
|-------------------------------|-------------|
| Source length                 | 8192 bytes  |
| Tokens                        | 4000        |
| Parse nesting depth           | 48          |
| Evaluation steps              | 200,000     |
| Produced string/array size    | 4 MB        |
| `range()` length              | 10,000      |
| Regex pattern length          | 512 (RE2, linear time) |

Exceeding a limit is an ordinary evaluation error and fails the node, not the process.

## Functions

Run `expressions.FunctionNames()` for the authoritative list. Groups:

- Generic: `length is_empty is_null type_of default coalesce now`
- Conversion: `to_string to_number to_bool json_parse json_stringify`
- Strings: `upper lower trim contains starts_with ends_with split join replace substring regex_match regex_replace base64_encode base64_decode url_encode`
- Numbers: `abs floor ceil round min max sum`
- Collections: `keys values has first last reverse slice sort unique range concat merge pluck`
- Time: `format_time add_seconds`

## Errors

Errors carry a kind (`syntax`, `runtime`, `limit`) and a byte position in the source, so the editor
can underline the fault.
