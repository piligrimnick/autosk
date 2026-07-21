# @autosk/gh-review

Review a GitHub issue or pull request from a task: enroll a task whose **title
carries a GitHub issue/PR URL** (`https://github.com/<owner>/<repo>/issues/9`
or `…/pull/12`), and a pi agent reviews it inside a **per-task
`docker run -i --rm` container** with a **READ-ONLY `gh`** — the verdict lands
as a structured comment on the autosk task. Nothing is ever written to GitHub
(the token's scopes reject every write with 403, and the prompt never tries).

It registers ONE workflow, **`gh-review`**:

```text
review ──▶ accept (human) ──▶ cleanup ──▶ done
```

- `review` — `piAgent` (xhigh) in a per-task `dockerSandbox` container
  (`ghcr.io/wierdbytes/pi-runtime`, which ships `gh`); it reads the issue/PR
  with `gh` (+ a clone when the diff is not enough), reviews it, posts the
  review as one task comment, and transits to `accept`.
- `accept` — `statusStep("human")`; the task parks for you to read the review.
- `cleanup` — `sandboxCleanupStep`: removes the per-task worktree/container,
  then transits to `done` (`autosk resume <id> --to cleanup`).

The enroll is **validated before any agent runs** (`onTransit`): a task with no
parsable GitHub URL — or a daemon with no gh config — is rejected with an
explanatory comment and stays `new`.

## Setup: read-only gh

1. On GitHub, create a **fine-grained personal access token**:
   - *Repository access*: only the repositories you want reviewed.
   - *Permissions* (all **Read-only**): `Contents`, `Issues`, `Pull requests`,
     `Metadata`. (`Contents: Read` is what allows `gh repo clone` for a deeper
     PR review; drop it for a diff-only token.)
2. Store it in `~/.autosk/github/ro-token.json` (override the dir with
   `AUTOSK_GH_DIR`):

   ```json
   { "token": "github_pat_…" }
   ```

   ```bash
   chmod 600 ~/.autosk/github/ro-token.json
   ```

3. Done. The extension reads the file at container-start and passes the token
   as the `GH_TOKEN` env — gh's native token mechanism: no gh config file
   exists in the container, so gh never tries to rewrite one (gh ≥2.40 rewrites
   `hosts.yml` on most invocations — a read-only config mount does not work),
   and `GH_NO_UPDATE_NOTIFIER=1` / `CHECKPOINT_DISABLE=1` keep gh from writing
   anything else. The token is visible in `docker inspect` while the per-task
   container lives (minutes); read-only *access* is enforced by the token's
   scopes on GitHub's side (a write → 403). Rotating the token in the file
   takes effect on the next run — no daemon restart.

## Use it

```bash
# 1. build (or pull) the pi-runtime image (ships gh)
daemon/extensions/pi-agent/docker/build.sh

# 2. install this extension (hot-applies to open projects, no restart)
autosk ext add npm:@autosk/gh-review      # or: autosk ext add /path/to/gh-review

# 3. enroll a task whose title carries the URL
id=$(autosk create "https://github.com/wierdbytes/autosk/pull/12" --workflow gh-review --json | jq -r .id)

# the daemon runs the review and parks the task at `accept`;
# read the review comment, then route it through cleanup:
autosk resume "$id" --to cleanup          # → done (worktree/container removed)
```

The host project must be a git repo (the sandbox is a per-task worktree, same
as `feature-dev`). If the URL points at a *different* repository, the agent
clones it inside the container (gone when the container exits).

Env knobs (all optional):

| var | default | what |
|-----|---------|------|
| `AUTOSK_GH_REVIEW_IMAGE` | `ghcr.io/wierdbytes/pi-runtime:latest` | image to run (must ship `gh`) |
| `AUTOSK_GH_DIR` | `~/.autosk/github` | dir holding `ro-token.json` (read fresh per run) |
| `AUTOSK_PI_DIR` | `~/.pi` | host pi config (auth + models) bind-mounted into the container |

## Exports

- default — the extension factory (registers `gh-review`).
- `ghReviewWorkflow(opts)` / `ghReviewSandbox()` — compose your own workflow
  over the same docker sandbox.
- `ghReviewGuard` — the `onTransit` enroll validator (URL + gh token).
- `parseGhTarget(text)` — the title/description URL parser.
- `defaultDockerImage()` / `ghConfigDir()` / `ghTokenFile()` / `readGhToken()` / `checkGhToken()`.
