# runr guide

Everything about using runr. For a short introduction, see the [README](../README.md).

- [Commands](#commands)
- [Importing a Procfile or compose file](#importing-a-procfile-or-compose-file)
- [Configuration](#configuration): [settings](#settings), [themes](#themes),
  [project fields](#project-fields), [validation](#validation)
- [The launcher](#the-launcher)
- [The running view](#the-running-view): [errors](#errors), [typing into a program](#typing-into-a-program)
- [Saving logs](#saving-logs)
- [Detaching](#detaching): [attaching again](#attaching-to-a-session)
- [Talking to a running session](#talking-to-a-running-session)
- [Using runr from an AI coding agent](#using-runr-from-an-ai-coding-agent)
- [Git](#git)
- [How processes are run](#how-processes-are-run): [cleaning up after a kill](#cleaning-up-after-a-kill)
- [Troubleshooting](#troubleshooting)
- [Building and releasing](#building-and-releasing)

## Commands

| Command | Description |
|---|---|
| `runr` | Open the launcher |
| `runr up <name...> [--log-dir <dir>]` | Start groups and/or projects (plus their dependencies) and open the running view; `--log-dir` also saves each project's output to `<dir>/<project>.log` (see [Saving logs](#saving-logs)) |
| `runr up -d <name...>` | Start them in the background without opening the running view; `runr attach` shows it (see [Detaching](#detaching)). Works without a terminal |
| `runr up --last` | Start what the last session started (including what you started later from the running view), with the same extra arguments. Projects or groups since removed are skipped with a warning |
| `runr add <name> [flags] -- <cmd>` | Register a project |
| `runr edit <name> [flags] [-- <cmd>]` | Change a project; only the given flags change |
| `runr rm <name> [--force]` | Remove a project (and from its groups); refuses if others depend on it unless `--force` |
| `runr rename <name> <new-name>` | Rename a project or group; groups and `depends_on` are updated |
| `runr ls` | List projects and groups, with warnings such as missing directories |
| `runr import [file\|dir] [flags]` | Register projects from a Procfile or Docker Compose file (see [Importing](#importing-a-procfile-or-compose-file)) |
| `runr group create <group> <project...>` | Create a group |
| `runr group add <group> <project...>` | Add projects to a group |
| `runr group rm <group> <project...>` | Remove projects from a group |
| `runr group delete <group>` | Delete a group (its projects are kept) |
| `runr group ls` | List groups |
| `runr theme [name]` | List the color themes (the current one marked with `*`), or set one |
| `runr status [--json]` | Show the runr sessions running in the background or in other terminals, and their processes (see [Talking to a running session](#talking-to-a-running-session)) |
| `runr logs [name...] [-n N] [-f] [--errors] [--until RE]` | Print a running session's recent output, keep following it, or wait for a line |
| `runr start\|stop\|restart <name...> [--wait]` | Start, stop or restart projects and groups in a running session; `--wait` reports how it went |
| `runr down [--force]` | Stop a running session, as `q` does there; `--force` kills its processes |
| `runr attach [--session <pid>]` | Open the running view of a session running in the background or in another terminal (see [Attaching to a session](#attaching-to-a-session)) |
| `runr cleanup [--yes]` | Stop processes left running by a runr that was killed (Linux, macOS), then run their projects' `stop_cmd`; asks first unless `--yes` |
| `runr completion <shell>` | Shell completion script (bash, zsh, fish, powershell), completes project and group names |

Flags for `add` and `edit`:

| Flag | Field |
|---|---|
| `--dir <path>` | `dir` (`add`: defaults to the current directory) |
| `--env KEY=VALUE` | `env`, repeatable; `edit` sets/overrides single variables |
| `--unset-env KEY` | `edit` only: remove a variable, repeatable |
| `--env-file <path>` | `env_file` |
| `--depends-on <name>` | `depends_on`, repeatable or comma-separated; `edit` replaces the list, `--depends-on=` clears it |
| `--ready exit` | `ready` (`--ready=` clears it) |
| `--ready-log <regex>` | `ready_log` |
| `--ready-port <port>` | `ready_port` (`--ready-port=` clears it) |
| `--ready-url <url>` | `ready_url` |
| `--color <color>` | `color` |
| `--display-name <label>` | `display_name` |
| `--pty` | `pty` (`--pty=false` turns it off) |
| `--stop-cmd <cmd>` | `stop_cmd` (`--stop-cmd=` clears it) |

With `edit`, a flag given an empty value (e.g. `--color=`) clears that field.

If `runr up` isn't connected to a terminal (piped, or run from a script), it prints the
interleaved output with a name prefix instead of opening the UI, and exits once all
processes have finished — with exit code 1 if any crashed. Ctrl+C stops everything. Unless
that output is a terminal (and `NO_COLOR` isn't set), it has no colors: the programs'
colors and other escape codes are removed too. Other terminals can still look at and control
it (see [Talking to a running session](#talking-to-a-running-session)).

## Importing a Procfile or compose file

If a repository already has a `Procfile` or a `compose.yaml`, register all of it at once:

```bash
cd ~/code/shop && runr import     # finds Procfile, compose.yaml, compose.yml,
                                  # docker-compose.yaml or docker-compose.yml
runr up shop                      # the group named after the compose name or the directory
```

- **Procfile**: one project per `name: command` line, run in the file's directory. A `.env`
  next to it becomes the projects' `env_file`, as foreman and overmind would load it.
- **Compose**: one project per service, run as
  `docker compose [-f <file>] up --no-deps --no-log-prefix <service>`. `--no-deps` leaves
  starting the dependencies to runr, from `depends_on`. A service with a published TCP port
  gets that host port as `ready_port`. A service that another one waits for with
  `condition: service_completed_successfully` (a migration, say) becomes a one-shot task
  (`ready = "exit"`) instead; other conditions are ignored. Each service gets
  `stop_cmd = "docker compose [-f <file>] stop <service>"`, so its container is stopped even
  when runr has to kill `docker compose up` (see
  [Cleaning up after a kill](#cleaning-up-after-a-kill)). Images, builds, environment and
  profiles stay in the compose file.
- `runr import <file>` imports a file of any name (`--format procfile|compose` if the name
  doesn't tell), `runr import <dir>` searches that directory.
- `--group <name>` names the group, `--no-group` skips it; `--prefix <p>` puts `<p>` in front
  of every project name. If a name is already taken, nothing is imported and the clashes
  are listed. `--dry-run` prints the TOML that would be added.
- It's a one-time copy: change the projects with `runr edit` afterwards, and re-import (e.g.
  after `runr rm`) if the file changes a lot.

## Configuration

Everything lives in one file:

| OS | Path |
|---|---|
| Linux | `~/.config/runr/config.toml` (or `$XDG_CONFIG_HOME/runr/config.toml`) |
| macOS | `~/Library/Application Support/runr/config.toml` |
| Windows | `%AppData%\runr\config.toml` |

Set `RUNR_CONFIG=/path/to/config.toml` to use a different file. The file is created on the
first change and is readable only by you, since `env` values may hold secrets. To share a
config with your team when everyone keeps their repositories somewhere else, see
[Sharing a config](#sharing-a-config).

You can edit it by hand, but `runr add`/`edit`, `runr group …` and the launcher's forms all
validate before saving and are usually easier. runr rewrites the whole file when it saves,
so **comments in a hand-edited file are lost** after the next change made through runr.

```toml
[settings]
shutdown_timeout = "5s"
scrollback_lines = 10000
show_all = true
theme = "tokyo-night"

[projects.api]
dir = "~/code/api"
cmd = "go run ./cmd/server"
env = { PORT = "8080" }
env_file = ".env"
depends_on = ["db", "migrate"]
ready_log = 'listening on :\d+'
color = "cyan"
display_name = "API"

[projects.migrate]
dir = "~/code/api"
cmd = "go run ./cmd/migrate"
ready = "exit"   # a one-shot task: api waits until it has finished

[groups]
backend = ["db", "api"]
```

### Settings

| Key | Default | Description |
|---|---|---|
| `shutdown_timeout` | `"5s"` | How long a stopping process gets before it is killed (Go duration: `500ms`, `10s`, `1m`). Docker Compose projects may need more, e.g. `"15s"`: stopping a Postgres container takes about 10 s |
| `scrollback_lines` | `10000` | Lines of output kept per process |
| `theme` | `"terminal"` | Colors of the TUI: `terminal` (your terminal's own colors), `catppuccin`, `tokyo-night`, `gruvbox` or `nord`. See [Themes](#themes). |
| `theme_background` | `false` | Paint the theme's background and text color behind the whole TUI instead of letting your terminal's show through. See [Themes](#themes). |
| `show_all` | `true` | List the registered projects that aren't running in the running view's sidebar, below the session's (`a` toggles it for a session) |
| `log_dir` | — | Directory where every session appends each project's output to `<project>.log`, e.g. `"~/.local/state/runr/logs"`; `runr up --log-dir` overrides it (see [Saving logs](#saving-logs)) |
| `error_pattern` | `'(?i)\b(error\|panic\|fatal\|exception\|traceback)\b'` | The [regular expression](https://pkg.go.dev/regexp/syntax) that marks a line of output as an error (see [Errors](#errors)), matched without the line's colors |

### Themes

`terminal` uses your terminal's 16 colors, so runr matches whatever scheme the terminal has.
The others bring their own colors: `catppuccin` (mocha on dark terminals, latte on light
ones), `tokyo-night` (night / day), `gruvbox` (dark / light) and `nord` (dark only; on a
light terminal runr uses the `terminal` theme instead). runr asks the terminal whether its
background is light or dark when it starts, and assumes dark if it doesn't answer. Themes
color the text only and your terminal's background shows through, unless you set
`theme_background = true`: then runr sets the terminal's background and text color to the
theme's while it runs (so output that resets its colors falls back to the theme's too) and
restores them when it exits. That needs a terminal that answers the background query and lets
programs set its colors, which most do. Named project colors
(`cyan`, `bright-red`, …) come from the theme; numbers and hex codes are used as they are.
Themes need a 256-color or true-color terminal; with 16 colors runr uses the `terminal`
theme.

To try them, press `t` in the launcher: it shows each theme as you move through the list and
saves the one you pick. `runr theme` lists them and `runr theme <name>` sets one.

### Project fields

| Field | Required | Description |
|---|---|---|
| `dir` | yes | Working directory. `~` is expanded, and a leading `$RUNR_ROOT` is replaced by that variable (see [Sharing a config](#sharing-a-config)). |
| `cmd` | yes | Command line, run with `sh -c` (Unix) or `cmd /c` (Windows), so pipes, `&&` and variables work |
| `env` | no | Environment variables, e.g. `{ PORT = "8080" }` |
| `env_file` | no | A `.env` file, relative to `dir` (absolute and `~` paths work too) |
| `depends_on` | no | Projects that must be ready before this one starts |
| `ready` | no | `"exit"` makes the project a one-shot task (a migration, a build step, `npm install`): it is ready once it exits with code 0, so its dependents wait until it has finished, and a non-zero exit blocks them. |
| `ready_log` | no | Regular expression ([Go syntax](https://pkg.go.dev/regexp/syntax)); the project is ready once an output line matches. Colors are stripped before matching. |
| `ready_port` | no | TCP port; the project is ready once a connection to it on `localhost` succeeds, e.g. `5432` for a database. |
| `ready_url` | no | `http://` or `https://` URL; the project is ready once a GET answers with a 2xx or 3xx status (redirects aren't followed), e.g. `"http://localhost:8080/health"`. The probe's requests show up in the server's access log. |
| `color` | no | Color of the name in the TUI: `black`, `red`, `green`, `yellow`, `blue`, `magenta`, `cyan`, `white`, their `bright-` variants (`bright-red`, …), an ANSI number `0`–`255`, or a hex code `#f80` / `#ff8800`. Projects without one get a color from a fixed palette, in name order, so a project has the same color in the launcher and every session. |
| `display_name` | no | Label shown in the TUI instead of the project name |
| `pty` | no | `true` runs the project on a pseudo-terminal, so it behaves as in a terminal: for programs that drop their colors or buffer their output otherwise. See [How processes are run](#how-processes-are-run). |
| `stop_cmd` | no | A command that cleans up after the project when it didn't stop cleanly, e.g. `"docker compose down"`: run like `cmd` (same `dir` and environment) only after runr had to kill the project, or for a project left over by a runr that was killed. See [Cleaning up after a kill](#cleaning-up-after-a-kill). |

A project has at most one of `ready`, `ready_log`, `ready_port` and `ready_url`. Without
any, it is ready as soon as it has started. Ports and URLs are tried every 250 ms (each try
times out after 1 s) for as long as the process runs; there is no overall time limit.

Names of projects and groups may contain letters, digits, `.`, `_` and `-` (starting with a
letter or digit), and share one namespace so `runr up <name>` is never ambiguous.

### Sharing a config

Set `RUNR_ROOT` to the directory where you keep your repositories, e.g. in your shell profile:

```sh
export RUNR_ROOT=~/work        # a teammate might use ~/src, or D:\src on Windows
```

A `dir` that starts with `$RUNR_ROOT` is then resolved under it, so the same config works for
everyone, wherever their checkouts are:

```toml
[projects.api]
dir = "$RUNR_ROOT/api"
cmd = "go run ./cmd/server"
```

While `RUNR_ROOT` is set, `runr add`, `runr edit --dir`, `runr import` and the launcher's form
write directories under it this way by themselves; directories elsewhere keep their `~` or
absolute form. To switch a project you added earlier, run `runr edit <name> --dir <its dir>`
(or open it in the launcher's form and save). The value of `RUNR_ROOT` may start with `~` and
must otherwise be an absolute path. Only the exact form `$RUNR_ROOT` is understood, not
`${RUNR_ROOT}` or other variables.

The launcher and the branch picker show the resolved directory (`~/work/api`); `runr ls`
shows the dir as written in the config. In the project form, `$RUNR_ROOT` is drawn in the
accent color (yellow while the variable isn't set), and the line under the field shows where
it points: `✓ exists · ~/work/api`.

If `RUNR_ROOT` isn't set, projects that use it get a warning (`dir: $RUNR_ROOT is not set`)
and `runr up` refuses to start them; the other projects work as usual.

To keep the shared file itself in one place, point `RUNR_CONFIG` at it. Remember that `env`
values are in the file too, so leave secrets to an `env_file` that each person keeps.

### Validation

runr checks the config when it loads it and before every save:

- group members and `depends_on` entries must exist, and dependencies can't form a cycle
  (reported as e.g. `api → auth → api`)
- at most one readiness condition per project; `ready` must be `"exit"`, `ready_log` a
  valid regex, `ready_port` a port 1–65535, `ready_url` an `http(s)://` URL with a host;
  `color` a known color
- `dir` and `env_file` should exist — only a warning in `runr ls` and the forms, but
  `runr up` refuses to start a project whose directory is missing

## The launcher

`runr` without arguments lists your groups and projects. Select some and press `enter` to
start them in the running view, or manage the registry with in-TUI forms. Sessions already
running (in the background or in other terminals) are listed above the groups, under
**Running**, to go back to. Groups are shown as
cards with a colored dot per member, side by side as far as the terminal is wide; projects
are listed below them with their color, directory, branch, command and dependencies.

![The launcher, with a detached session under Running](images/launcher.png)

The bottom line is the status bar: what the launcher is doing (`SELECT`, `NEW PROJECT`,
`SWITCH`, …), how much is registered and selected, and the project under the cursor with its
branch. On terminals shorter than 20 rows the `runr` mark makes room for the list.

| Key | Action |
|---|---|
| `↑` `↓`, `home` `end` | Move: through the running sessions, the group cards row by row, then down the project list |
| `←` `→` | Move between the group cards in a row |
| `space` | Select / unselect; on a group, selects all its projects (or unselects them if all are selected) |
| `enter` | Start the selection, or the item under the cursor if nothing is selected. On a running session: open its running view (`D` or `Q` there comes back to the launcher) |
| `x` | On a running session: stop it, after confirming, as `runr down` does |
| `l` | Start the last session again, like `runr up --last`. Under the `runr` mark the launcher shows what that is: `l  last session: backend, web` |
| `R` | Start with extra arguments: like `enter`, but first a form with a field per project that will start, dependencies included. What you type is added to the end of the project's command, e.g. `--port 3001` makes `go run .` run as `go run . --port 3001`. The fields start with what you typed last, also in an earlier session. `shift+enter` does the same in terminals that report it (kitty, ghostty, WezTerm, foot, recent Windows Terminal) |
| `/` | Jump: every group and project shows a letter (`a`–`z`, then `A`–`Z`). A group's letter starts that group; a project's letter selects or unselects it and stays in jump mode, so you can pick several and press `enter` to start them. Running sessions show `1`–`9`, which open them. `esc` or `/` leaves |
| `n` | New project |
| `e` | Edit the project (or group) under the cursor; changing the name renames it |
| `g` | On a group: edit it. On a project: create a group, preselecting the selected projects |
| `d` | Delete, after confirmation that lists the affected groups and dependents |
| `b` | Switch the git branch of the project under the cursor (see [Git](#git)) |
| `p` | Pull the git repository of the project under the cursor (`git pull --ff-only`), after confirming |
| `F` | Fetch the git repositories of all registered projects |
| `P` | Pull the git repositories of the selection, or of the group or project under the cursor, after confirming |
| `c` | Copy the command line that starts the project under the cursor the way runr does: `cd` to its directory, runr's default variables, its `env_file` and `env`, then the command, e.g. `cd ~/code/api && env FORCE_COLOR=1 … PORT=8080 /bin/sh -c 'go run .'` (`cmd.exe` syntax on Windows). It goes to the clipboard through the terminal (OSC 52; in tmux, `set-clipboard on`) and is shown on the bottom line |
| `t` | Pick a color theme; the launcher previews the theme under the cursor, `enter` saves it, `esc` keeps the old one |
| `q`, `esc` | Quit |

![The new-project form, filled in](images/new-project.png)

In forms: `tab`/`enter` next field, `shift+tab` back, `←→` and `space` pick
dependencies or group members, `↑↓` on the color cycles through the named colors, `enter` in
the environment starts a new line. `ctrl+s` saves from any field, `esc` cancels. Errors show
under their field once you've edited or left it, and are counted in the status bar; the form
can't be saved until they are fixed. The directory and env file are checked as you type
(`✓ exists · git: main`), and a preview shows the project's row as the running view will.

In the directory field, the first folder matching what you've typed shows faint after the
cursor; `→` at the end of the line accepts it (hidden folders show once you type a `.`).
`ctrl+o` opens a folder browser at the directory typed (or the nearest folder above it that
exists): type to filter, `enter` opens a folder, `backspace` goes up, `ctrl+o` uses the folder
shown and `esc` goes back to the form. Git repositories are marked `git`. Quotes around a
directory or env file are dropped, so a path from Explorer's "Copy as path" can be pasted
as is.

## The running view

![The running view: four processes with their ports and git branches, combined logs, and a toast for a crash](images/running.png)

The `processes` pane gives each process two lines: its state, how long it's been running
(or `exit 1` in red once it crashed) and a sparkline of its output over the last 32 seconds,
then the ports it listens on and its git branch underneath (`:5173 +1 · main`: port 5173
and one more). On narrow terminals the sparkline goes first, then the uptime, and the
branch is shortened. The `logs` pane shows their output — either all of them interleaved (the
combined view) or just the selected one — with `following`, or `paused · 12 more below` once
you scroll up, in its border. The status bar below shows the mode (`FOLLOW`, `PAUSED`,
`SEARCH`, `STOPPING`, …), the session, how many processes are running, starting and failed,
the selected project's branch and the time. Terminals smaller than 50×10 get a compact
layout without the pane borders.

Below the session's processes, under `not running`, the `processes` pane lists the other
registered projects, dimmed; select one and press `s` or `enter` to start it (with its
dependencies). Turn that list off with
`show_all = false` in `[settings]`, or toggle it for a session with `a`. When something
crashes or is blocked, or a port it needs is taken, a toast in the corner of the output says
so for 5 seconds.

| Icon | State |
|---|---|
| `○` | pending |
| `⠋` (spinning) | waiting for dependencies, or started but not ready yet (its `ready_log` hasn't matched, its port or URL doesn't answer yet, or a one-shot task is still running); faint while stopping |
| `●` | running (ready) |
| `✓` | exited with code 0 (`done` for a one-shot task) |
| `✗` | crashed — the exit code is shown next to it |
| `■` | stopped by you |
| `⊘` | blocked: a dependency ended before it became ready |

| Key | Action |
|---|---|
| `↑` `↓` | Select a process |
| `tab` | Toggle between the combined view and the selected process's output |
| `r` | Restart the selected process |
| `R` | Restart the selected process with extra arguments, added to the end of its command; the input starts with its current ones, and an empty one removes them. They are kept for later restarts and shown in the title of the project's own log |
| `s` | Stop the selected process, or start it (with its dependencies) if it isn't running |
| `enter` | Start the selected project if it's one of those not running |
| `+` | Start a group, or another project when the sidebar doesn't list them all (type to filter, `enter` to start, `esc` to close) |
| `a` | Show or hide the projects that aren't running |
| `!` | Select the project of the newest toast |
| `i` | Attach to the selected process to type into it (`pty = true` projects; see [Typing into a program](#typing-into-a-program)); `ctrl+q` detaches |
| `o` | Open the selected process's port (`http://localhost:<port>`) in the browser; asks which one when it listens on several |
| `b` | Switch the selected project's git repository to another branch (see [Git](#git)) |
| `p` | Pull the selected project's git repository (`git pull --ff-only`), after confirming |
| `F` | Fetch the git repositories of every project in the session |
| `P` | Pull the git repositories of every project in the session, after confirming |
| `/` | Search the output; `enter` jumps to the newest match, `esc` cancels |
| `n` / `N` | Next newer / older match |
| `e` / `E` | Jump to the next newer / older error line (see [Errors](#errors)) |
| `esc` | Clear the search and the error position |
| `PgUp` / `PgDn` | Scroll a page |
| `g` / `G` (`home` / `end`) | Jump to the top / bottom |
| `f` | Toggle follow (auto-scroll); scrolling to the bottom resumes it |
| `w` | Save the output on screen (all of it, or the selected process's) to a file in the current directory (see [Saving logs](#saving-logs)) |
| `c` | Copy the command line that starts the selected project the way runr does (with its extra arguments, if it has any): `cd` to its directory, runr's default variables, its `env_file` and `env`, then the command, e.g. `cd ~/code/api && env FORCE_COLOR=1 … PORT=8080 /bin/sh -c 'go run .'` (`cmd.exe` syntax on Windows). It goes to the clipboard through the terminal (OSC 52; in tmux, `set-clipboard on`) and is shown on the bottom line |
| `q`, `Ctrl+C` | Stop everything and quit; a second `Ctrl+C` kills everything immediately |
| `Q` | Stop everything, then show the launcher |
| `d` | Detach: quit, and let the session run on in the background (see [Detaching](#detaching)) |
| `D` | Detach and show the launcher, with the session under **Running** |

Search is plain text (no regex) and smart-case: an all-lowercase query ignores case, any
uppercase letter makes it case-sensitive.

### Errors

runr marks the lines of output that look like errors: those containing `error`, `panic`,
`fatal`, `exception` or `traceback` as a word, in any case (`no errors` and `terror` don't
count), and its own line when a process crashes or fails to start. A red `▌` sits left of
each of them, and the process's second line in the sidebar says how many its current run
printed, e.g. `3 errors`; the count starts again at every restart.

`e` jumps to the next error line in the log you're looking at (all processes, or the
selected one), `E` to the previous one, wrapping around at either end. The line jumped to
gets a yellow `▌`, scrolling stops, and the bottom line shows where you are, e.g.
`error 2/7`; `esc` clears it and `G` scrolls back to the newest output.

To match something else, set `error_pattern` in `[settings]`, e.g.
`error_pattern = '\b(ERROR|FATAL)\b'` for upper-case log levels only. Of runr's own
lines, `crashed (…)` and `failed to start: …` always count, whatever the pattern; the others
(`starting: …`, `blocked: …`) never do.

![Searching the combined logs for GET](images/search.png)

runr adds its own lines to each process's log, such as `starting: <cmd>`,
`waiting for db`, `ready` and `crashed (exit code 1)`, so you can see what happened when.

### Typing into a program

A project with `pty = true` runs on a terminal, and you can type into it, e.g. to answer a
script that asks for a branch name. Select it and press `i`: runr shows its log and every key
goes to the program, as in a terminal: `enter`, `backspace`, arrows, `ctrl+c` (which
interrupts the program, not runr) and pastes. `ctrl+q` detaches and gives runr its keys back;
runr also detaches when the program exits.

The line the program is writing, such as a prompt and what you type after it, shows at the
bottom of the log while it is unfinished, in both views and also when you aren't attached,
so you can see that a program is waiting. It becomes a log line once the program ends it;
log files, search, error counts and `ready_log` only see finished lines. What you type
appears because the program's terminal echoes it; input a program reads without echo
(`Read-Host -AsSecureString`, passwords) doesn't appear in the log. Projects without
`pty = true` get no input.

![Answering a deploy script's prompts: the token was typed but isn't echoed](images/typing.png)

## Saving logs

- **`w`** in the running view writes the output on screen — the combined view, or the
  selected process's — to `runr-<project|combined>-<YYYYMMDD-HHMMSS>.log` in the directory
  you started runr from. It holds what runr still has in memory (`scrollback_lines` per
  process), including runr's own lines (`starting: …`, `crashed …`).
- **`--log-dir <dir>`** (or `log_dir` in `[settings]`) keeps everything: each project's
  output is appended to `<dir>/<project>.log` as it arrives, written to disk at least once a
  second, so a crash leaves its last lines behind. Each session starts with a
  `--- runr session 2026-10-25 13:27:50 ---` line. The directory is created if needed; files
  are never rotated or deleted by runr. If a file can't be written, the project's log says
  so and runr stops saving its output there. Works with the launcher and with plain output
  too.

Both are plain text without colors, one line per output line:

```
13:27:52.145 » starting: go run .
13:27:52.204 │ 2026/10/25 13:27:52 database connection ok
```

Saved combined logs put the project's name before `│` / `»`; `»` marks runr's own lines.

## Detaching

Every session started in a terminal (`runr up`, or from the launcher) can be left running:
press `d` in the running view, and runr quits while the projects run on in the background.
Come back with `runr attach`, or from the launcher's **Running** section. `D` detaches too,
but shows the launcher, with the session under **Running** and the cursor on it: `enter`
shows it again, `q` quits to the shell. In the same way, `q` stops the session and quits,
and `Q` stops it and shows the launcher. `D` and `Q` need the launcher, which needs your
config: if runr can't read it, `runr attach` still shows the session (it sends its own
config), but alone, without `D` and `Q`, and `d` just quits.

```
$ runr up backend
  …                                   # d
runr: detached from session 48211; runr attach to come back
$ runr attach
```

- **Closing the terminal without `d` stops the session**, as `q` would: what you started in
  a terminal ends with it unless you detached first. That also holds for a view opened with
  `runr attach`: closing the last view of a session without `d` stops it.
- **`runr up -d backend`** starts a session in the background right away, without a view:
  `started session 48211 · runr attach to watch it`. It runs until `runr down`, `q` in a
  view, or until a view opened later is closed without `d`. It doesn't need a terminal, so
  scripts and coding agents can use it.
- **`runr status`** shows how many views each session has (`attached: 0` for a detached one);
  `runr logs`, `start`, `stop`, `restart` and `down` work on background sessions like on any
  other.

The session runs in a background runr process of its own (`runr host`), which the running
view talks to over the session's socket. It keeps a log of what goes wrong in `hosts/` in
runr's state directory, shown by `runr up` if the session can't start. If that process is
killed, its projects are cleaned up like those of any killed runr (see
[Cleaning up after a kill](#cleaning-up-after-a-kill)). Plain output (`runr up backend >
file`) still runs in the runr you started.

### Attaching to a session

`runr attach` opens the running view of a session that runs elsewhere: a detached one, one
started with `runr up -d`, or one running in another terminal. It shows everything the
session still has in memory and follows it live, with the usual keys: restart, stop, start,
search, git, typing into `pty = true` projects.

- **`d` detaches** again (`D`: to the launcher); **`q` stops the session**, as in its own
  terminal (a session in another terminal quits there too).
- **Closing the view's terminal** stops a background session when it was its last view; a
  session in another terminal runs on.
- Several views can watch one session at the same time. With several sessions running,
  choose one with `--session <pid>`.

The view uses the session's own config, as it was when the session started.

## Talking to a running session

Other terminals can ask a running session what it is doing, and tell it what to do: a
second terminal, an editor task, a git hook, or a coding agent that should restart a service
and read a crash's output itself instead of having it pasted in. This works for every
session: `runr up` (also with `-d`), the launcher, and plain output
(`runr up backend > /dev/null &`).

```bash
runr status                   # every running session and its processes
runr status --json            # the same as JSON, for scripts
runr logs api                 # api's last 100 lines
runr logs backend -n 0        # everything runr still has of the group's projects
runr logs --errors            # only error lines (settings.error_pattern) and crashes
runr logs -f api web          # keep printing new lines until Ctrl+C or the session ends
runr logs -f api | grep -m1 listening   # ends as soon as grep has its line
runr logs api --until 'Job .* done'     # wait for a new line that matches, then exit
```

```
session 48211 · backend, web · started 10:42 · attached: 1
NAME  STATE                  UPTIME  PID    PORTS   ERRORS  RESTARTS
db    running                12m03s  48230  :55432  0       0
api   crashed (exit code 1)  -       -      -       3       1
web   running                12m01s  48236  :3000   0       0
```

`runr logs` prints lines in the [saved logs' format](#saving-logs), without colors; with
several projects each line starts with the project's display name. It reads what the
session still has in memory (`scrollback_lines` per project). `-f` follows until Ctrl+C, the
session ends or nothing reads its output any more (`| head`, `| grep -m1`).

`--until <regexp>` follows until a new line matches, prints it and exits 0: wait for a
hot reload, a background job or a request without polling. It exits 1 if no line has
matched within `--timeout` (default 1m, `0` waits for ever) or the session ends first. Only
lines that arrive after `runr logs` starts count, never the ones it printed first, so a line
from an earlier run can't end the wait. The expression is matched against a line's text,
without the time and label, and runr's own lines count too:
`--until '^(exited|crashed|stopped|failed to start)'` waits until a project's current run is
over.

```bash
runr restart api --wait       # restart api; wait until it is ready again
runr start worker             # add a project (and what it depends on) to the session
runr stop web                 # stop one, as s does
runr down                     # stop the whole session, as q does
```

`start`, `stop` and `restart` take projects and groups, like the keys in the running view:
`start` also adds projects the session didn't run yet, with their dependencies; `stop` and
`restart` act on a group's own projects only. They return at once. With `--wait` they wait
until each project is ready (`api running (pid 4130, :8080)`), or for `stop` has stopped,
and exit with an error if one crashes, is blocked or isn't done within `--timeout` (default
1m), printing its last 20 lines first:

```
--- last lines of api ---
10:58:12.031 » starting: go run ./cmd/server
10:58:12.770 │ 2026/11/18 10:58:12 ERROR: cannot connect to database
10:58:12.774 » crashed (exit code 1)
runr: api crashed (exit code 1)
```

`runr down` stops everything in reverse dependency order and returns once the session has
ended; `--force` kills instead, like a second `q`. A second plain `runr down` while it is
stopping doesn't turn into a kill.

When several sessions run, a command picks the one that has the projects you name; if
that's still more than one (or you name none, as with `runr down`), choose with
`--session <pid>`, the pid shown by `runr status`. `runr start` with projects no session has
yet uses the only session, or asks for `--session`.

Each session listens on a socket named after its pid in `control/` in runr's state directory
(`~/.local/state/runr` on Linux, `~/Library/Caches/runr` on macOS, `%LocalAppData%\runr` on
Windows, or `$RUNR_STATE_DIR`). Only your user can open it. The socket goes away when the
session ends; one left by a runr that was killed is removed by the next `runr status` or
`runr logs`. Socket paths are limited to about 100 characters, so a very long
`$RUNR_STATE_DIR` turns this off (`runr status` says so).

## Using runr from an AI coding agent

Coding agents (Claude Code, Codex, Cursor and the like) run commands in a terminal of their
own, so they can't see the runr you are watching. With the commands from
[Talking to a running session](#talking-to-a-running-session) they don't need to: they
read the logs of your session, restart what they changed and check that it came up, while
you keep watching it in the running view. No copy-pasting crash output.

The commands an agent needs:

| Command | What the agent gets |
|---|---|
| `runr status --json` | Every session and its processes: state, pid, ports, error count, how the last run ended |
| `runr logs api --errors -n 50` | api's recent error lines and crashes, in plain text |
| `runr logs api -n 200` | The context around them |
| `runr restart api --wait` | Restarts api after a change; exit code 0 once it is ready, 1 with its last 20 lines if it crashes, is blocked or isn't ready within `--timeout` (default 1m) |
| `runr start worker --wait` | Starts a project (and what it needs) in your session |
| `runr logs api --until 'listening on' --timeout 2m` | Waits for a new line that matches, e.g. after a hot reload or triggering a job; exit code 1 if none came in time |
| `runr up -d backend` | A session of its own in the background, when none runs; `runr down` stops it afterwards |

Things to tell an agent, or to keep in mind when it uses runr:

- **No plain `runr logs -f`**: it follows until the session ends, so the agent's command
  never returns. `-n` gives it the recent lines, and `--until` waits for a line and exits.
- **`runr down` stops the whole session.** An agent should only stop a session it started
  itself, and use `--session <pid>` when several run.
- **Exit codes mean something:** `runr status` exits 1 when no session runs, `restart --wait`
  and `start --wait` when the project didn't come up.
- **Read-only commands are safe to allow without asking:** `runr status` and `runr logs` only
  read. `start`, `stop`, `restart` and `down` change what runs.

To make an agent use runr, add something like this to the instructions it reads in your
repository (`AGENTS.md`, `CLAUDE.md`, …):

```markdown
## Running the app

The services run under runr (https://github.com/Skaanning/runr), usually in a session
the developer is watching. Don't start your own servers on the same ports.

- See what runs: `runr status` (`--json` for details). Exit code 1: nothing runs; then
  start it yourself in the background with `runr up -d backend` and stop it with
  `runr down` when you're done.
- Read logs: `runr logs <project> -n 100`; only errors and crashes: `--errors`.
  To wait for something to show up in the logs: `runr logs <project> --until '<regexp>'`
  (exit code 1 if it hasn't within `--timeout`, default 1m). Never plain `runr logs -f`:
  it doesn't return.
- After changing a service, restart it and check it came up:
  `runr restart <project> --wait`. On failure it prints the project's last lines.
- Never `runr down` or `runr stop` a session you didn't start.
```

Replace `backend` with your group and the project names with yours (`runr ls` lists them).

What the agent sees: the session's processes, a restart that worked, and one that failed,
with the lines that say why:

![runr status and runr restart --wait in a shell](images/cli.png)

## Git

When `git` is installed, the sidebar (on each process's second line) and the launcher show
the branch of the repository each project's `dir` is in, with markers:

```
│ ▌● api     4m02s ▁▂▃▅▃▂▁▂ │
│ ▌  feat/login * ↓2        │
│  ● web     3m58s ▁▁▂▁▁▂▁▁ │
│    main ↑1                │
│  ✗ worker exit 1 ▁▁▁▁▁▃▇▁ │
│    (a1b2c3d) !            │
```

| Marker | Meaning |
|---|---|
| `*` | uncommitted changes to tracked files (staged or unstaged) |
| `↑n` / `↓n` | commits ahead of / behind the upstream, as of the last fetch or pull |
| `!` | a merge, rebase, cherry-pick, revert or `git am` in progress, or unresolved conflicts |
| `(a1b2c3d)` | detached HEAD |
| `?` (faint) | the state can't be read, e.g. git refuses the directory ("dubious ownership") |

Projects outside a repository show nothing. The state is read every 5 seconds and after
each action; runr never fetches on its own — press `F` to update `↑n` / `↓n`.

![The branch picker for web](images/branch-picker.png)

`b` (switch branch) and `p` (pull) work in the launcher and the running view. They act on the
selected project's repository, and only in the simple cases — anything that needs a decision
is left to you:

- Both are refused up front while the repository has uncommitted changes to tracked files,
  unresolved conflicts, or an operation in progress. Untracked files don't count, but git
  still refuses to overwrite them.
- `b` lists local branches and remote branches without a local one (most recent first),
  each with how far it is ahead/behind its upstream (`↑2 ↓1`), its last commit's subject and
  date; picking a remote branch creates a local branch that tracks it. The bottom of the list
  shows the git command `enter` runs. Type to filter; the matching part is highlighted.
- `p` fast-forwards the current branch from its upstream and fails if the branch has
  diverged — runr never merges, rebases, stashes or resets. It is refused on a detached
  HEAD or a branch without an upstream.
- If other projects are in the same repository, the picker (`also switches: …`) or the
  confirmation says so,
  since they change too (in the launcher: any registered project; in the running view:
  projects in the session).
- In the running view, git's output goes to the selected project's log, and a short summary
  or error to the bottom line. In the launcher, git's output is shown when it fails. Running
  processes are not restarted; press `r` if your dev server doesn't reload by itself.

`F` and `P` do the same for every repository at once — in the running view those of the
session's projects; in the launcher `F` covers all registered projects and `P` the selection
(or the group or project under the cursor):

- `F` runs `git fetch` in each repository, four at a time, then reads the state again, so
  the `↑n` / `↓n` markers are current. The bottom line sums it up (`fetched 3 repos ·
  1 failed`); failures go to the projects' logs (in the launcher: shown in place of the
  list).
- `P` asks first, listing the repositories (`Pull 2 repos? infra (db) · app (api, worker)`),
  then pulls them one at a time like `p`. Repositories that `p` would refuse (uncommitted
  changes, no upstream, …) are skipped, not fatal. The bottom line sums it up (`pulled 2 ·
  up to date 1 · skipped 1`), with the details in each project's log (in the launcher: the
  skipped and failed ones in place of the list).
- git never prompts: it gets no input, credential prompts are off and `ssh` runs in
  batch mode, so a pull that needs a password or passphrase fails instead of hanging — use
  an SSH agent or a credential helper. A `GIT_SSH_COMMAND` of your own is used as is; a
  `core.sshCommand` too, with `-o BatchMode=yes` added when it runs `ssh`. Each git command
  times out after 60 seconds.

## How processes are run

- **Dependencies.** Starting a project starts everything in its `depends_on`, transitively.
  Independent projects start in parallel; a project starts once all its dependencies are
  ready. Without a readiness condition, a project is ready as soon as it has started — so
  give a migration or build step `ready = "exit"`: its dependents then wait until it exits
  with code 0. If a dependency exits or crashes *before* it was ready, its dependents are
  blocked (`⊘`); restarting the dependency unblocks them. A crash *after* it was ready leaves
  dependents running. Restarting a finished one-shot task runs it again without restarting
  the projects that depend on it.
- **Environment**, later entries win: runr's own environment → `FORCE_COLOR=1`,
  `CLICOLOR_FORCE=1`, `PYTHONUNBUFFERED=1` → `env_file` → `env`.
- **Output** is read from pipes, not a terminal. The variables above get most tools to keep
  colors and not buffer their output; tools that ignore them may print without colors or
  in bursts. Processes get no input (stdin is empty). Lines longer than 64 KiB are split.
- **`pty = true`** (or `--pty`, or *Terminal* in the project form) runs a project on a
  pseudo-terminal instead, for tools that only color or flush their output on a terminal
  (`dotnet run`, `docker compose`, `ls --color=auto`, …). It gets `TERM=xterm-256color`
  and a terminal 50 rows tall and as wide as the logs pane, resized with it; stdout and
  stderr arrive as one stream. The line it is writing shows as it changes, so a prompt
  (`branch: `) is visible before it ends; select the project and press `i` to attach and
  type into it (see [Typing into a program](#typing-into-a-program)). Stopping works as
  without it (on Windows runr types Ctrl+C into the project's console instead of sending
  CTRL_BREAK). On Windows the terminal is a pseudo console (ConPTY), which redraws a screen
  rather than passing output through: runr keeps the text and its colors and drops the rest
  of what it sends. A pseudo console that is resized redraws its whole screen into the
  output, so on Windows a running project keeps the width it started with; it gets the new
  width when it starts again.
- **Stopping** (`s`, `r`, `q`): each process and everything it started gets SIGTERM
  (`CTRL_BREAK` on Windows) and `shutdown_timeout` to exit before it is killed. On quit,
  projects stop in reverse dependency order: dependents first, then what they depend on.
- **Processes left behind.** If a command exits but something it started keeps running in
  the background, runr stops that too (on Unix it gets SIGTERM and `shutdown_timeout`
  first). Programs that fully detach themselves (daemons) are not affected.
- **Ports** shown in the `processes` pane are the TCP ports the project's processes listen
  on, including ones opened by programs it started (e.g. `npm run dev` → `node`). They are
  checked every 2 seconds. On macOS this needs `lsof` (installed by default). Press `o` to
  open one in the browser.
- **Port conflicts.** When a project starts, runr checks the ports it is known to use — its
  `ready_port`, the port of its `ready_url`, and ports it listened on earlier in the
  session — and if another program already listens there, says so in its log and in a toast
  (`port 3000 is in use by pid 4123 (python3)`). The project is started anyway.
- runr never restarts a process on its own.
- **If runr itself is killed** (`kill -9`, a crash), the programs it started keep running
  on Linux and macOS. runr keeps a small file
  per session listing them (in `~/.local/state/runr/sessions`, or under `$XDG_STATE_HOME`; on
  macOS `~/Library/Caches/runr/sessions`), so the next `runr` or `runr up` asks first:
  `Kill 2 processes left over from a previous runr session? api (pid 4123), db (pid 4120)`.
  `enter`/`y` stops them like a normal shutdown, `esc`/`n` leaves them running and doesn't
  ask again. `runr up` starts your projects only after you've answered; with plain output it
  just prints a warning. `runr cleanup` does the same from the command line. Process start
  times are checked, so a process that merely got the same pid is never touched. On
  Windows, programs end with runr anyway.

### Cleaning up after a kill

Some programs leave things running that runr can't see: `docker compose up` stops its
containers when it is asked to stop, but when it is killed, or loses its output because
runr was killed, the containers keep running (they belong to the Docker daemon). A
project's `stop_cmd` cleans up after it:

```toml
[projects.db]
dir = "~/code/infra"
cmd = "docker compose up"
stop_cmd = "docker compose down"
```

It runs in the project's `dir` with its environment, without a terminal, when:

- **runr had to kill the project:** it was still running after `shutdown_timeout`, you
  pressed `q` (or Ctrl+C) a second time, or ran `runr down --force`. The project stays
  `stopping` until its `stop_cmd` has finished, so quitting waits for it; its output and
  `stop_cmd done (exit code 0)` or `stop_cmd failed (…)` go to the project's log. A
  `stop_cmd` still running after a minute is killed, as is one running when you force
  quitting again.
- **The project was left over by a runr that was killed** (Linux, macOS): `runr cleanup`,
  or answering the question at start, stops its processes and then runs its `stop_cmd`,
  also when its process is gone already (`db  gone  stop_cmd: docker compose down`).

A project that stops on its own signal, exits or crashes doesn't run it. `runr import` of
a compose file sets `stop_cmd = "docker compose stop <service>"` for each service.

On Windows, runr's programs end with runr itself, so after runr was killed there is nothing
left to run a `stop_cmd` for (but containers may still be running).

## Troubleshooting

- **No colors, or output arrives late:** the program thinks it isn't writing to a terminal.
  Look for a flag or variable that forces colors / line buffering (e.g. `--color=always`)
  and add it to `cmd` or `env`.
- **`stopped (signal: terminated)` although the program shut down cleanly:** wrappers like
  `go run` report the signal rather than the program's own exit code. So does the shell on
  systems where `/bin/sh` is dash (Debian, Ubuntu): it doesn't replace itself with the
  command, and stops on SIGTERM before the program does. Harmless (the program still gets
  `shutdown_timeout`); start `cmd` with `exec` to see the program's own status.
- **`still running after 5s, killing` for a Docker Compose project:** stopping a container
  can take longer than the default `shutdown_timeout` (Postgres in Docker takes about 10 s).
  Raise it, e.g. `shutdown_timeout = "15s"`, and set a `stop_cmd` (see
  [Cleaning up after a kill](#cleaning-up-after-a-kill)) for when runr kills it anyway.
- **npm prints an update notice when stopping:** add
  `--env NPM_CONFIG_UPDATE_NOTIFIER=false`.
- **`theme_background = true` has no effect:** the terminal didn't answer runr's question
  about its background color (inside tmux this happens when tmux doesn't know the outer
  terminal's colors), or it ignores programs setting its colors. The theme is still used for
  the text. With 16 colors, and with the `terminal` theme, nothing is painted.
- **The terminal kept runr's background after runr was killed:** runr restores the colors when
  it exits, but not when it's killed with `SIGKILL`. Run `reset`, or
  `printf '\e]110\a\e]111\a'`.
- **A program waits for input:** without `pty = true` it gets none; with it, select the
  project and press `i` to type into it (see [Typing into a program](#typing-into-a-program)).
- **A project's spinner keeps spinning:** it is waiting for a dependency, or its `ready_log`
  hasn't matched, or its `ready_port`/`ready_url` doesn't answer (wrong port, or the server
  listens on another address than `localhost`). Select it and press `tab` to see its log;
  runr's lines say what it's waiting for.

## Building and releasing

```bash
git clone https://github.com/Skaanning/runr.git && cd runr
go install ./cmd/runr        # or: make build → bin/runr
make cross                   # binaries for Linux, macOS and Windows in dist/
```

Release binaries are named `runr-<os>-<arch>`, with their SHA-256 sums in `checksums.txt`.
Rename yours to `runr` (`runr.exe` on Windows), make it executable (`chmod +x runr`) and put
it on your `PATH`. On macOS the binaries aren't signed: remove the quarantine flag with
`xattr -d com.apple.quarantine runr`.

Maintainers release by pushing a tag like `v0.1.0`: `.github/workflows/release.yml` builds the binaries
with `make cross` and publishes them as a GitHub release.
