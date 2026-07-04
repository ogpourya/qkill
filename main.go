package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/shirou/gopsutil/v4/process"
)

var (
	subtle = lipgloss.NewStyle().Foreground(lipgloss.Color("#626262"))
	prompt = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF5F87"))
	footer = lipgloss.NewStyle().Foreground(lipgloss.Color("#5F5F5F"))
)

const refreshInterval = 1500 * time.Millisecond

type proc struct {
	pid     int32
	name    string
	mem     uint64
	cmdline string
}

type model struct {
	procs    []proc
	query    textinput.Model
	cursor   int
	height   int
	width    int
	quitting bool
	killPID  int32
	askForce bool
	killing  bool
}

type tickMsg time.Time
type forceKillTickMsg time.Time

func tickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func forceKillCmd() tea.Cmd {
	return tea.Tick(3*time.Second, func(t time.Time) tea.Msg {
		return forceKillTickMsg(t)
	})
}

type procsMsg []proc

func fetchProcs() tea.Msg {
	ps, err := process.Processes()
	if err != nil {
		return procsMsg(nil)
	}
	out := make([]proc, 0, len(ps))
	for _, p := range ps {
		name, err := p.Name()
		if err != nil {
			continue
		}
		var mem uint64
		if mi, err := p.MemoryInfo(); err == nil {
			mem = mi.RSS
		}
		cmdline, _ := p.Cmdline()
		out = append(out, proc{pid: p.Pid, name: name, mem: mem, cmdline: cmdline})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].mem > out[j].mem
	})
	return procsMsg(out)
}

func isAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func isAlnum(c byte) bool {
	return isAlpha(c) || (c >= '0' && c <= '9')
}

func shellHighlight(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		switch {
		case s[i] == '#':
			b.WriteString("\033[38;2;95;95;95m" + s[i:] + "\033[0m")
			return b.String()

		case s[i] == '\'':
			j := i + 1
			for j < len(s) && s[j] != '\'' {
				j++
			}
			if j >= len(s) {
				j = len(s) - 1
			}
			b.WriteString("\033[38;2;215;175;0m" + s[i:j+1] + "\033[0m")
			i = j + 1

		case s[i] == '"':
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(s) {
				j = len(s) - 1
			}
			b.WriteString("\033[38;2;95;215;95m" + s[i:j+1] + "\033[0m")
			i = j + 1

		case s[i] == '$' && i+1 < len(s) && (isAlpha(s[i+1]) || s[i+1] == '{'):
			j := i + 1
			if s[j] == '{' {
				j++
				for j < len(s) && s[j] != '}' {
					j++
				}
				if j < len(s) {
					j++
				}
			} else {
				for j < len(s) && (isAlnum(s[j]) || s[j] == '_') {
					j++
				}
			}
			b.WriteString("\033[38;2;95;215;255m" + s[i:j] + "\033[0m")
			i = j

		case s[i] == '|' || s[i] == '&' || s[i] == ';':
			b.WriteString("\033[38;2;255;175;95m" + string(s[i]) + "\033[0m")
			i++

		case s[i] == '>' || s[i] == '<':
			b.WriteString("\033[38;2;255;175;95m" + string(s[i]) + "\033[0m")
			i++

		default:
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String()
}

func (m *model) filtered() []proc {
	q := strings.ToLower(m.query.Value())
	if q == "" {
		return m.procs
	}
	out := make([]proc, 0, len(m.procs)/4)
	for _, p := range m.procs {
		if strings.Contains(strings.ToLower(p.name), q) ||
			strings.Contains(fmt.Sprint(p.pid), m.query.Value()) ||
			strings.Contains(strings.ToLower(p.cmdline), q) {
			out = append(out, p)
		}
	}
	return out
}

func (m model) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, fetchProcs, tickCmd())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		return m, tea.Batch(fetchProcs, tickCmd())

	case procsMsg:
		m.procs = msg
		if m.cursor >= len(m.filtered()) {
			m.cursor = 0
		}
		return m, nil

	case forceKillTickMsg:
		exists, err := process.PidExists(m.killPID)
		if err == nil && exists {
			m.askForce = true
			return m, nil
		}
		m.quitting = true
		return m, tea.Quit

	case tea.WindowSizeMsg:
		m.height = msg.Height
		m.width = msg.Width
		return m, nil

	case tea.KeyMsg:
		if m.quitting {
			return m, tea.Quit
		}

		if m.killing {
			if m.askForce {
				switch msg.String() {
				case "y", "Y", "enter":
					syscall.Kill(int(m.killPID), syscall.SIGKILL)
					m.quitting = true
					return m, tea.Quit
				case "n", "N", "esc":
					m.quitting = true
					return m, tea.Quit
				}
			}
			return m, nil
		}

		switch msg.String() {
		case "ctrl+c":
			m.quitting = true
			return m, tea.Quit

		case "esc":
			if m.query.Value() == "" {
				m.quitting = true
				return m, tea.Quit
			}
			m.query.SetValue("")
			m.cursor = 0
			return m, nil

		case "enter":
			list := m.filtered()
			if m.cursor >= 0 && m.cursor < len(list) {
				p := list[m.cursor]
				syscall.Kill(int(p.pid), syscall.SIGTERM)
				m.killPID = p.pid
				m.killing = true
				return m, tea.Batch(textinput.Blink, forceKillCmd())
			}
			return m, nil

		case "up":
			if m.cursor > 0 {
				m.cursor--
			}
			return m, nil

		case "down":
			list := m.filtered()
			if m.cursor < len(list)-1 {
				m.cursor++
			}
			return m, nil
		}
	}

	var cmd tea.Cmd
	m.query, cmd = m.query.Update(msg)
	return m, cmd
}

func (m model) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder

	if m.killing {
		if m.askForce {
			b.WriteString(prompt.Render("  ⚡"))
			b.WriteString(subtle.Render(fmt.Sprintf(" process %d still running — force kill? (y/n) ", m.killPID)))
		} else {
			b.WriteString(prompt.Render("  ⚡"))
			b.WriteString(subtle.Render(fmt.Sprintf(" sent SIGTERM to %d, waiting 3s... ", m.killPID)))
		}
		b.WriteString("\n")
		return b.String()
	}

	b.WriteString(prompt.Render("  ⚡"))
	b.WriteString(subtle.Render(" kill pattern "))
	b.WriteString(m.query.View())
	b.WriteString("\n\n")

	list := m.filtered()
	if len(list) > 0 && m.cursor >= len(list) {
		m.cursor = 0
	}
	if m.cursor < 0 {
		m.cursor = 0
	}

	boxW := m.width
	if boxW < 40 {
		boxW = 40
	}
	contentW := boxW - 4
	if contentW < 20 {
		contentW = 20
	}

	availH := m.height - 3
	if availH < 3 {
		availH = 3
	}

	start := 0
	end := len(list)
	if end > 0 {
		// walk up from cursor counting box heights
		up := m.cursor
		usedUp := 0
		for up >= 0 {
			boxH := (len(list[up].cmdline)+contentW-1)/contentW + 3
			if usedUp+boxH > availH/2 && up < m.cursor {
				up++
				break
			}
			usedUp += boxH
			up--
		}
		if up < 0 {
			up = 0
		}
		start = up

		// walk down from cursor
		down := m.cursor
		usedDown := 0
		for down < len(list) {
			boxH := (len(list[down].cmdline)+contentW-1)/contentW + 3
			if usedDown+boxH > availH/2 && down > m.cursor {
				break
			}
			usedDown += boxH
			down++
		}
		end = down
	}

	for i := start; i < end; i++ {
		p := list[i]

		curs := " "
		if i == m.cursor {
			curs = "❯"
		}

		header := fmt.Sprintf("%s %d  %s  %s", curs, p.pid, fmtMem(p.mem), p.name)
		if w := lipgloss.Width(header); w < contentW {
			header += strings.Repeat(" ", contentW-w)
		}

		var content strings.Builder
		content.WriteString(header)

		if p.cmdline != "" {
			cl := p.cmdline
		outer:
			for {
				switch {
				case len(cl) > contentW:
					content.WriteString("\n")
					line := shellHighlight(cl[:contentW])
					if w := lipgloss.Width(line); w < contentW {
						line += strings.Repeat(" ", contentW-w)
					}
					content.WriteString(line)
					cl = cl[contentW:]
				default:
					content.WriteString("\n")
					line := shellHighlight(cl)
					if w := lipgloss.Width(line); w < contentW {
						line += strings.Repeat(" ", contentW-w)
					}
					content.WriteString(line)
					break outer
				}
			}
		}

		borderColor := "#3A3A3A"
		if i == m.cursor {
			borderColor = "#5FD7FF"
		}
		style := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color(borderColor)).
			Padding(0, 1)

		b.WriteString(style.Render(content.String()))
		b.WriteString("\n")
	}

	switch {
	case len(list) == 0:
		b.WriteString(subtle.Render("  nothing found"))
	case end < len(list):
		b.WriteString(footer.Render(fmt.Sprintf("  %d total — ↓ more", len(list))))
	default:
		b.WriteString(footer.Render(fmt.Sprintf("  %d process%s", len(list), plural(len(list)))))
	}

	return b.String()
}

func fmtMem(b uint64) string {
	switch {
	case b >= 1024*1024*1024:
		return fmt.Sprintf("%.1fG", float64(b)/(1024*1024*1024))
	case b >= 1024*1024:
		return fmt.Sprintf("%.1fM", float64(b)/(1024*1024))
	case b >= 1024:
		return fmt.Sprintf("%.1fK", float64(b)/1024)
	default:
		return fmt.Sprintf("%dB", b)
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "es"
}

// --- CLI ---

func runCLI() {
	force := false
	var targets []string
	for _, a := range os.Args[1:] {
		switch a {
		case "-f", "--force":
			force = true
		default:
			targets = append(targets, a)
		}
	}
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: qkill [-f] <pid|name|:port>...")
		os.Exit(1)
	}

	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}

	for _, t := range targets {
		switch {
		case strings.HasPrefix(t, ":"):
			killPort(t[1:], sig)
		case isDigit(t):
			pid, _ := strconv.Atoi(t)
			syscall.Kill(pid, sig)
		default:
			killByName(t, sig)
		}
	}
}

func isDigit(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func killByName(name string, sig syscall.Signal) {
	ps, _ := process.Processes()
	lower := strings.ToLower(name)
	for _, p := range ps {
		pname, err := p.Name()
		if err != nil {
			continue
		}
		if strings.EqualFold(pname, name) || strings.Contains(strings.ToLower(pname), lower) {
			syscall.Kill(int(p.Pid), sig)
		}
	}
}

func killPort(port string, sig syscall.Signal) {
	if runtime.GOOS == "linux" {
		for _, pid := range findPIDsByPortLinux(port) {
			syscall.Kill(pid, sig)
		}
		return
	}
	cmd, args := portCmd(port)
	out, err := exec.Command(cmd, args...).Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			continue
		}
		syscall.Kill(pid, sig)
	}
}

func portCmd(port string) (string, []string) {
	switch runtime.GOOS {
	case "darwin":
		return "lsof", []string{"-ti", ":" + port}
	default:
		return "fuser", []string{port + "/tcp"}
	}
}

func findPIDsByPortLinux(port string) []int {
	portUint, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return nil
	}
	hexPort := fmt.Sprintf("%04X", portUint)
	var inodes []string
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n")[1:] {
			fields := strings.Fields(line)
			if len(fields) < 10 {
				continue
			}
			parts := strings.Split(fields[1], ":")
			if len(parts) >= 2 && strings.EqualFold(parts[len(parts)-1], hexPort) {
				inodes = append(inodes, fields[9])
			}
		}
	}
	if len(inodes) == 0 {
		return nil
	}
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		fds, err := os.ReadDir("/proc/" + entry.Name() + "/fd")
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink("/proc/" + entry.Name() + "/fd/" + fd.Name())
			if err != nil {
				continue
			}
			for _, ino := range inodes {
				if link == "socket:["+ino+"]" {
					return []int{pid}
				}
			}
		}
	}
	return nil
}

func main() {
	if len(os.Args) < 2 {
		m := model{query: textinput.New()}
		m.query.Placeholder = "..."
		m.query.Prompt = ""
		m.query.Focus()
		m.query.CharLimit = 64
		m.query.Width = 32
		m.query.TextStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFFFFF"))
		m.query.PlaceholderStyle = subtle

		if _, err := tea.NewProgram(m).Run(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	runCLI()
}
