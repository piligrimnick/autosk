# `@autogent/generic`

The minimum-viable autosk agent: an empty `autosk.agent` block. Nothing
about the spawn is configured by the package — `pi` is invoked with the
user's own defaults (model, thinking level, system prompt, extensions,
skills) coming from `~/.pi/`.

Useful as:

- A starting point when you want pi-with-defaults to handle a task and
  don't want to author a real package yet.
- A diff baseline: copy this and add one field at a time to see what
  each `autosk.agent.*` knob actually changes.
- The `--agent` of last resort when you just need *some* agent the
  workflow can route to.

| Setting | Value | Effect |
|---|---|---|
| `model` | (none) | pi uses its default model |
| `thinking` | (none) | pi uses its default thinking level |
| `first_message` / `first_message_file` | (none) | nothing is prepended to the first user turn the executor sends |
| `extra_args` | (none) | no extra flags appended to the pi spawn |
| `pi_extensions` / `pi_skills` | (none) | only pi's own auto-loaded extensions/skills are available |
| `runner` | (none) | standard pi-spawn branch (no custom JS runner) |

The executor still wraps the user turn in the usual autosk preamble
("You are agent `\"@autogent/generic\"` on step `\"…\"` of workflow
`\"…\"` …" + task title + description + transition list). Only the
package-supplied prefix is empty.

## Install

```bash
autosk agent install ./agents/generic-agent
```

The agent registers in the project DB under its full npm name
`@autogent/generic`. Reference it from workflows / `--agent` accordingly:

```bash
autosk create "play with this" --agent @autogent/generic
```

See [`../README.md`](../README.md) for the full picture, and
[`../../docs/plans/20260518-Agent-Packages.md`](../../docs/plans/20260518-Agent-Packages.md)
for the package format reference.
