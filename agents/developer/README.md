# `developer`

Standard autosk agent: implements a task from `new` → reviewable code.
First step of the `feature-dev` workflow.

| Setting | Value |
|---|---|
| `model` | `sonnet:high` |
| `thinking` | `high` |
| `first_message_file` | [`./prompts/first_message.md`](./prompts/first_message.md) |
| `runner` | (none — standard pi-spawn agent) |

Outgoing transition (in `feature-dev`): `→ review`.

## Install

```bash
autosk agent install ./agents/developer
```

See [`../README.md`](../README.md) for the full picture.
