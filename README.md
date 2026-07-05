# qkill

Quickly kill processes. Cross-platform. Interactive substring search.

## Install

```bash
go install github.com/ogpourya/qkill@latest
```

## Usage

```bash
# interactive TUI
qkill

# kill by pid
qkill 1337

# kill by name
qkill chrome

# kill by port
qkill :8080

# force kill (SIGKILL)
qkill -f 1337 chrome :3000
```

## Interactive TUI

Processes are displayed in bordered boxes sorted by RAM usage (descending). Each box shows PID, memory, process name, and the command line with shell syntax highlighting.

Long command lines are wrapped to fit the terminal width.

## Keys

| key | action |
|-----|--------|
| type | substring search (name, pid, cmdline) |
| `↑` `↓` | navigate |
| `enter` | kill (SIGTERM) — if process still alive after 3s, prompts for force kill |
| `ctrl+c` | quit |
| `esc` | clear query / quit (when query empty) |

## Kill flow

1. Press `enter` on a process → sends SIGTERM
2. If the process is already dead → exits immediately
3. Waits 3 seconds, then checks if the process still exists
4. If still alive → prompts `process X still running — force kill? (y/n)`
5. `y` / `enter` → sends SIGKILL and exits
6. `n` / `esc` → exits without killing

## License

MIT
