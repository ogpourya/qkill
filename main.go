package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/shirou/gopsutil/v4/process"
)

var (
	subtle = lipgloss.NewStyle().Foreground(lipgloss.Color("#626262"))
	prompt = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF5F87"))
	footer = lipgloss.NewStyle().Foreground(lipgloss.Color("#5F5F5F"))

	boxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#3A3A3A")).
			Padding(0, 1)

	selectedBoxStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("#5FD7FF")).
				Padding(0, 1)

	selfPID = int32(os.Getpid())
)

const refreshInterval = 1500 * time.Millisecond

type sortMode int

const (
	sortByMem sortMode = iota
	sortByCPU
)

func (s sortMode) String() string {
	if s == sortByCPU {
		return "CPU"
	}
	return "RAM"
}

type proc struct {
	pid          int32
	name         string
	nameLower    string
	pidStr       string
	mem          uint64
	cpu          float64
	cmdline      string
	cmdlineLower string
}

// cachedProc preserves *process.Process across ticks so CPUPercent() computes
// real-time interval deltas and static strings (name, cmdline) are read once.
type cachedProc struct {
	handle       *process.Process
	createTime   int64
	name         string
	nameLower    string
	pidStr       string
	cmdline      string
	cmdlineLower string
}

var (
	procCacheMu sync.Mutex
	procCache   = make(map[int32]*cachedProc)
)

var sanitizeReplacer = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ")

type model struct {
	procs    []proc
	query    textinput.Model
	cursor   int
	height   int
	width    int
	sortBy   sortMode
	quitting bool
	killPID  int32
	askForce bool
	killing  bool
	errMsg   string
}

type tickMsg time.Time
type forceKillTickMsg time.Time

func tickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func forceKillCmd() tea.Cmd {
	return tea.Tick(1500*time.Millisecond, func(t time.Time) tea.Msg {
		return forceKillTickMsg(t)
	})
}

type procsMsg []proc

func fetchProcs() tea.Msg {
	pids, err := process.Pids()
	if err != nil {
		return procsMsg(nil)
	}

	procCacheMu.Lock()
	defer procCacheMu.Unlock()

	active := make(map[int32]struct{}, len(pids))
	out := make([]proc, 0, len(pids))

	for _, pid := range pids {
		if pid == selfPID {
			continue
		}
		active[pid] = struct{}{}

		cp, ok := procCache[pid]
		if !ok {
			h, err := process.NewProcess(pid)
			if err != nil {
				continue
			}
			name, err := h.Name()
			if err != nil {
				continue
			}
			name = sanitizeReplacer.Replace(name)
			cmdline, _ := h.Cmdline()
			cmdline = sanitizeReplacer.Replace(cmdline)
			ctime, _ := h.CreateTime()

			// Prime baseline CPU times on first sight so the next tick measures delta
			_, _ = h.CPUPercent()

			cp = &cachedProc{
				handle:       h,
				createTime:   ctime,
				name:         name,
				nameLower:    strings.ToLower(name),
				pidStr:       strconv.Itoa(int(pid)),
				cmdline:      cmdline,
				cmdlineLower: strings.ToLower(cmdline),
			}
			procCache[pid] = cp
		} else {
			// Guard against PID reuse
			if ctime, err := cp.handle.CreateTime(); err != nil || (cp.createTime != 0 && ctime != cp.createTime) {
				delete(procCache, pid)
				continue
			}
		}

		var mem uint64
		if mi, err := cp.handle.MemoryInfo(); err == nil && mi != nil {
			mem = mi.RSS
		}
		cpuPct, _ := cp.handle.CPUPercent()

		out = append(out, proc{
			pid:          pid,
			name:         cp.name,
			nameLower:    cp.nameLower,
			pidStr:       cp.pidStr,
			mem:          mem,
			cpu:          cpuPct,
			cmdline:      cp.cmdline,
			cmdlineLower: cp.cmdlineLower,
		})
	}

	// Evict dead PIDs from cache
	for pid := range procCache {
		if _, alive := active[pid]; !alive {
			delete(procCache, pid)
		}
	}

	return procsMsg(out)
}

func sortProcs(list []proc, mode sortMode) {
	sort.SliceStable(list, func(i, j int) bool {
		if mode == sortByCPU {
			if list[i].cpu != list[j].cpu {
				return list[i].cpu > list[j].cpu
			}
			if list[i].mem != list[j].mem {
				return list[i].mem > list[j].mem
			}
		} else {
			if list[i].mem != list[j].mem {
				return list[i].mem > list[j].mem
			}
			if list[i].cpu != list[j].cpu {
				return list[i].cpu > list[j].cpu
			}
		}
		return list[i].nameLower < list[j].nameLower
	})
}

func (m *model) filtered() []proc {
	raw := m.query.Value()
	if raw == "" {
		return m.procs
	}
	q := strings.ToLower(raw)
	out := make([]proc, 0, len(m.procs)/4)
	for _, p := range m.procs {
		if strings.Contains(p.nameLower, q) ||
			strings.Contains(p.pidStr, raw) ||
			strings.Contains(p.cmdlineLower, q) {
			out = append(out, p)
		}
	}
	return out
}

func (m *model) clampCursor() {
	n := len(m.filtered())
	if n == 0 {
		m.cursor = 0
	} else if m.cursor >= n {
		m.cursor = n - 1
	} else if m.cursor < 0 {
		m.cursor = 0
	}
}

func sliceChunk(s string, limit int) (head, tail string) {
	if len(s) <= limit {
		return s, ""
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if cut == 0 {
		return s[:limit], s[limit:]
	}
	return s[:cut], s[cut:]
}

func countChunks(s string, limit int) int {
	if s == "" {
		return 0
	}
	lines := 0
	for s != "" {
		_, s = sliceChunk(s, limit)
		lines++
	}
	return lines
}

func (m model) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, fetchProcs, tickCmd())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		if m.killing || m.quitting {
			return m, nil
		}
		return m, tea.Batch(fetchProcs, tickCmd())

	case procsMsg:
		m.procs = msg
		sortProcs(m.procs, m.sortBy)
		m.clampCursor()
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
					_ = syscall.Kill(int(m.killPID), syscall.SIGKILL)
					m.quitting = true
					return m, tea.Quit
				case "n", "N", "esc":
					m.quitting = true
					return m, tea.Quit
				}
			}
			if msg.String() == "ctrl+c" || msg.String() == "esc" {
				m.quitting = true
				return m, tea.Quit
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
			m.errMsg = ""
			return m, nil

		case "tab", "ctrl+s":
			if m.sortBy == sortByMem {
				m.sortBy = sortByCPU
			} else {
				m.sortBy = sortByMem
			}
			sortProcs(m.procs, m.sortBy)
			m.cursor = 0
			return m, nil

		case "enter":
			list := m.filtered()
			if m.cursor >= 0 && m.cursor < len(list) {
				p := list[m.cursor]
				err := syscall.Kill(int(p.pid), syscall.SIGTERM)
				if errors.Is(err, syscall.ESRCH) {
					m.quitting = true
					return m, tea.Quit
				}
				if errors.Is(err, syscall.EPERM) {
					m.errMsg = fmt.Sprintf("permission denied killing PID %d (try sudo)", p.pid)
					return m, nil
				}
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
	prevQuery := m.query.Value()
	m.query, cmd = m.query.Update(msg)
	if m.query.Value() != prevQuery {
		m.errMsg = ""
		m.clampCursor()
	}
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
			b.WriteString(subtle.Render(fmt.Sprintf(" sent SIGTERM to %d, waiting 1.5s... ", m.killPID)))
		}
		b.WriteString("\n")
		return b.String()
	}

	b.WriteString(prompt.Render("  ⚡"))
	b.WriteString(subtle.Render(" kill pattern "))
	b.WriteString(m.query.View())
	b.WriteString("\n\n")

	list := m.filtered()

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

	boxHeight := func(idx int) int {
		return countChunks(list[idx].cmdline, contentW) + 3
	}

	start, end := 0, len(list)
	if end > 0 {
		cur := m.cursor
		if cur >= end {
			cur = end - 1
		}
		if cur < 0 {
			cur = 0
		}

		used := boxHeight(cur)
		upBudget := (availH - used) / 2
		if upBudget < 0 {
			upBudget = 0
		}

		up := cur
		usedUp := 0
		for up > 0 {
			h := boxHeight(up - 1)
			if usedUp+h > upBudget {
				break
			}
			usedUp += h
			up--
		}

		// Let downward items use whatever vertical space remains
		downBudget := availH - used - usedUp
		down := cur + 1
		usedDown := 0
		for down < len(list) {
			h := boxHeight(down)
			if usedDown+h > downBudget {
				break
			}
			usedDown += h
			down++
		}

		// If near the bottom of the list, give leftover space back to upward items
		remaining := availH - used - usedUp - usedDown
		for up > 0 && remaining > 0 {
			h := boxHeight(up - 1)
			if h > remaining {
				break
			}
			remaining -= h
			up--
		}

		start, end = up, down
	}

	for i := start; i < end; i++ {
		p := list[i]

		curs := " "
		if i == m.cursor {
			curs = "❯"
		}

		header := fmt.Sprintf("%s %d  %s  %5.1f%%  %s", curs, p.pid, fmtMem(p.mem), p.cpu, p.name)
		if w := lipgloss.Width(header); w < contentW {
			header += strings.Repeat(" ", contentW-w)
		}

		var content strings.Builder
		content.WriteString(header)

		if p.cmdline != "" {
			cl := p.cmdline
			for {
				chunk, rest := sliceChunk(cl, contentW)
				content.WriteString("\n")
				line := chunk
				if w := lipgloss.Width(line); w < contentW {
					line += strings.Repeat(" ", contentW-w)
				}
				content.WriteString(line)
				if rest == "" {
					break
				}
				cl = rest
			}
		}

		style := boxStyle
		if i == m.cursor {
			style = selectedBoxStyle
		}

		b.WriteString(style.Render(content.String()))
		b.WriteString("\n")
	}

	if m.errMsg != "" {
		b.WriteString(prompt.Render("  ⚠ " + m.errMsg))
		return b.String()
	}

	sortHint := fmt.Sprintf("sort: %s (tab)", m.sortBy)
	switch {
	case len(list) == 0:
		b.WriteString(subtle.Render("  nothing found • " + sortHint))
	case end < len(list):
		b.WriteString(footer.Render(fmt.Sprintf("  %d total — ↓ more • %s", len(list), sortHint)))
	default:
		b.WriteString(footer.Render(fmt.Sprintf("  %d process%s • %s", len(list), plural(len(list)), sortHint)))
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

func runCLI(targets []string, force bool) {
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: qkill [-c|--cpu] [-f|--force] [pid|name|:port]...")
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
			if int32(pid) != selfPID {
				_ = syscall.Kill(pid, sig)
			}
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
		if p.Pid == selfPID {
			continue
		}
		pname, err := p.Name()
		if err != nil {
			continue
		}
		if strings.EqualFold(pname, name) || strings.Contains(strings.ToLower(pname), lower) {
			_ = syscall.Kill(int(p.Pid), sig)
		}
	}
}

func killPort(port string, sig syscall.Signal) {
	if runtime.GOOS == "linux" {
		for _, pid := range findPIDsByPortLinux(port) {
			if int32(pid) != selfPID {
				_ = syscall.Kill(pid, sig)
			}
		}
		return
	}
	cmd, args := portCmd(port)
	out, err := exec.Command(cmd, args...).Output()
	if err != nil {
		return
	}
	for _, field := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(field)
		if err != nil || int32(pid) == selfPID {
			continue
		}
		_ = syscall.Kill(pid, sig)
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
	targetSockets := make(map[string]struct{})

	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n")[1:] {
			fields := strings.Fields(line)
			if len(fields) < 10 || fields[9] == "0" {
				continue
			}
			parts := strings.Split(fields[1], ":")
			if len(parts) >= 2 && strings.EqualFold(parts[len(parts)-1], hexPort) {
				targetSockets["socket:["+fields[9]+"]"] = struct{}{}
			}
		}
	}
	if len(targetSockets) == 0 {
		return nil
	}

	var pids []int
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		fdDir := "/proc/" + entry.Name() + "/fd"
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink(fdDir + "/" + fd.Name())
			if err != nil {
				continue
			}
			if _, ok := targetSockets[link]; ok {
				pids = append(pids, pid)
				break // Found match for this PID; move to next process
			}
		}
	}
	return pids
}

func main() {
	initialSort := sortByMem
	force := false
	var targets []string

	for _, a := range os.Args[1:] {
		switch a {
		case "-c", "--cpu":
			initialSort = sortByCPU
		case "-f", "--force":
			force = true
		default:
			targets = append(targets, a)
		}
	}

	if len(targets) == 0 && !force {
		// Prime CPU baseline once before the first render so initial CPU % is populated
		_ = fetchProcs()

		m := model{
			query:  textinput.New(),
			sortBy: initialSort,
		}
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

	runCLI(targets, force)
}
