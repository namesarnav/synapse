# ADR 0005: A small purpose-built expression language

Status: accepted

## Context
Nodes need to reference earlier outputs, the trigger payload and secrets, and conditions need arithmetic and comparison. Workflows are authored by users and run on shared workers, so evaluating arbitrary code is off the table.

## Decision
A hand-written lexer, Pratt parser and tree-walking evaluator in `internal/expressions`. No `eval`, no reflection, no host access. Values are JSON values. Evaluation is bounded by step count, output size and range length (`Limits`). A static checker walks the AST at validation time to reject unknown functions, bad arity and references to nodes that are not upstream, so most mistakes fail at publish rather than at run time. Templates (`{{ ... }}` inside strings) share the same evaluator. The language is documented in `docs/expressions.md`.

## Consequences
- The attack surface is the parser and evaluator, both fuzzed (`FuzzParse`) and limit-bounded.
- Features are added deliberately; there are no user-defined functions or loops.
- `secrets.NAME` is resolved by the worker at run time and never reaches the stored graph or events.
