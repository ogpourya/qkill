package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	subtle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#626262"))
	prompt     = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF5F87"))
	footer     = lipgloss.NewStyle().Foreground(lipgloss.Color("#5F5F5F"))
	activePill = lipgloss.NewStyle().Foreground(lipgloss.Color("#000000")).Background(lipgloss.Color("#5FD7FF")).Bold(true).Padding(0, 1)
	idlePill   = lipgloss.NewStyle().Foreground(lipgloss.Color("#8A8A8A")).Background(lipgloss.Color("#262626")).Padding(0, 1)

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

const (
	steadyInterval  = 1000 * time.Millisecond
	warmupInterval  = 200 * time.Millisecond
	killGracePeriod = 3 * time.Second
	killPollInterval = 40 * time.Millisecond
)

type sortMode int

const (
	sortByMem sortMode = iota
	sortByCPU
	sortByDisk
	sortByNet
	sortByGPU
	sortModeCount
)

func (s sortMode) String() string {
	switch s {
	case sortByCPU:
		return "CPU"
	case sortByDisk:
		return "DISK"
	case sortByNet:
		return "NET"
	case sortByGPU:
		return "GPU"
	default:
		return "RAM"
	}
}

type proc struct {
	pid          int32
	name         string
	nameLower    string
	pidStr       string
	mem          uint64  // Resident memory (bytes)
	cpu          float64 // CPU usage (%)
	diskRate     uint64  // Disk read+write rate (bytes/sec)
	netRate      uint64  // Network rx+tx rate (bytes/sec)
	gpuMem       uint64  // GPU VRAM/GTT usage (bytes)
	gpuPct       float64 // GPU engine usage (%)
	cmdline      string
	cmdlineLower string
}

type gpuSample struct {
	mem uint64
	pct float64
}

type cachedProc struct {
	handle       *process.Process
	createTime   int64
	name         string
	nameLower    string
	pidStr       string
	cmdline      string
	cmdlineLower string
	lastDisk     uint64
	lastNet      uint64
	lastGPUNs    uint64
	hasNetSample bool
	hasGPUSample bool
	lastSample   time.Time
}

var (
	procCacheMu      sync.Mutex
	procCache        = make(map[int32]*cachedProc)
	sanitizeReplacer = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ")

	toolCheckOnce sync.Once
	hasNvidiaSMI  bool
	hasSS         bool
	hasNettop     bool

	ssPidRe   = regexp.MustCompile(`pid=(\d+)`)
	ssAckedRe = regexp.MustCompile(`bytes_acked:(\d+)`)
	ssRecvRe  = regexp.MustCompile(`bytes_received:(\d+)`)
)

func initTools() {
	toolCheckOnce.Do(func() {
		_, err := exec.LookPath("nvidia-smi")
		hasNvidiaSMI = err == nil
		_, err = exec.LookPath("ss")
		hasSS = err == nil
		_, err = exec.LookPath("nettop")
		hasNettop = err == nil
	})
}

type model struct {
	procs    []proc
	query    textinput.Model
	cursor   int
	height   int
	width    int
	sortBy   sortMode
	quitting bool
	killPID  int32
	killDeadline time.Time
	askForce bool
	killing  bool
	errMsg   string
}

type tickMsg time.Time
type killCheckMsg struct{}

func nextTickCmd(firstRun bool) tea.Cmd {
	delay := steadyInterval
	if firstRun {
		delay = warmupInterval
	}
	return tea.Tick(delay, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func checkKillCmd() tea.Cmd {
	return tea.Tick(killPollInterval, func(time.Time) tea.Msg {
		return killCheckMsg{}
	})
}

// isAliveAndNotZombie reports whether pid is still a live process.
// A zombie (dead but unreaped) counts as dead so we quit instead of prompting.
func isAliveAndNotZombie(pid int32) bool {
	if errors.Is(syscall.Kill(int(pid), 0), syscall.ESRCH) {
		return false
	}
	if p, err := process.NewProcess(pid); err == nil {
		if statuses, err := p.Status(); err == nil {
			for _, s := range statuses {
				if s == process.Zombie {
					return false
				}
			}
		} else {
			return false
		}
	} else {
		return false
	}
	return true
}

type procsMsg []proc

func fetchProcs() tea.Msg {
	initTools()

	pids, err := process.Pids()
	if err != nil {
		return procsMsg(nil)
	}

	var (
		netTotals map[int32]uint64
		nvTotals  map[int32]gpuSample
		wg        sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		netTotals = collectNetBytesByPID()
	}()
	go func() {
		defer wg.Done()
		nvTotals = collectNvidiaGPUByPID()
	}()
	wg.Wait()

	now := time.Now()

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

			_, _ = h.CPUPercent()
			var initDisk uint64
			if ioStat, err := h.IOCounters(); err == nil && ioStat != nil {
				initDisk = ioStat.ReadBytes + ioStat.WriteBytes
			}
			var initGPUNs uint64
			if runtime.GOOS == "linux" {
				_, initGPUNs = readLinuxDRMStats(pid)
			}

			cp = &cachedProc{
				handle:       h,
				createTime:   ctime,
				name:         name,
				nameLower:    strings.ToLower(name),
				pidStr:       strconv.Itoa(int(pid)),
				cmdline:      cmdline,
				cmdlineLower: strings.ToLower(cmdline),
				lastDisk:     initDisk,
				lastNet:      netTotals[pid],
				lastGPUNs:    initGPUNs,
				hasNetSample: netTotals[pid] > 0,
				hasGPUSample: initGPUNs > 0,
				lastSample:   now,
			}
			procCache[pid] = cp
		} else {
			if ctime, err := cp.handle.CreateTime(); err != nil || (cp.createTime != 0 && ctime != cp.createTime) {
				delete(procCache, pid)
				continue
			}
		}

		dt := now.Sub(cp.lastSample).Seconds()
		if dt <= 0 {
			dt = steadyInterval.Seconds()
		}

		// 1. RAM
		var mem uint64
		if mi, err := cp.handle.MemoryInfo(); err == nil && mi != nil {
			mem = mi.RSS
		}

		// 2. CPU
		cpuPct, _ := cp.handle.CPUPercent()

		// 3. Disk I/O rate (bytes/sec)
		var diskRate uint64
		if ioStat, err := cp.handle.IOCounters(); err == nil && ioStat != nil {
			currDisk := ioStat.ReadBytes + ioStat.WriteBytes
			if currDisk >= cp.lastDisk && !cp.lastSample.Equal(now) {
				diskRate = uint64(float64(currDisk-cp.lastDisk) / dt)
			}
			cp.lastDisk = currDisk
		}

		// 4. Network rate (bytes/sec)
		var netRate uint64
		if currNet, exists := netTotals[pid]; exists {
			if cp.hasNetSample && currNet >= cp.lastNet && !cp.lastSample.Equal(now) {
				netRate = uint64(float64(currNet-cp.lastNet) / dt)
			}
			cp.lastNet = currNet
			cp.hasNetSample = true
		}

		// 5. GPU (Native Linux DRM for AMD/Intel + NVIDIA pmon fallback)
		var gpuMem uint64
		var gpuPct float64

		if runtime.GOOS == "linux" {
			drmMem, drmNs := readLinuxDRMStats(pid)
			gpuMem = drmMem
			if drmNs > 0 {
				if cp.hasGPUSample && drmNs >= cp.lastGPUNs && !cp.lastSample.Equal(now) {
					elapsedNs := float64(now.Sub(cp.lastSample).Nanoseconds())
					if elapsedNs > 0 {
						gpuPct = (float64(drmNs-cp.lastGPUNs) / elapsedNs) * 100.0
						if gpuPct > 100.0 {
							gpuPct = 100.0
						}
					}
				}
				cp.lastGPUNs = drmNs
				cp.hasGPUSample = true
			}
		}

		if nv, ok := nvTotals[pid]; ok {
			if nv.mem > gpuMem {
				gpuMem = nv.mem
			}
			if nv.pct > gpuPct {
				gpuPct = nv.pct
			}
		}

		cp.lastSample = now

		out = append(out, proc{
			pid:          pid,
			name:         cp.name,
			nameLower:    cp.nameLower,
			pidStr:       cp.pidStr,
			mem:          mem,
			cpu:          cpuPct,
			diskRate:     diskRate,
			netRate:      netRate,
			gpuMem:       gpuMem,
			gpuPct:       gpuPct,
			cmdline:      cp.cmdline,
			cmdlineLower: cp.cmdlineLower,
		})
	}

	for pid := range procCache {
		if _, alive := active[pid]; !alive {
			delete(procCache, pid)
		}
	}

	return procsMsg(out)
}

// readLinuxDRMStats reads AMD (amdgpu) and Intel (i915/xe) GPU usage directly
// from the Linux kernel DRM subsystem via /proc/<pid>/fd and /proc/<pid>/fdinfo.
func readLinuxDRMStats(pid int32) (totalMem uint64, totalEngineNs uint64) {
	pidStr := strconv.Itoa(int(pid))
	fdDir := "/proc/" + pidStr + "/fd"
	fdinfoDir := "/proc/" + pidStr + "/fdinfo"

	fds, err := os.ReadDir(fdDir)
	if err != nil {
		return 0, 0
	}

	seenClients := make(map[string]struct{})

	for _, fd := range fds {
		name := fd.Name()
		// Fast filter: only read fdinfo if the FD points to a GPU device (/dev/dri/*)
		link, err := os.Readlink(fdDir + "/" + name)
		if err != nil || !strings.HasPrefix(link, "/dev/dri/") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(fdinfoDir, name))
		if err != nil {
			continue
		}

		var (
			clientID string
			vram     uint64
			gtt      uint64
			otherMem uint64
			engineNs uint64
		)

		for _, line := range strings.Split(string(data), "\n") {
			switch {
			case strings.HasPrefix(line, "drm-client-id:"):
				clientID = strings.TrimSpace(strings.TrimPrefix(line, "drm-client-id:"))

			case strings.HasPrefix(line, "drm-memory-vram:"):
				vram = parseDRMBytes(line)

			case strings.HasPrefix(line, "drm-memory-gtt:"):
				gtt = parseDRMBytes(line)

			case strings.HasPrefix(line, "drm-resident-memory:"), strings.HasPrefix(line, "drm-total-memory:"):
				if v := parseDRMBytes(line); v > otherMem {
					otherMem = v
				}

			case strings.HasPrefix(line, "drm-engine-"):
				// Matches drm-engine-gfx, drm-engine-compute, drm-engine-dec, drm-engine-enc, etc.
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					if ns, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
						engineNs += ns
					}
				}
			}
		}

		if clientID != "" {
			if _, seen := seenClients[clientID]; seen {
				continue
			}
			seenClients[clientID] = struct{}{}
		}

		// Prefer dedicated VRAM; on AMD APUs or Intel iGPUs where VRAM is 0/tiny, include GTT/resident
		clientMem := vram
		if clientMem == 0 {
			if gtt > 0 {
				clientMem = gtt
			} else {
				clientMem = otherMem
			}
		}

		totalMem += clientMem
		totalEngineNs += engineNs
	}

	return totalMem, totalEngineNs
}

func parseDRMBytes(line string) uint64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	val, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	if len(fields) >= 3 {
		switch strings.ToLower(fields[2]) {
		case "kib", "kb":
			val *= 1024
		case "mib", "mb":
			val *= 1024 * 1024
		case "gib", "gb":
			val *= 1024 * 1024 * 1024
		}
	}
	return val
}

// collectNvidiaGPUByPID uses `nvidia-smi pmon` to capture BOTH Graphics (Vulkan/OpenGL)
// and Compute (CUDA) processes along with their SM % and VRAM usage.
func collectNvidiaGPUByPID() map[int32]gpuSample {
	res := make(map[int32]gpuSample)
	if !hasNvidiaSMI {
		return res
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	// pmon -c 1 -s um outputs: gpu pid type sm mem enc dec jpg ofa fb ccpm command
	out, err := exec.CommandContext(ctx, "nvidia-smi", "pmon", "-c", "1", "-s", "um").Output()
	if err != nil {
		return res
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 9 {
			continue
		}
		pid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		smPct, _ := strconv.ParseFloat(fields[3], 64)
		fbMiB, _ := strconv.ParseUint(fields[8], 10, 64)

		prev := res[int32(pid)]
		prev.mem += fbMiB * 1024 * 1024
		if smPct > prev.pct {
			prev.pct = smPct
		}
		res[int32(pid)] = prev
	}
	return res
}

// collectNetBytesByPID returns cumulative rx+tx network bytes per PID.
func collectNetBytesByPID() map[int32]uint64 {
	res := make(map[int32]uint64)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	switch runtime.GOOS {
	case "linux":
		if !hasSS {
			return res
		}
		out, err := exec.CommandContext(ctx, "ss", "-tinp").Output()
		if err != nil {
			return res
		}
		lines := strings.Split(string(out), "\n")
		var currentPIDs []int32
		for _, line := range lines {
			if matches := ssPidRe.FindAllStringSubmatch(line, -1); len(matches) > 0 {
				currentPIDs = currentPIDs[:0]
				for _, m := range matches {
					if p, err := strconv.Atoi(m[1]); err == nil {
						currentPIDs = append(currentPIDs, int32(p))
					}
				}
				continue
			}
			if len(currentPIDs) > 0 {
				var total uint64
				if m := ssAckedRe.FindStringSubmatch(line); len(m) == 2 {
					v, _ := strconv.ParseUint(m[1], 10, 64)
					total += v
				}
				if m := ssRecvRe.FindStringSubmatch(line); len(m) == 2 {
					v, _ := strconv.ParseUint(m[1], 10, 64)
					total += v
				}
				if total > 0 {
					for _, pid := range currentPIDs {
						res[pid] += total
					}
				}
				currentPIDs = currentPIDs[:0]
			}
		}

	case "darwin":
		if !hasNettop {
			return res
		}
		out, err := exec.CommandContext(ctx, "nettop", "-P", "-L", "1", "-x", "-J", "bytes_in,bytes_out").Output()
		if err != nil {
			return res
		}
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Split(strings.TrimSpace(line), ",")
			if len(fields) < 3 {
				continue
			}
			procField := fields[0]
			dotIdx := strings.LastIndexByte(procField, '.')
			if dotIdx == -1 {
				continue
			}
			pid, err := strconv.Atoi(procField[dotIdx+1:])
			if err != nil {
				continue
			}
			inBytes, _ := strconv.ParseUint(strings.TrimSpace(fields[1]), 10, 64)
			outBytes, _ := strconv.ParseUint(strings.TrimSpace(fields[2]), 10, 64)
			res[int32(pid)] += inBytes + outBytes
		}
	}
	return res
}

func sortProcs(list []proc, mode sortMode) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		switch mode {
		case sortByCPU:
			if a.cpu != b.cpu {
				return a.cpu > b.cpu
			}
		case sortByDisk:
			if a.diskRate != b.diskRate {
				return a.diskRate > b.diskRate
			}
		case sortByNet:
			if a.netRate != b.netRate {
				return a.netRate > b.netRate
			}
		case sortByGPU:
			if a.gpuPct != b.gpuPct {
				return a.gpuPct > b.gpuPct
			}
			if a.gpuMem != b.gpuMem {
				return a.gpuMem > b.gpuMem
			}
		default: // sortByMem
			if a.mem != b.mem {
				return a.mem > b.mem
			}
		}
		if a.mem != b.mem {
			return a.mem > b.mem
		}
		if a.cpu != b.cpu {
			return a.cpu > b.cpu
		}
		return a.nameLower < b.nameLower
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
	// Fetch immediately; procsMsg schedules the warmup tick.
	return tea.Batch(textinput.Blink, fetchProcs)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		if m.killing || m.quitting {
			return m, nil
		}
		return m, fetchProcs

	case procsMsg:
		firstRun := len(m.procs) == 0
		m.procs = msg
		sortProcs(m.procs, m.sortBy)
		m.clampCursor()
		if m.killing || m.quitting {
			return m, nil
		}
		return m, nextTickCmd(firstRun)

	case killCheckMsg:
		if !isAliveAndNotZombie(m.killPID) {
			m.quitting = true
			return m, tea.Quit
		}
		if time.Now().After(m.killDeadline) {
			m.askForce = true
			return m, nil
		}
		return m, checkKillCmd()

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

		case "tab":
			m.sortBy = (m.sortBy + 1) % sortModeCount
			sortProcs(m.procs, m.sortBy)
			m.cursor = 0
			return m, nil

		case "shift+tab":
			m.sortBy = (m.sortBy - 1 + sortModeCount) % sortModeCount
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
				m.killDeadline = time.Now().Add(killGracePeriod)
				return m, tea.Batch(textinput.Blink, checkKillCmd())
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

func renderSortBar(active sortMode) string {
	var b strings.Builder
	modes := []sortMode{sortByMem, sortByCPU, sortByDisk, sortByNet, sortByGPU}
	for i, mode := range modes {
		if i > 0 {
			b.WriteString(" ")
		}
		if mode == active {
			b.WriteString(activePill.Render(mode.String()))
		} else {
			b.WriteString(idlePill.Render(mode.String()))
		}
	}
	b.WriteString(subtle.Render(" ⇥ tab"))
	return b.String()
}

func fmtGPU(mem uint64, pct float64) string {
	if pct >= 0.5 && mem > 0 {
		return fmt.Sprintf("%4.0f%%/%s", pct, fmtBytes(mem))
	}
	if pct >= 0.5 {
		return fmt.Sprintf("%5.1f%%", pct)
	}
	return fmtBytes(mem)
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
			b.WriteString(subtle.Render(fmt.Sprintf(" sent SIGTERM to %d, waiting... ", m.killPID)))
		}
		b.WriteString("\n")
		return b.String()
	}

	b.WriteString(prompt.Render("  ⚡"))
	b.WriteString(subtle.Render(" kill pattern "))
	b.WriteString(m.query.View())
	b.WriteString("  ")
	b.WriteString(renderSortBar(m.sortBy))
	b.WriteString("\n\n")

	list := m.filtered()

	boxW := m.width
	if boxW < 60 {
		boxW = 60
	}
	contentW := boxW - 4
	if contentW < 30 {
		contentW = 30
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

		header := fmt.Sprintf(
			"%s %-6d  RAM %6s  CPU %5.1f%%  DSK %8s  NET %8s  GPU %9s  %s",
			curs,
			p.pid,
			fmtBytes(p.mem),
			p.cpu,
			fmtRate(p.diskRate),
			fmtRate(p.netRate),
			fmtGPU(p.gpuMem, p.gpuPct),
			p.name,
		)
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

	sortHint := fmt.Sprintf("sorted by %s (tab / shift+tab)", m.sortBy)
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

func fmtBytes(b uint64) string {
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

func fmtRate(b uint64) string {
	if b == 0 {
		return "0B/s"
	}
	return fmtBytes(b) + "/s"
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
		fmt.Fprintln(os.Stderr, "Usage: qkill [-m|--ram] [-c|--cpu] [-d|--disk] [-n|--net] [-g|--gpu] [-f|--force] [pid|name|:port]...")
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
				break
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
		case "-m", "--ram", "--mem":
			initialSort = sortByMem
		case "-c", "--cpu":
			initialSort = sortByCPU
		case "-d", "--disk":
			initialSort = sortByDisk
		case "-n", "--net":
			initialSort = sortByNet
		case "-g", "--gpu":
			initialSort = sortByGPU
		case "-f", "--force":
			force = true
		default:
			targets = append(targets, a)
		}
	}

	if len(targets) == 0 && !force {
		m := model{
			query:  textinput.New(),
			sortBy: initialSort,
		}
		m.query.Placeholder = "..."
		m.query.Prompt = ""
		m.query.Focus()
		m.query.CharLimit = 64
		m.query.Width = 24
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
