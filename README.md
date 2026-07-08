# focusd

> You code. It keeps score.
>
> A local-first focus and habit engine that lives in your terminal: git hook,
> tmux status bar, Neovim plugin, one small Go binary. No cloud. No account.
> Nothing ever leaves your machine.

Prefere português? Leia o [manifesto](docs/manifesto.md).

## Why

Habit trackers ask you to check in. Checking in is friction, and friction
kills habits. focusd inverts the deal: the work you already do — commits,
edits, focus sessions — feeds the tracker automatically, and the score shows
up where your eyes already are: the tmux status bar and your editor's
statusline.

## Install

```sh
go install github.com/SiiahK/focusd@latest
focusd init --hook   # inside any repo: every commit updates your habit & streak
focusd               # start the daemon (clients also autostart it on demand)
```

Every `git commit` now answers back:

```
🚀 focusd · commit registrado · streak: 15 dias 🔥
```

The hook never blocks or fails a commit: hard 250ms budget, silent when the
daemon is unreachable.

## tmux

Live focus timer in the status bar (prints nothing when idle — a clean bar):

```tmux
# ~/.tmux.conf
set -g status-interval 1
set -g status-right '#(/path/to/focusd/focus_status.sh)'
```

Optional keybind to log a 25-minute focus block on habit 1:

```tmux
bind-key F run-shell "/path/to/focusd/tmux_focus.sh 1 25"
```

## Neovim

[focusd.nvim](focusd.nvim/) ships in this repo: passive coding telemetry
(coalesced heartbeats over the unix socket, zero keystroke overhead) plus a
pure-memory statusline component.

```lua
-- lazy.nvim
{ dir = "/path/to/focusd/focusd.nvim", event = "VeryLazy", opts = {} }
```

## Web dashboard (optional)

The daemon speaks HTTP over a unix socket by default. Want the HTMX
dashboard in a browser? Opt in to TCP:

```sh
FOCUSD_ADDR=127.0.0.1:8080 focusd
```

## Guarantees

- **One static binary.** CGO-free SQLite ([modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite)),
  WAL mode, ~27µs writes, cross-compiles anywhere Go does.
- **250ms, always.** Every client — hook, tmux segment, Neovim plugin — obeys
  a hard 250ms timeout. Your shell, bar and editor never wait on the daemon.
- **Unix socket IPC** (mode 0600). TCP only if you ask for it.
- **Suspend-aware timing.** Closing the laptop lid overnight does not become
  a 9-hour "focus session"; slept time is detected and deducted.
- **Singleton by flock.** Clients autostart the daemon safely; concurrent
  spawns exit silently.
- **Your data is a file.** One SQLite database you own. Query it, back it
  up, delete it — it's yours.

## Configuration

| Variable      | Default                              | Meaning                       |
| ------------- | ------------------------------------ | ----------------------------- |
| `FOCUSD_DB`   | `focus.db` in the daemon's cwd       | SQLite database path          |
| `FOCUSD_SOCK` | `$XDG_RUNTIME_DIR/focusd/focusd.sock` (or OS cache dir) | unix socket path |
| `FOCUSD_ADDR` | *(empty — disabled)*                 | optional TCP for the web UI   |

## License

[MIT](LICENSE)
