![arranger](web/hero.png)

# arranger

**Arrange a team of AI coding agents and ship work that passes your checks.**

Arranger is a local web app for running a team of coding agents (Claude Code, Codex, Gemini CLI, pi, opencode, Cursor, Amp, or any command). You drag agents into a tree the way you'd staff a real team: managers split the work, workers do it, reviewers check it. You give the top agent one goal and a definition of done, a set of shell checks like `go test ./...`. Nothing counts as done until those checks pass.

It's a single Go binary with the UI built in and one SQLite file for state. There's no cloud account, and your code never leaves your machine.

## Why

One agent with one huge prompt drifts, forgets, and claims success. A team with small, checked goals doesn't:

- **Checks decide "done", not the agent.** Arranger runs your checks itself after every attempt and sends failures back with the exact output, up to three tries.
- **Parallel and isolated.** Every agent works in its own git worktree and branch, so agents on the same level run at the same time without touching each other's files. Your branches are never changed until you merge.
- **Review before anything merges.** Managers read each report's diff and accept it or send it back with feedback.
- **You stay in control.** Live logs and status for every agent, a diff per agent, remove or promote single changes, revert any checkpoint, token limits per agent, and one-click merge into the branch you pick.

## Install

macOS and Linux (amd64 and arm64):

```sh
curl -fsSL https://raw.githubusercontent.com/arranger-dev/arranger/main/install.sh | sh
```

This downloads the latest release, verifies its checksum, and puts `arranger` in `/usr/local/bin`. Set `INSTALL_DIR` to install somewhere else, or `ARRANGER_VERSION=v0.3.0` to pin a version.

From source (Go 1.26+):

```sh
git clone https://github.com/arranger-dev/arranger && cd arranger && go build -o arranger ./cmd/arranger
```

You also need `git`, and at least one agent CLI on your `PATH` (for example `claude`, `codex` or `gemini`), logged in the way you normally use it.

## Quick start

1. **Start it.**

   ```sh
   arranger
   ```

   Then open <http://127.0.0.1:7777> and click **Open arranger**.

2. **Point it at a repo.** Click **Settings** in the header and enter the path to a git repository. Leave the base branch blank to use the current branch. The repo needs at least one commit.

3. **Arrange the team.** Drag agent types from the left onto the canvas. Drop one agent onto another to put it under that agent. Click a box to pick its tool (runtime), model, color and instructions in the **Settings** tab. Click **Save**.

4. **Set the goal.** Select the top agent and fill in the **Goal** tab:
   - **Goal:** the outcome, e.g. "Add rate limiting to /login".
   - **Acceptance criteria:** what reviewers should hold the work to.
   - **Checks:** one shell command per line. All of them must exit 0, e.g. `go test ./...`.

5. **Run.** Press **Run**. A manager plans one subgoal (with its own checks) for each report, runs them in parallel, reviews and merges their work into its own branch, then runs its own checks. Links on the canvas animate while agents hand work down and report back. The **Logs** tab streams what each agent is doing.

6. **Review and merge.** Open the **Diff** tab to see every change, remove or promote single hunks, or revert a checkpoint. When you're happy, press **Merge…** to merge the agent's branch into `main` (or any branch, created if needed) as a merge commit, a squash, or a fast-forward.

**Stats** in the header shows cost, tokens, time and first-try success, per agent and per runtime.

## Options

```text
arranger [flags]
  -addr string      listen address (default "127.0.0.1:7777")
  -data string      data directory (default "~/.arranger")
  -parallel int     max agent processes running at once (default 4)
  -log string       log level: debug, info, warn or error (default "info")
  -log-json         log JSON lines instead of readable text
```

The server logs runs, agent processes (exit code, time, tokens, cost) and every API change to stderr. Use `-log debug` to also see each check and every request.

## How it works

- **State:** `~/.arranger/arranger.db` (SQLite, WAL) holds projects, agents, goals, runs, and the event log.
- **Workspaces:** each agent gets a worktree under `~/.arranger/worktrees/<agent>` on branch `arranger/<agent>`, branched from its manager's branch or the project base. Every run first merges in new commits from that branch, so agents always work on the latest code (the Diff tab also has a **Sync** button). Every attempt is committed as a checkpoint.
- **Agents:** each run starts the agent CLI headlessly (for example `claude -p --output-format stream-json`) and parses its streamed output into messages, tool calls, file edits and token usage.
- **Live UI:** the page gets updates over Server-Sent Events.

## Development

```sh
go test ./...
go run ./cmd/arranger -log debug
```

The code is organized as `cmd/arranger` (entry point), `internal/server` (HTTP and SSE), `internal/orch` (running goals, managers, checks), `internal/agents` (agent CLI drivers), `internal/git` (worktrees, diffs, merges), `internal/store` (SQLite), and `web` (the embedded UI).

Push a tag like `v0.3.0` to build release binaries for macOS and Linux and publish them on GitHub.

## License

The embedded fonts (IBM Plex Sans, IBM Plex Serif, Lilex) are licensed under the SIL Open Font License 1.1.
