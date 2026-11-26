# runr

## Gorillas start  
- Installer Go [link](https://go.dev/dl/) (vi kan også bygge en exe her på github, men så er det en virus ifølge fujitsjus, med go installeret kan vi selv bygge det og så er det ikke en virus 🤓)
- Installer runr - terminal fra mappen her -  `go install ./cmd/runr`
- Sæt environment variable `RUNR_ROOT` = path til mappen du har dine git repo's i
- Kør powershell scriptet i examples/ mappen for at importere gorillas project setup
- Kør runr fra terminalen ved at skrive `runr`

[![CI](https://github.com/Skaanning/runr/actions/workflows/ci.yml/badge.svg)](https://github.com/Skaanning/runr/actions/workflows/ci.yml)

**Your whole dev stack in one terminal.** runr starts the programs you work on (a
database, a few services, a frontend) in the right order, and shows all their output in one
view.

Say your app is Postgres in Docker, an API, a background worker and a web frontend, spread
over three repositories. Without runr that's four terminals and remembering what to start
first. With runr, once you've registered them and put them in two groups
([Quick start](#quick-start)), one command does it:

```console
$ runr up backend frontend
```

runr starts Postgres and waits until it's listening, then the API and the worker, then the
frontend once the API is up, and shows them side by side:

![runr's running view: four processes with their ports and git branches, their combined logs, and a toast for a crash](docs/images/running.png)

Here the worker has just crashed, and a toast says so: select it and press `tab` for its
output, `r` to restart it. `/` searches all the logs, `b` switches a project's git branch.
Press `d` to get your terminal back while everything keeps running, and `runr attach` to
return. `q` stops everything, in reverse order.

From another terminal, or from your coding agent:

```console
$ runr restart api --wait     # returns once api is ready again, or shows why it crashed
$ runr logs worker --errors   # just the error lines and crashes
```

It isn't only for servers. A migration, seeding a database, `npm install` or a test run is a
project too: mark it as a one-shot task (`--ready exit`), and whatever depends on it waits
until it has finished. End-to-end tests that need the database migrated and the API up, for
example:

```bash
runr add migrate --dir ~/code/api --depends-on db --ready exit -- go run ./cmd/migrate up
runr add e2e --dir ~/code/web --depends-on api,migrate -- npm run test:e2e
runr up e2e    # db, then the migration and api, then the tests; r runs them again
```

Works on Linux, macOS and Windows.

## Features

- **Dependencies and readiness:** projects start after what they depend on is ready — when
  a log line matches, a port or health URL answers, or a one-shot task (a migration,
  `npm install`) has finished.
- **Launcher:** pick groups and projects to start, and add or edit them in forms.
- **Running view:** every process with its state, uptime, ports and git branch; combined or
  per-process logs with search; restart, stop and start processes as you go.
- **Interactive programs:** run a project on a terminal (`pty = true`) and attach to it to
  answer its prompts, e.g. a script that asks for a branch name.
- **Shared configs:** write project directories as `$RUNR_ROOT/api`, so a team can share one
  config while everyone keeps their checkouts in a different place.
- **Detach and come back:** `d` leaves a session running in the background (or start it
  that way with `runr up -d`); `runr attach` or the launcher brings its running view back.
  `D` and `Q` detach or stop and go to the launcher instead of quitting. Closing the
  terminal without `d` stops the session.
- **From another terminal or an AI agent:** `runr status`, `runr logs api --errors`,
  `runr logs api --until <regexp>` and `runr restart api --wait` show, wait for and change
  what a running session is doing, so scripts and coding agents can restart a service, wait
  for a log line and read a crash without copy-paste (see
  [Using runr from an AI coding agent](docs/guide.md#using-runr-from-an-ai-coding-agent)).
- **Clean shutdowns:** everything stops in reverse dependency order, and a project's
  `stop_cmd` cleans up what it leaves behind when it has to be killed, e.g. Docker containers.
- **Git:** see each project's branch and changes, switch branches, fetch and pull all.
- **Import** projects from a `Procfile` or a Docker Compose file.
- **Logs** saved to files on demand or for every session, plain output when piped, and color
  themes.

## Screenshots

The launcher, where you pick what to start and manage projects and groups. Sessions
running in the background are listed on top, to open again or stop:

![The launcher, with a detached session under Running](docs/images/launcher.png)

Adding a project: the form checks the directory and env file as you type and previews the
project's row:

![The new-project form](docs/images/new-project.png)

Switching a project's git branch from the running view:

![The branch picker](docs/images/branch-picker.png)

Searching the logs:

![Search](docs/images/search.png)

Typing into a script that asks for input (`i` attaches, `ctrl+q` gives the keys back):

![Answering a deploy script's prompts](docs/images/typing.png)

From another terminal, or an AI coding agent: what runs, and a restart that waits and shows
why it failed:

![runr status and runr restart --wait in a shell](docs/images/cli.png)

## How it compares

Tools for running a local dev stack mostly fall into three groups, and runr takes something
from each:

- **Procfile runners** start everything listed in one file and interleave the output. runr
  also starts things in dependency order, waits until each one is ready, and lets you
  restart, stop or search one process while the others keep running. `runr import` reads a
  `Procfile`.
- **Terminal process managers** give each process a pane of its own, usually set up per
  directory. runr keeps one list of projects across all your repositories and starts any
  combination of them (`runr up api web`), with one log of everything, git state, and
  sessions you can detach from and come back to.
- **Container tools** run images. runr runs your programs the way you would in a terminal
  (`go run`, `npm run dev`), and can run `docker compose up` as one of its projects, e.g.
  for the database.

On top of that, `runr status`, `runr logs` and `runr restart --wait` work on the session you
are watching, so scripts and coding agents can use it too.

## Install

Download a binary from the [releases](https://github.com/Skaanning/runr/releases), or
build it with Go 1.27 or newer:

```bash
go install github.com/Skaanning/runr/cmd/runr@latest
```

This puts `runr` in `$(go env GOPATH)/bin`, usually `~/go/bin`; make sure that directory is
on your `PATH`. See [Building and releasing](docs/guide.md#building-and-releasing) for the
release binaries and building from a clone.

## Quick start

Register your projects. `--dir` defaults to the current directory; everything after `--`
is the command.

```bash
runr add db --dir ~/code/infra --ready-log 'listening on IPv4' -- docker compose up postgres
runr add api --dir ~/code/api --depends-on db --env PORT=8080 -- go run ./cmd/server
runr add worker --dir ~/code/api --depends-on db -- go run ./cmd/worker
cd ~/code/web && runr add web --depends-on api -- npm run dev
```

Group them, if you like:

```bash
runr group create backend db api worker
runr group create frontend web
```

Then start what you need:

```bash
runr up backend frontend   # everything: db first, api and worker once db is listening
runr up web                # just web, plus what it needs: api and db
runr                       # or pick from a list in the launcher
```

Press `q` to stop everything, or `d` to leave it running in the background and come back
with `runr attach`. See [examples/config.toml](examples/config.toml) for a
complete config file.

Already have a `Procfile` or `compose.yaml`? `runr import` registers all of it at once (see
[Importing](docs/guide.md#importing-a-procfile-or-compose-file)).

## Documentation

The [guide](docs/guide.md) covers everything else:

- [Commands](docs/guide.md#commands) and their flags
- [Configuration](docs/guide.md#configuration): settings, themes, project fields,
  [sharing a config](docs/guide.md#sharing-a-config)
- [The launcher](docs/guide.md#the-launcher) and [the running view](docs/guide.md#the-running-view), with all their keys,
  and [typing into a program](docs/guide.md#typing-into-a-program)
- [Saving logs](docs/guide.md#saving-logs), [detaching](docs/guide.md#detaching),
  [talking to a running session](docs/guide.md#talking-to-a-running-session),
  [using runr from an AI coding agent](docs/guide.md#using-runr-from-an-ai-coding-agent)
  and [Git](docs/guide.md#git)
- [How processes are run](docs/guide.md#how-processes-are-run),
  [cleaning up after a kill](docs/guide.md#cleaning-up-after-a-kill) and
  [Troubleshooting](docs/guide.md#troubleshooting)

## License

MIT, see [LICENSE](LICENSE).
