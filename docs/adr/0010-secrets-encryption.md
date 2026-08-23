# ADR 0010: Secrets are encrypted at rest and redacted on the way out

Status: accepted

## Context
Workflows call external APIs with credentials. Credentials must not sit in workflow graphs, logs, events or API responses.

## Decision
Secrets are stored per workspace, encrypted with AES-256-GCM under a master key from `SYNAPSE_MASTER_KEY` (required in production), with the workspace and secret name bound as additional data. Graphs refer to them as `secrets.NAME`; the worker resolves the value just before executing the node. The worker passes node output and error messages through a per-workspace redactor that masks resolved secret values as `***` before anything is stored, so events, execution detail and replays carry only the masked text. The API never returns a secret value, only names and metadata; writing requires the admin role.

## Consequences
- A database dump alone does not reveal secrets.
- Redaction is value-based: a secret transformed by the node (for example base64-encoded) is not recognised. Users should not echo credentials into outputs.
- Structured logs do not include node output; the logger also accepts a redactor for defence in depth.
- Rotating the master key requires re-encrypting rows; there is no online rotation yet.
