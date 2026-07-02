# end

Quickly end processes. Cross-platform. Interactive fuzzy search.

## Install

```bash
go install github.com/ogpourya/end@latest
```

## Usage

```bash
# interactive TUI
end

# kill by pid
end 1337

# kill by name
end chrome

# kill by port
end :8080

# force kill (SIGKILL)
end -f 1337 chrome :3000
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
