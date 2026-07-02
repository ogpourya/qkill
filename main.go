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
	arrow  = lipgloss.NewStyle().Foreground(lipgloss.Color("#5FD7FF"))
	hl     = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFD700"))
	pidC   = lipgloss.NewStyle().Foreground(lipgloss.Color("#757575"))
	nameC  = lipgloss.NewStyle().Foreground(lipgloss.Color("#D0D0D0"))
	bar    = lipgloss.NewStyle().Padding(0, 1).Background(lipgloss.Color("#3A3A3A"))
	footer = lipgloss.NewStyle().Foreground(lipgloss.Color("#5F5F5F"))
)

const refreshInterval = 1500 * time.Millisecond

type proc struct {
	pid  int32
	name string
}

type model struct {
	procs    []proc
	query    textinput.Model
	cursor   int
	height   int
	quitting bool
}

type tickMsg time.Time

func tickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg {
		return tickMsg(t)
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
		out = append(out, proc{pid: p.Pid, name: name})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].name) < strings.ToLower(out[j].name)
	})
	return procsMsg(out)
}

func fuzzyMatch(s, q string) bool {
	if q == "" {
		return true
	}
	qi := 0
	for i := 0; i < len(s) && qi < len(q); i++ {
		if s[i] == q[qi] {
			qi++
		}
	}
	return qi == len(q)
}

func (m *model) filtered() []proc {
	q := strings.ToLower(m.query.Value())
	if q == "" {
		return m.procs
	}
	out := make([]proc, 0, len(m.procs)/4)
	for _, p := range m.procs {
		if fuzzyMatch(strings.ToLower(p.name), q) || strings.Contains(fmt.Sprint(p.pid), m.query.Value()) {
			out = append(out, p)
		}
	}
	return out
}

func buildName(name, q string) string {
	if q == "" {
		return nameC.Render(name)
	}
	lower := strings.ToLower(name)
	ql := strings.ToLower(q)
	var b strings.Builder
	qi := 0
	inHL := false
	for i := 0; i < len(name); i++ {
		match := qi < len(ql) && lower[i] == ql[qi]
		if match && !inHL {
			b.WriteString("\033[0m\033[38;2;255;215;0m")
			inHL = true
		} else if !match && inHL {
			b.WriteString("\033[0m\033[38;2;208;208;208m")
			inHL = false
		}
		if match {
			qi++
		}
		b.WriteByte(name[i])
	}
	if inHL {
		b.WriteString("\033[0m")
	}
	return b.String()
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

	case tea.WindowSizeMsg:
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		if m.quitting {
			return m, tea.Quit
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
				syscall.Kill(int(list[m.cursor].pid), syscall.SIGTERM)
			}
			m.quitting = true
			return m, tea.Quit

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

	b.WriteString(prompt.Render("  ⚡"))
	b.WriteString(subtle.Render(" kill pattern "))
	b.WriteString(m.query.View())
	b.WriteString("\n\n")

	list := m.filtered()
	if m.cursor >= len(list) {
		m.cursor = 0
	}
	if m.cursor < 0 {
		m.cursor = 0
	}

	maxShow := m.height - 2
	if maxShow < 5 {
		maxShow = 5
	}
	if maxShow > 15 {
		maxShow = 15
	}

	start := m.cursor - maxShow/2
	if start < 0 {
		start = 0
	}
	end := start + maxShow
	if end > len(list) {
		end = len(list)
		start = end - maxShow
		if start < 0 {
			start = 0
		}
	}

	for i := start; i < end; i++ {
		p := list[i]
		pidStr := pidC.Render(fmt.Sprintf("%6d", p.pid))

		var line string
		if i == m.cursor {
			line = arrow.Render("❯") + " " + pidStr + " " + nameC.Render(p.name)
			line = bar.Render(line)
		} else {
			line = "  " + pidStr + " " + buildName(p.name, m.query.Value())
		}
		b.WriteString(line)
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
