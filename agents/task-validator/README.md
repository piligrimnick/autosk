# `task-validator`

Standard autosk agent: final automated gate before handing the task to
a human. Third (last) step of the `feature-dev` workflow.

| Setting | Value |
|---|---|
| `model` | `sonnet:high` |
| `thinking` | `medium` |
| `first_message_file` | [`./prompts/first_message.md`](./prompts/first_message.md) |
| `runner` | (none — standard pi-spawn agent) |

Outgoing transitions (in `feature-dev`): `→ dev` (bounce-back), `→
human_feedback` (clean). There is intentionally no `→ done` — the
human owns the final close.

## Install

```bash
autosk agent install ./agents/task-validator
```

See [`../README.md`](../README.md) for the full picture.
