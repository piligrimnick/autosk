# Tutorial: run the autosk daemon yourself

Most of the time you never think about `autoskd` — the first `autosk` command
[auto-spawns one](daemon.md#auto-spawn-the-normal-path) and you forget it exists.
This tutorial takes the lid off. By the end you'll have:

- started **your own** `autoskd` in the foreground and watched its logs;
- pointed the `autosk` CLI at it over a private socket;
- driven a task through a workflow and watched the **live session** and its
  **pi-format transcript** as the engine ran it;
- served **two projects from the one daemon**, each selected by its `{cwd}`;
- exposed the daemon over **TCP with token auth** so the desktop GUI can connect
  remotely.

This is the learning-oriented companion to [docs/daemon.md](daemon.md) (the
complete reference) — when you want the full flag/RPC surface, go there; here we
just run things and watch them work.

> **Scope.** Everything happens in throwaway directories and a private socket, so
> nothing here touches an existing daemon or your real `~/.autosk/`. Delete the
> directories when you're done and it's gone.

---

## What you'll need

- The `autosk` CLI and the `autoskd` daemon. From a checkout of this repo:

  ```bash
  make build           # → bin/autosk   (CGO-free Go)
  make build-autoskd   # → bin/autoskd  (Bun, compiled standalone — embeds its runtime)
  ```

  A released install already has both on `PATH`. Building needs Go 1.25 and Bun;
  **running** `autoskd` needs neither — the compiled binary embeds the Bun
  runtime.
- The two binaries reachable as `autosk` / `autoskd`. `bin/` is **not** on
  `PATH` automatically; either `make install` (puts both side by side in
  `$GOBIN`) or, for this tutorial, just prepend the checkout's `bin/`:

  ```bash
  export PATH="$PWD/bin:$PATH"     # run from the repo root
  ```

  Auto-spawn finds `autoskd` as a **sibling of `autosk`**, so keeping them
  together is all it takes.

---

## Step 1: pick a private socket and a scratch project

A running daemon listens on a Unix socket. The default is
`~/.autosk/daemon.sock` — which a background daemon may already hold. To stay out
of its way, give this tutorial its own socket and tell every command to use it
with the **`AUTOSK_SOCK`** environment variable:

```bash
export AUTOSK_SOCK=/tmp/acme-daemon.sock
mkdir -p /tmp/acme-api && cd /tmp/acme-api
autosk init
```

```
initialized /tmp/acme-api/.autosk
```

`AUTOSK_SOCK` is read by **both** sides — `autoskd` listens there, and the CLI
connects there — so setting it once wires the whole tutorial together.

---

## Step 2: start a daemon you can watch

Normally the CLI spawns the daemon for you and detaches it. To *watch* it, start
it yourself. Run it in the background with its log going to a file so you can
`cat` it (in real life you'd give it its own terminal, or run it as a service):

```bash
AUTOSK_NO_AUTO_INSTALL=1 autoskd serve --sock "$AUTOSK_SOCK" >/tmp/acme-daemon.log 2>&1 &
```

```bash
cat /tmp/acme-daemon.log
```

```
[info] autoskd: AUTOSK_NO_AUTO_INSTALL set — skipping first-run extension install
[info] autoskd: TCP listening on 0.0.0.0:7077 (token auth)
[info] autoskd 0.0.0-dev: listening on /tmp/acme-daemon.sock
```

You have a daemon. Three things to notice in those log lines:

- **`AUTOSK_NO_AUTO_INSTALL=1`** keeps the run offline. Without it, a fresh
  daemon's first act is to npm-install the reference [`@autosk/feature-dev`
  workflow](workflows.md#the-reference-workflow-feature-dev) into your global
  `~/.autosk/`. We don't need it here, so we skip it.
- **TCP is on by default** at `0.0.0.0:7077` — we'll use that in Step 6. (If
  some other daemon already holds `7077` you'll instead see a non-fatal
  `bind tcp 0.0.0.0:7077` error and the daemon keeps serving UDS only — that's
  the [documented fallback](daemon.md#transports-auth-single-instance-idle-shutdown);
  Step 6 picks a port of its own.)
- **`serve` is the default verb.** `autoskd` and `autoskd serve` are the same
  thing.

> **Single instance.** Try starting a second daemon on the same socket
> (`autoskd serve --sock "$AUTOSK_SOCK"`) and it prints `autoskd: already running`
> and exits `0` — the bind is the lock, so a double-spawn is always harmless.

---

## Step 3: point the CLI at your daemon

Because `AUTOSK_SOCK` is exported, the CLI already targets your daemon. Confirm
the handshake with `version` — it reports the CLI build **and** the daemon it's
talking to:

```bash
autosk version
```

```
autosk v0.2.2 (f1c177e)
  backend:        autoskd
  daemon:         0.0.0-dev ()
  go:             go1.25.5 darwin/arm64
```

The `daemon:` line is filled in over RPC (`meta.version`); if it's blank, the CLI
couldn't reach a daemon at `$AUTOSK_SOCK`. (`autosk version` never spawns a
daemon — it's a pure read.)

---

## Step 4: drive a task and watch the session

Now the fun part: hand the daemon a task and watch its engine run it. We need a
workflow with a runnable step. So that this works **offline with no agent
harness**, we'll register a tiny in-process agent — a single TypeScript file
dropped into the project's `.autosk/extensions/`.

Create `.autosk/extensions/demo.ts`:

```ts
// .autosk/extensions/demo.ts — a tiny in-process agent, no external harness.
import { type AutoskAPI } from "@autosk/sdk";

export default function (autosk: AutoskAPI) {
  autosk.registerWorkflow({
    name: "demo",
    firstStep: "work",
    steps: {
      work: {
        async onRun(ctx) {
          ctx.log.custom("tutorial:note", { text: "starting work" });
          await new Promise((r) => setTimeout(r, 3000)); // pretend to do work
          await ctx.comment("processed by the demo agent");
          await ctx.transit({ status: "done" });
        },
      },
    },
  });
}
```

(The `import { type AutoskAPI }` is type-only — it compiles away, so the daemon
needs no `node_modules` to load this file. For the full extension story see the
[extension tutorial](extensions-tutorial.md).)

The daemon builds a project's extension registry **lazily** — the first command
that needs it discovers and caches it. We wrote `demo.ts` before any such command
ran, so it's picked up with no restart:

```bash
autosk workflow list
```

```
NAME  FIRST_STEP  STEPS
demo  work        work
```

(The registry is cached for the daemon's lifetime once built, so *editing* an
extension later **does** need a daemon restart — the next `autosk` command
re-reads the files. A brand-new file discovered on first access, like now, does
not.)

Create a task and enroll it into `demo` in one go, then **immediately** list
sessions — the 3-second delay in `onRun` gives you time to catch it live:

```bash
autosk create "Process the demo task" --workflow demo
autosk session list
```

```
ask-c82e17
SESSION                               TASK        STEP  AGENT  STATUS   ERROR
019efab5-5d50-72bc-b09a-4dd7d02a16da  ask-c82e17  work  work   running
```

There it is — `running`. The engine saw a `work`-status task whose current step
is an agent step with no live session, and started one. Wait a few seconds and
list again:

```bash
autosk session list
```

```
SESSION                               TASK        STEP  AGENT  STATUS  ERROR
019efab5-5d50-72bc-b09a-4dd7d02a16da  ask-c82e17  work  work   done
```

Now read the **transcript** of that session (use the SESSION id from your
output):

```bash
autosk session transcript 019efab5-5d50-72bc-b09a-4dd7d02a16da
```

```
== session 019efab5-5d50-72bc-b09a-4dd7d02a16da — demo/work (work) ==
tutorial:note {"text":"starting work"}
transit   {"to":{"status":"done"},"from":{"workflow":"demo","step":"work"}}
session_end {"status":"done"}
```

You're reading the file the agent wrote at
`.autosk/sessions/<id>.jsonl`, rendered. The first line is your agent's
`ctx.log.custom(...)`; the `transit` and `session_end` lines are **engine
structural entries** the daemon emits itself, so a transcript is self-contained.
A real pi/Claude agent fills the middle with streamed `assistant` messages and
tool calls — same format, same viewer.

> **Steer and abort.** A *live* session takes `autosk session input <id> "<msg>"`
> (steer the running agent) and `autosk session abort <id>` (cancel it and park
> the task to `human`). Our agent finishes in 3 s, so you'd need a longer-running
> one to try them — see [docs/daemon.md → Sessions](daemon.md#sessions--transcripts).

---

## Step 5: one daemon, many projects

A single `autoskd` serves **any number of projects**. It never needs telling
which — every request carries a `{cwd}` and the daemon walks up from there to the
nearest `.autosk/`. Spin up a second project against the **same** daemon:

```bash
mkdir -p /tmp/acme-web && cd /tmp/acme-web
autosk init
autosk create "Write the landing page"
autosk create "Wire up analytics"
autosk list
```

```
initialized /tmp/acme-web/.autosk
ID          STATUS  TITLE
ask-0252f7  new     Wire up analytics
ask-d8a84d  new     Write the landing page
```

Those are `acme-web`'s tasks — the cwd selected the project. Reach into the
*other* project without leaving this directory by overriding the selector with
**`AUTOSK_CWD`**:

```bash
AUTOSK_CWD=/tmp/acme-api autosk list
```

```
ID          STATUS  TITLE
ask-c82e17  done    Process the demo task
```

And the daemon knows about both:

```bash
autosk project list
```

```
NAME      ROOT
acme-api  /tmp/acme-api
acme-web  /tmp/acme-web
```

One process, two projects, one socket — confirm it never started a second daemon:

```bash
grep -c "listening on" /tmp/acme-daemon.log     # → 1
```

> **A note on machine time.** The human output above is your **local** time
> (`autosk show <id>` prints e.g. `created_at: 2026-06-24 19:35:11`). The
> machine-readable `--json` output is always **RFC3339 UTC**
> (`"created_at":"2026-06-24T17:35:11Z"`) — handy to know when you script against
> the CLI.

---

## Step 6: expose it over TCP for the desktop GUI

Everything so far went over the local Unix socket. To reach the daemon from
**another machine** — the [desktop or mobile GUI](gui-release.md) running on your
laptop while the daemon runs on a workstation — you use its **TCP listener**,
gated by a token.

> **The CLI is local-only.** `autosk` always talks to a daemon over the Unix
> socket; it has no `--host`/token flags. TCP is for the *GUI* front ends. So in
> this step you won't point the CLI at the port — you'll hand the address +
> token to the GUI.

Restart the daemon with an explicit TCP address (pick a free port; `0.0.0.0`
makes it reachable on your LAN):

```bash
pkill -f "autoskd serve --sock $AUTOSK_SOCK"
AUTOSK_NO_AUTO_INSTALL=1 autoskd serve --sock "$AUTOSK_SOCK" --tcp 0.0.0.0:7878 >/tmp/acme-daemon.log 2>&1 &
cat /tmp/acme-daemon.log
```

```
[info] autoskd: TCP listening on 0.0.0.0:7878 (token auth)
[info] autoskd 0.0.0-dev: listening on /tmp/acme-daemon.sock
```

The first TCP request on a connection must authenticate with a token the daemon
minted on first use. Read it:

```bash
cat ~/.autosk/daemon-token
```

```
6841356a861675370e497a09032cf15d339382fb339ba67c2ef7c33deab22d7d
```

Sanity-check that the port is accepting connections (auth happens inside the GUI):

```bash
nc -z -w2 127.0.0.1 7878 && echo "open"
```

```
Connection to 127.0.0.1 port 7878 [tcp/*] succeeded!
open
```

Now connect from the GUI: open **Settings → Remote**, enter the daemon's
`host:port` (`<workstation-ip>:7878`) and paste the token. The GUI's first call
on the wire is `meta.auth {token}`; once it succeeds you're driving the same
projects, tasks, and live sessions you created above — over the network. The full
desktop walkthrough (build/install, the remote connection UI, firewall notes) is
in [docs/gui-release.md](gui-release.md).

Two daemon behaviours change the moment TCP is live:

- **Idle-shutdown is off.** A UDS-only daemon shuts itself down after the idle
  window (`AUTOSK_IDLE_SECS`, default 30 min) with no work pending. A daemon with
  a TCP listener is treated as a long-lived service and never idle-exits.
- **It's reachable on your LAN.** `0.0.0.0` binds all interfaces. The token +
  port are the gate; bind `127.0.0.1:7878` instead if you only want loopback
  (e.g. an SSH tunnel).

---

## Clean up

```bash
pkill -f "autoskd serve --sock $AUTOSK_SOCK"
rm -rf /tmp/acme-api /tmp/acme-web /tmp/acme-daemon.sock /tmp/acme-daemon.log
unset AUTOSK_SOCK
```

---

## What you built

You ran the daemon the way the front ends do, and watched each moving part:

- **A daemon of your own.** `autoskd serve --sock <path>` in the foreground, with
  `AUTOSK_SOCK` wiring the CLI to it — the same socket the auto-spawn path uses.
- **The engine at work.** Enroll a task, the scheduler starts a **session**, the
  agent's `onRun` writes a **pi-format transcript**, and `ctx.transit` settles
  the task — all observable through `autosk session list` / `transcript`.
- **One daemon, many projects.** The `{cwd}` selector (and `AUTOSK_CWD`) picks the
  project per request; one process serves them all.
- **Remote access.** A TCP listener + a token file is all the GUI needs to drive
  the daemon from another machine — and turning it on keeps the daemon alive.

### Where to go next

- **The reference.** [docs/daemon.md](daemon.md) — every `serve` flag and env
  knob, the full [JSON-RPC proto-v2 surface](daemon.md#json-rpc-v2-surface),
  transports/auth, idle-shutdown, and the MCP tool servers.
- **A real agent.** Swap the toy `onRun` for a real harness: the
  [Claude Code workflow tutorial](extensions-tutorial-claude.md) wires in
  `@autosk/claude-agent` running in a per-task git worktree.
- **The extension model.** [The extension tutorial](extensions-tutorial.md) builds
  the `demo.ts` idea out properly — discovery, error isolation, recovery.
- **The desktop GUI.** [docs/gui-release.md](gui-release.md) — install the app and
  use the Remote mode you set up in Step 6 for real.
