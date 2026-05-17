# pi-autosk

pi extension that wraps the [autosk](../README.md) CLI as a single LLM-callable
tool. The agent stops shelling out to `bash autosk ...` and gets a structured,
rendered task tracker instead.

## Layout

```
extension/
├── package.json          # pi.extensions → ./src/index.ts
├── tsconfig.json
└── src/
    ├── index.ts          # extension factory
    ├── tool.ts           # registerTool() + tool description for the LLM
    ├── actions.ts        # dispatch per action; build argv, parse JSON
    ├── autosk-cli.ts     # spawn + error normalization
    ├── schema.ts         # TypeBox params: { action, args }
    ├── render.ts         # renderCall / renderResult (compact tables)
    └── types.ts
```

## Tool surface

One tool, `autosk`, parameters `{ action, args }`. `action` is a whitelist:

| action     | required args                              | notes |
|------------|--------------------------------------------|-------|
| `ready`    | —                                          | `args.limit?`. Returns new + unblocked tasks, priority-sorted. |
| `next`     | —                                          | Alias for `ready --limit 1`. |
| `list`     | —                                          | `args.status_filter?`, `args.priority_filter?`, `args.limit?`. Default filter: `new,claimed`. Use `status_filter: "all"` for everything. |
| `show`     | `id`                                       | Full task with `blocked_by` / `blocks`. |
| `create`   | `title`                                    | Plus `description?`, `priority?` (0..3), `blocks?`, `blocked_by?`. |
| `update`   | `id` + at least one of `title` / `description` / `priority` / `status` | Any → any status. |
| `claim`    | `id`                                       | Idempotent. |
| `done`     | `id`                                       | |
| `cancel`   | `id`                                       | |
| `reopen`   | `id`                                       | done/cancelled → new. |
| `block`    | `id`, `blocker_ids[]`                      | Each blocker blocks `id`. |
| `unblock`  | `id`, `blocker_ids[]` or `all: true`       | |
| `dep_list` | `id`                                       | Returns the task with its dep arrays. |
| `init`     | —                                          | Explicit DB init; `args.prefix?`. Optional — write commands auto-init. |

Read commands and most write commands run with `--json` and return parsed
tasks under `details.task` or `details.tasks`. `block` / `unblock` / `dep_list`
have no native JSON output yet — the extension follows them with
`autosk show --json <id>` so the agent always sees the post-mutation state.

Errors are normalized to `details = { kind: "error", reason, message, stderr?, stdout? }`,
where `reason ∈ { "missing_binary" | "invalid_args" | "cli_error" | "parse_error" | "aborted" }`.

## Renderer

- `renderCall`: one-liner — `autosk <action>` + the salient arg (`as-a1b2`,
  `"title"`, `--all`, `limit=5`, …).
- `renderResult`:
  - `kind: "tasks"` → compact `ID | P | STATUS | TITLE [⛔]` table.
  - `kind: "task"`  → header line + title + description + dep summary.
  - `kind: "ack"`   → init message.
  - `kind: "error"` → red one-liner + dim detail.

## Install (development)

The `autosk` binary must be on `$PATH` (build from repo root via `make build`,
then put `./bin` on PATH or symlink `./bin/autosk` into `/usr/local/bin`).

Then load the extension in pi. Pick one:

```bash
# 1. Per-project: drop a symlink so pi auto-discovers it.
mkdir -p .pi/extensions
ln -s ~/me/dev/autosk/extension .pi/extensions/autosk

# 2. Global: same, under your pi config dir.
mkdir -p ~/.pi/extensions
ln -s ~/me/dev/autosk/extension ~/.pi/extensions/autosk

# 3. Explicit: add to ~/.pi/config.json
#    { "extensions": ["~/me/dev/autosk/extension"] }
```

pi reads `package.json#pi.extensions` and loads `./src/index.ts` from this
directory.

## Typecheck

```bash
cd extension
npm install
npm run typecheck
```
