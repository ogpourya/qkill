# qkill

Quickly kill processes. Cross-platform. Interactive fuzzy search.

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

## Keys

| key | action |
|-----|--------|
| type | fuzzy search |
| `↑` `↓` | navigate |
| `tab` / `space` | select |
| `enter` | kill |
| `alt+enter` | force kill |
| `esc` | clear / quit |

## License

MIT
