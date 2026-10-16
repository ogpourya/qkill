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

Processes are displayed in bordered boxes. Each header shows PID, RAM, CPU%, disk I/O rate, network rate, GPU usage, and process name, followed by the wrapped command line.

A sort bar (`RAM CPU DISK NET GPU`) next to the search prompt shows the active metric. Refresh is every 1s (200ms warmup on startup); CPU/disk/net/GPU rates are per-second deltas between ticks. qkill never lists or kills itself.

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
