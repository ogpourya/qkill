# qkill

Quickly kill processes. Interactive substring search.

## Install

```bash
GOPROXY=direct go install github.com/ogpourya/qkill@latest
```

## Usage

```bash
# interactive TUI (default sort: RAM)
qkill
qkill -c  # sort by CPU
qkill -d  # sort by disk I/O rate
qkill -n  # sort by network rate
qkill -g  # sort by GPU usage

# kill by pid (% prefix)
qkill %1337

# kill by name (case-insensitive exact match)
qkill chrome

# kill by port
qkill :8080

# force kill (SIGKILL)
qkill -f %1337 chrome :3000
```

Targets are disambiguated by prefix: `%1337` is PID 1337, `:8080` is port 8080, and anything else is a process name. `%` and `:` are used because `#` reads as a shell comment and `@` is ambiguous.

Name matching for CLI targets is case-insensitive and exact: `qkill chrome` kills only a process whose name is exactly `chrome`, not `google-chrome` or `chrome_crashpad`. The interactive TUI search is still substring based. Use `--` before a name that starts with `-`, e.g. `qkill -- -weird-name`. Unknown flags are rejected instead of being treated as names.

A bare number is a name too, so a process literally named `1337` is killed with `qkill 1337`, while `qkill %1337` kills the process whose PID is 1337.

## Interactive TUI

Processes are displayed in bordered boxes. Each header shows PID, RAM, CPU%, disk I/O rate, network rate, GPU usage, and process name, followed by the wrapped command line.

A sort bar (`RAM CPU DISK NET GPU`) next to the search prompt shows the active metric. Refresh is every 1s (200ms warmup on startup); CPU/disk/net/GPU rates are per-second deltas between ticks. qkill never lists or kills itself.

Selection follows the same PID across refreshes, so re-sorting never slides the cursor onto a neighbor. If the selected process exits on its own, its entry stays pinned and marked red as exited (ENTER on it does nothing) until you move.

Metric sources:

- RAM: resident memory (RSS)
- CPU: per-process CPU%
- DSK: disk read+write rate via process I/O counters
- NET: per-PID rate from `ss -tinp` (Linux) or `nettop` (macOS); 0 when tool missing
- GPU: VRAM/GTT + engine % from Linux DRM fdinfo (AMD/Intel), plus `nvidia-smi pmon` for NVIDIA

## Keys

| key | action |
|-----|--------|
| type | substring search (name, pid, cmdline) |
| `↑` `↓` | navigate |
| `tab` / `shift+tab` | cycle sort: RAM → CPU → DISK → NET → GPU |
| `enter` | kill (SIGTERM) — exits as soon as process dies, prompts for force kill after 3s |
| `ctrl+c` | quit |
| `esc` | clear query / quit (when query empty) |

## Kill flow

1. Press `enter` on a process → sends SIGTERM
2. If the process is already dead → exits immediately
3. Polls every 40ms; exits as soon as the process is gone (zombies count as dead)
4. If still alive after 3 seconds → prompts `process X still running — force kill? (y/n)`
5. `y` / `enter` → sends SIGKILL and exits
6. `n` / `esc` → exits without killing

## License

MIT
