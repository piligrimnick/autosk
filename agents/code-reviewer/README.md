# `code-reviewer`

Standard autosk agent: reviews the `developer`'s changes; routes back
to `developer` on issues, or forward to `task-validator` when clean.
Second step of the `feature-dev` workflow.

| Setting | Value |
|---|---|
| `model` | `sonnet:high` |
| `thinking` | `medium` |
| `first_message_file` | [`./prompts/first_message.md`](./prompts/first_message.md) |
| `runner` | (none — standard pi-spawn agent) |

Outgoing transitions (in `feature-dev`): `→ validator` (clean), `→ dev`
(bounce-back).

## Install

```bash
autosk agent install ./agents/code-reviewer
```

See [`../README.md`](../README.md) for the full picture.
