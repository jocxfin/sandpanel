package process

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"sandpanel/backend/internal/buffer"
	"sandpanel/backend/internal/mods"
	"sandpanel/backend/internal/rcon"
	"sandpanel/backend/internal/ws"
)

type StartRequest struct {
	Map          string   `json:"map"`
	Scenario     string   `json:"scenario"`
	Mutators     []string `json:"mutators"`
	ExtraArgs    []string `json:"extraArgs"`
	SecurityCode string   `json:"securityCode"`
	Password     string   `json:"password"`
	Lighting     string   `json:"lighting"`
}

type Status struct {
	Running     bool      `json:"running"`
	PID         int       `json:"pid,omitempty"`
	Threads     int       `json:"threads,omitempty"`
	CpuPercent  float64   `json:"cpuPercent,omitempty"`
	StartedAt   time.Time `json:"startedAt,omitempty"`
	LastExit    string    `json:"lastExit,omitempty"`
	LastCommand []string  `json:"lastCommand,omitempty"`
}

type Manager struct {
	mu sync.Mutex

	binaryPath string
	configRoot string
	logRoot    string
	instanceID string
	name       string
	cmd        *exec.Cmd
	status     Status
	hub        *ws.Hub
	mods       *mods.Manager

	rconPort  int
	gamePort  int
	queryPort int
	rconPass  string

	pendingModIO     *modIOAuth
	corruptMods      map[string]struct{}
	activeInstallMod string
	installCrashes   map[string]int

	// Two-phase startup: bootstrap without mutators, then RCON travel with mutators.
	pendingMutatorTravel string            // full travel URL with mutators, empty if single-phase
	hasMods              bool              // whether mods are enabled for this launch
	readinessSignals     map[string]bool   // tracks readiness signals from server log
	worldReadyCount      int               // counts WaitingToStart occurrences (need 2 when mods are present)

	serverBuffer *buffer.Ring
	rconBuffer   *buffer.Ring
	chatBuffer   *buffer.Ring
}

type modIOAuth struct {
	Code  string
}

var modCachePathPattern = regexp.MustCompile(`^/home/steam/mod\.io/common/254/mods/([0-9]+)/$`)
var modBeginInstallPattern = regexp.MustCompile(`ModId:\s*([0-9]+),\s*ModEvent:\s*BeginInstall`)
var modInstalledPattern = regexp.MustCompile(`ModId:\s*([0-9]+),\s*ModEvent:\s*Installed`)

type Option func(*Manager)

func WithInstance(id, name, rconPassword string) Option {
	return func(m *Manager) {
		m.instanceID = id
		m.name = name
		if rconPassword != "" {
			m.rconPass = rconPassword
		}
	}
}

// UpdateRCONPassword updates the RCON password used for connecting to this server.
func (m *Manager) UpdateRCONPassword(password string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rconPass = password
}

func New(binaryPath, configRoot, logRoot string, hub *ws.Hub, gamePort, queryPort, rconPort int, opts ...Option) *Manager {
	mgr := &Manager{
		binaryPath:     binaryPath,
		configRoot:     configRoot,
		logRoot:        logRoot,
		hub:            hub,
		mods:           mods.New(configRoot),
		status:         Status{},
		rconPort:       rconPort,
		gamePort:       gamePort,
		queryPort:      queryPort,
		rconPass:       "",  // set via WithInstance option from profile data
		corruptMods:    map[string]struct{}{},
		installCrashes: map[string]int{},
		serverBuffer:   buffer.New(4000),
		rconBuffer:     buffer.New(2000),
		chatBuffer:     buffer.New(1000),
	}
	for _, opt := range opts {
		opt(mgr)
	}
	return mgr
}

func (m *Manager) Start(_ context.Context, req StartRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.refreshProcessStateLocked()
	if m.cmd != nil && m.status.Running {
		return errors.New("server already running")
	}

	if err := os.MkdirAll(m.logRoot, 0o755); err != nil {
		return err
	}
	if err := chownPathRecursive(m.configRoot); err != nil {
		return err
	}
	if err := chownPathRecursive(m.logRoot); err != nil {
		return err
	}
	if err := ensureMachineIDs(); err != nil {
		return err
	}
	enabledMods, err := m.mods.EnabledIDs()
	if err != nil {
		return err
	}
	if err := m.syncRuntimeConfig(); err != nil {
		return err
	}
	logPath := filepath.Join(m.logRoot, "server.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}

	resolvedMutators := m.resolveMutators(req.Mutators)
	mapName := defaultString(req.Map, "Hideout")

	password := strings.TrimSpace(req.Password)

	// Two-phase startup: launch WITHOUT mutators first (bootstrap).
	// The final RCON travel (with mutators and/or after mod downloads) is
	// issued once readiness signals confirm all mods are loaded.
	bootstrapTravel := mods.BuildTravelURL(mapName, req.Scenario, nil, password)
	fullTravel := mods.BuildTravelURL(mapName, req.Scenario, resolvedMutators, password)

	args := []string{
		bootstrapTravel,
		"-Port=" + fmt.Sprint(m.gamePort),
		"-QueryPort=" + fmt.Sprint(m.queryPort),
		"-RconListenPort=" + fmt.Sprint(m.rconPort),
		"-RconPassword=" + m.rconPass,
		"-AdminList=Admins",
		"-MapCycle=MapCycle",
		"-log",
	}
	hasMods := len(enabledMods) > 0
	if hasMods {
		args = append(args,
			"-Mods",
			"-CmdModList="+strings.Join(enabledMods, ","),
			"-ModDownloadTravelTo="+bootstrapTravel,
		)
	}

	req.SecurityCode = strings.TrimSpace(req.SecurityCode)
	securityCode := "none"
	if req.SecurityCode != "" {
		securityCode = req.SecurityCode
		m.pendingModIO = &modIOAuth{Code: req.SecurityCode}
	} else {
		m.pendingModIO = nil
	}
	args = append(args, "-SecurityCode="+securityCode)
	// Password is embedded in the travel URL (see above), not as a CLI flag.
	lighting := strings.TrimSpace(req.Lighting)
	if lighting != "" {
		args = append(args, "-lighting="+lighting)
	}
	args = append(args, sanitizeManagedArgs(req.ExtraArgs)...)

	cmd := exec.Command("su", "-s", "/bin/bash", "steam", "-c", gameCommand(m.binaryPath, args...))
	cmd.Dir = filepath.Dir(m.binaryPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return err
	}

	// Set up two-phase readiness tracking.
	// When mods are enabled we MUST wait for the engine to finish downloading
	// and loading mod paks (signalled by the second WaitingToStart) before
	// issuing the final RCON travel. Without this, a first-time launch or a
	// mod update can cause the server to become accessible before mods are
	// loaded.
	m.hasMods = hasMods
	m.readinessSignals = map[string]bool{}
	m.worldReadyCount = 0
	if hasMods || len(resolvedMutators) > 0 {
		m.pendingMutatorTravel = fullTravel
	} else {
		m.pendingMutatorTravel = ""
	}

	m.cmd = cmd
	m.status = Status{Running: true, PID: cmd.Process.Pid, StartedAt: time.Now(), LastCommand: append([]string{m.binaryPath}, args...)}
	m.serverBuffer.Reset()
	m.rconBuffer.Reset()
	m.chatBuffer.Reset()
	m.broadcastStatus()
	go m.watchProcess(cmd, logFile)
	go m.streamLog(logPath, cmd.Process.Pid)
	return nil
}

func (m *Manager) Stop() error {
	m.mu.Lock()
	m.refreshProcessStateLocked()
	if m.cmd == nil || !m.status.Running {
		m.mu.Unlock()
		return nil
	}
	pid := m.cmd.Process.Pid
	pgid, err := syscall.Getpgid(m.cmd.Process.Pid)
	if err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
	} else {
		_ = m.cmd.Process.Signal(syscall.SIGTERM)
	}
	m.broadcastStatus()
	m.mu.Unlock()

	// Wait for graceful termination first so restart does not race Start().
	graceDeadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(graceDeadline) {
		if !processAlive(pid) {
			return nil
		}
		if !m.Status().Running {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Escalate to SIGKILL if process group did not terminate in time.
	if running := m.Status().Running; running {
		if killPGID, killErr := syscall.Getpgid(pid); killErr == nil {
			_ = syscall.Kill(-killPGID, syscall.SIGKILL)
		} else if proc, findErr := os.FindProcess(pid); findErr == nil {
			_ = proc.Signal(syscall.SIGKILL)
		}
	}
	killDeadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(killDeadline) {
		if !processAlive(pid) || !m.Status().Running {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("failed to stop server process pid=%d", pid)
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshProcessStateLocked()
	status := m.status
	if status.PID > 0 {
		status.Threads = readThreadCount(status.PID)
		status.CpuPercent = readCPUPercent(status.PID)
	}
	return status
}

func (m *Manager) Logs(kind string, limit int) []buffer.Entry {
	switch kind {
	case "rcon":
		return m.rconBuffer.Snapshot(limit)
	case "chat":
		return m.chatBuffer.Snapshot(limit)
	default:
		return m.serverBuffer.Snapshot(limit)
	}
}

func (m *Manager) ExecRCON(command string) (string, error) {
	client := rcon.New("127.0.0.1", m.rconPort, m.rconPass, 4*time.Second)
	defer client.Close()
	m.rconBuffer.Add("TX >> " + command)
	resp, err := client.Exec(command)
	if err != nil {
		m.rconBuffer.Add("ERR << " + err.Error())
		return "", err
	}
	m.rconBuffer.Add("RX << " + resp)
	return resp, nil
}

func (m *Manager) ListPlayers() ([]rcon.Player, []rcon.Player, error) {
	client := rcon.New("127.0.0.1", m.rconPort, m.rconPass, 4*time.Second)
	defer client.Close()
	return client.ListPlayers()
}

func (m *Manager) watchProcess(cmd *exec.Cmd, logFile *os.File) {
	err := cmd.Wait()
	_ = logFile.Close()
	_ = m.syncRuntimeBansBack()
	_ = m.syncRuntimeKeyValueStoreBack()
	m.recoverFromModioStateCrash(err)
	m.purgeCorruptMods()
	m.recoverFromCrash(err)

	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.Running = false
	if err != nil {
		m.status.LastExit = err.Error()
	} else {
		m.status.LastExit = "exited cleanly"
	}
	m.cmd = nil
	m.broadcastStatus()
}

func (m *Manager) streamLog(path string, pid int) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(0, 2); err != nil {
		return
	}
	reader := bufio.NewReader(f)
	for {
		m.mu.Lock()
		running := m.status.Running && m.status.PID == pid
		m.mu.Unlock()
		if !running {
			return
		}

		line, err := reader.ReadString('\n')
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		m.trackInstallProgress(line)
		m.markCorruptModFromLine(line)
		m.consumeModIOBootMarkers(line)
		m.checkReadiness(line)
		m.serverBuffer.Add(line)
		if strings.Contains(line, "LogChat") {
			m.chatBuffer.Add(line)
		}
		if strings.Contains(line, "LogRcon") || strings.Contains(line, "RCON ") {
			m.rconBuffer.Add(line)
		}
		payload, _ := json.Marshal(map[string]any{"type": "log", "line": line, "time": time.Now().UTC(), "instanceId": m.instanceID, "name": m.name})
		m.hub.Broadcast(payload)
	}
}

func (m *Manager) syncRuntimeConfig() error {
	root := filepath.Clean(filepath.Join(filepath.Dir(m.binaryPath), "..", ".."))
	configDir := filepath.Join(root, "Config")
	serverDir := filepath.Join(root, "Config", "Server")
	savedDir := filepath.Join(root, "Saved", "Config", "LinuxServer")
	steamConfigRoot := filepath.Join("/home", "steam", ".config")
	epicRoot := filepath.Join(steamConfigRoot, "Epic")
	epicDir := filepath.Join(epicRoot, "Epic Games")
	epicKeyStore := filepath.Join(epicDir, "KeyValueStore.ini")
	unrealConfigRoot := filepath.Join(epicRoot, "UnrealEngine", "4.27", "Saved", "Config", "Linux")
	crashConfigRoot := filepath.Join(epicRoot, "CrashReportClient", "Saved", "Config", "Linux")
	modCacheDir := filepath.Join(root, "Saved", "PersistentDownloadDir", "Mods")
	prosCacheDir := filepath.Join(root, "Saved", "PersistentDownloadDir", "ProsCache")
	modioDir := filepath.Join(root, "Saved", "Modio")
	modioClientRoot := filepath.Join("/home", "steam", "mod.io")
	modioGameRoot := filepath.Join(modioClientRoot, "254")
	modioServerRoot := filepath.Join(modioGameRoot, "ModServer")
	modioHomeRoot := filepath.Join("/home", "steam", "mod.io", "common", "254")
	modioHomeMetadata := filepath.Join(modioHomeRoot, "metadata")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(serverDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(savedDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(modCacheDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(prosCacheDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(modioDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(modioServerRoot, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(modioHomeMetadata, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(epicDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(unrealConfigRoot, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(crashConfigRoot, 0o755); err != nil {
		return err
	}
	serverFiles := []string{"Game.ini", "Admins.txt", "MapCycle.txt", "Mods.txt", "ModScenarios.txt", "Motd.txt", "Bans.json"}
	savedFiles := []string{"Game.ini", "GameUserSettings.ini", "Engine.ini"}
	for _, name := range serverFiles {
		if err := copyFileIfPresent(filepath.Join(m.configRoot, name), filepath.Join(serverDir, name)); err != nil {
			return err
		}
	}
	for _, name := range savedFiles {
		if err := copyFileIfPresent(filepath.Join(m.configRoot, name), filepath.Join(savedDir, name)); err != nil {
			return err
		}
	}
	if err := reconcileKeyValueStore(filepath.Join(m.configRoot, "KeyValueStore.ini"), epicKeyStore); err != nil {
		return err
	}
	if err := chownPathRecursive(serverDir); err != nil {
		return err
	}
	if err := chownPathRecursive(savedDir); err != nil {
		return err
	}
	if err := chownPathRecursive(configDir); err != nil {
		return err
	}
	if err := chownPathRecursive(modCacheDir); err != nil {
		return err
	}
	if err := chownPathRecursive(prosCacheDir); err != nil {
		return err
	}
	if err := chownPathRecursive(modioDir); err != nil {
		return err
	}
	if err := chownPathRecursive(modioClientRoot); err != nil {
		return err
	}
	if err := chownPathRecursive(modioHomeRoot); err != nil {
		return err
	}
	if err := chownPathRecursive(steamConfigRoot); err != nil {
		return err
	}
	if err := chownPathRecursive(epicDir); err != nil {
		return err
	}
	return nil
}

func (m *Manager) syncRuntimeBansBack() error {
	root := filepath.Clean(filepath.Join(filepath.Dir(m.binaryPath), "..", ".."))
	serverBans := filepath.Join(root, "Config", "Server", "Bans.json")
	dst := filepath.Join(m.configRoot, "Bans.json")
	if _, err := os.Stat(serverBans); err != nil {
		return nil
	}
	return copyFileIfPresent(serverBans, dst)
}

func (m *Manager) syncRuntimeKeyValueStoreBack() error {
	runtimeKeyStore := filepath.Join("/home", "steam", ".config", "Epic", "Epic Games", "KeyValueStore.ini")
	dst := filepath.Join(m.configRoot, "KeyValueStore.ini")
	return reconcileKeyValueStore(dst, runtimeKeyStore)
}

func copyFileIfPresent(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

func reconcileKeyValueStore(configPath, runtimePath string) error {
	configData, configErr := os.ReadFile(configPath)
	if configErr != nil && !os.IsNotExist(configErr) {
		return configErr
	}
	runtimeData, runtimeErr := os.ReadFile(runtimePath)
	if runtimeErr != nil && !os.IsNotExist(runtimeErr) {
		return runtimeErr
	}

	configHas := strings.TrimSpace(string(configData)) != ""
	runtimeHas := strings.TrimSpace(string(runtimeData)) != ""

	switch {
	case configHas && runtimeHas:
		merged := mergeKeyValueStore(configData, runtimeData)
		if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(runtimePath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(configPath, merged, 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(runtimePath, merged, 0o644); err != nil {
			return err
		}
	case configHas:
		if err := os.MkdirAll(filepath.Dir(runtimePath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(runtimePath, configData, 0o644); err != nil {
			return err
		}
	case runtimeHas:
		if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(configPath, runtimeData, 0o644); err != nil {
			return err
		}
	default:
		if _, err := os.Stat(configPath); os.IsNotExist(err) {
			if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(configPath, []byte("\n"), 0o644); err != nil {
				return err
			}
		}
		if _, err := os.Stat(runtimePath); os.IsNotExist(err) {
			if err := os.MkdirAll(filepath.Dir(runtimePath), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(runtimePath, []byte("\n"), 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

func mergeKeyValueStore(configData, runtimeData []byte) []byte {
	cfgVals, cfgOrder := parseKeyValueStore(configData)
	runVals, runOrder := parseKeyValueStore(runtimeData)

	order := make([]string, 0, len(runOrder)+len(cfgOrder))
	seen := map[string]struct{}{}
	for _, key := range runOrder {
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		order = append(order, key)
	}
	for _, key := range cfgOrder {
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		order = append(order, key)
	}

	values := map[string]string{}
	for key, value := range runVals {
		values[key] = value
	}
	for key, value := range cfgVals {
		values[key] = value
	}

	lines := make([]string, 0, len(order))
	for _, key := range order {
		lines = append(lines, key+"="+values[key])
	}
	return []byte(strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n")
}

func parseKeyValueStore(data []byte) (map[string]string, []string) {
	values := map[string]string{}
	order := []string{}
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, ";") || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "[") {
			continue
		}
		parts := strings.SplitN(trimmed, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if key == "" {
			continue
		}
		if _, exists := values[key]; !exists {
			order = append(order, key)
		}
		values[key] = value
	}
	return values, order
}

func (m *Manager) resolveMutators(mutators []string) []string {
	out := make([]string, 0, len(mutators))
	for _, raw := range mutators {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		out = append(out, name)
	}
	return out
}

func (m *Manager) consumeModIOBootMarkers(line string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pendingModIO == nil {
		return
	}
	ll := strings.ToLower(line)
	if strings.Contains(ll, "rcon listening") || (strings.Contains(ll, "modio") && (strings.Contains(ll, "authorized") || strings.Contains(ll, "authenticated") || strings.Contains(ll, "success"))) {
		m.pendingModIO = nil
		payload, _ := json.Marshal(map[string]any{"type": "modio", "message": "Security code consumed; Mod.io args removed for next restart.", "instanceId": m.instanceID, "name": m.name})
		m.hub.Broadcast(payload)
	}
}

// checkReadiness watches server log lines for readiness signals that indicate
// the bootstrap world is ready for the final RCON travel.
//
// When mods are enabled with -ModDownloadTravelTo, the engine performs its own
// internal travel after modio downloads finish. This produces a SECOND
// WaitingToStart. The first WaitingToStart is the initial bootstrap; the second
// is after the engine has loaded/activated mod paks and done a ModDownloadTravelTo.
// We issue our RCON travel only on the second WaitingToStart so that mod
// content (including mutator asset paths) is fully registered.
//
// This two-phase approach is used whenever mods OR mutators are present.
// Without it, first-time mod downloads or mod updates can race the server
// start, causing the server to become accessible before mods are loaded.
//
// When no mods are enabled, a single WaitingToStart + RCON ready is sufficient.
func (m *Manager) checkReadiness(line string) {
	m.mu.Lock()
	if m.pendingMutatorTravel == "" || m.readinessSignals == nil {
		m.mu.Unlock()
		return
	}

	ll := strings.ToLower(line)

	// Signal 1: RCON is listening.
	if strings.Contains(ll, "rcon listening") {
		m.readinessSignals["rcon_ready"] = true
	}

	// Signal 2: Bootstrap world reached a ready state.
	// Count occurrences: first = initial bootstrap, second = after ModDownloadTravelTo.
	if strings.Contains(ll, "waitingtostart") && strings.Contains(ll, "loadingassets") {
		m.worldReadyCount++
	}

	// Determine if all required signals have been received.
	rconReady := m.readinessSignals["rcon_ready"]
	var ready bool
	if m.hasMods {
		// With mods: need RCON + second WaitingToStart (after ModDownloadTravelTo)
		ready = rconReady && m.worldReadyCount >= 2
	} else {
		// Without mods: need RCON + first WaitingToStart
		ready = rconReady && m.worldReadyCount >= 1
	}

	if !ready {
		m.mu.Unlock()
		return
	}

	// All signals received — issue the final mutator travel via RCON.
	travelURL := m.pendingMutatorTravel
	m.pendingMutatorTravel = "" // clear so we only fire once
	m.mu.Unlock()

	travelCmd := "travel " + travelURL
	msg := fmt.Sprintf("[SandPanel] All readiness signals received. Issuing final mutator travel: %s", travelCmd)
	m.serverBuffer.Add(msg)
	m.rconBuffer.Add(msg)
	payload, _ := json.Marshal(map[string]any{
		"type":       "log",
		"line":       msg,
		"time":       time.Now().UTC(),
		"instanceId": m.instanceID,
		"name":       m.name,
	})
	m.hub.Broadcast(payload)

	go func() {
		// Brief delay to let the server fully settle before issuing RCON command.
		time.Sleep(2 * time.Second)
		resp, err := m.ExecRCON(travelCmd)
		if err != nil {
			errMsg := fmt.Sprintf("[SandPanel] RCON travel failed: %s", err.Error())
			m.serverBuffer.Add(errMsg)
			m.rconBuffer.Add(errMsg)
			errPayload, _ := json.Marshal(map[string]any{
				"type":       "log",
				"line":       errMsg,
				"time":       time.Now().UTC(),
				"instanceId": m.instanceID,
				"name":       m.name,
			})
			m.hub.Broadcast(errPayload)
			return
		}
		okMsg := fmt.Sprintf("[SandPanel] RCON travel response: %s", resp)
		m.serverBuffer.Add(okMsg)
		m.rconBuffer.Add(okMsg)

		// Update the lastCommand to reflect the final mutator travel.
		m.mu.Lock()
		m.status.LastCommand = append(m.status.LastCommand, "// RCON: "+travelCmd)
		m.mu.Unlock()
		m.broadcastStatus()

		okPayload, _ := json.Marshal(map[string]any{
			"type":       "log",
			"line":       okMsg,
			"time":       time.Now().UTC(),
			"instanceId": m.instanceID,
			"name":       m.name,
		})
		m.hub.Broadcast(okPayload)
	}()
}

func (m *Manager) markCorruptModFromLine(line string) {
	if !strings.Contains(line, "LogFileManager: Error: Requested read of") {
		return
	}
	match := modCachePathPattern.FindStringSubmatch(line)
	if len(match) < 2 {
		return
	}
	id := strings.TrimSpace(match[1])
	if id == "" {
		return
	}
	m.mu.Lock()
	m.corruptMods[id] = struct{}{}
	m.mu.Unlock()
	payload, _ := json.Marshal(map[string]any{
		"type":       "modio",
		"level":      "warn",
		"instanceId": m.instanceID,
		"name":       m.name,
		"message":    fmt.Sprintf("Detected corrupt mod cache for id=%s. Cache will be purged after process exit.", id),
	})
	m.hub.Broadcast(payload)
}

func (m *Manager) purgeCorruptMods() {
	m.mu.Lock()
	if len(m.corruptMods) == 0 {
		m.mu.Unlock()
		return
	}
	ids := make([]string, 0, len(m.corruptMods))
	for id := range m.corruptMods {
		ids = append(ids, id)
	}
	m.corruptMods = map[string]struct{}{}
	m.mu.Unlock()

	modioRoot := filepath.Clean(filepath.Join("/home", "steam", "mod.io", "common", "254", "mods"))
	downloadRoot := filepath.Clean(filepath.Join(filepath.Dir(m.binaryPath), "..", "..", "Saved", "PersistentDownloadDir", "Mods"))
	for _, id := range ids {
		if modioTarget := filepath.Clean(filepath.Join(modioRoot, id)); strings.HasPrefix(modioTarget, modioRoot+string(filepath.Separator)) {
			_ = os.RemoveAll(modioTarget)
		}
		if dlTarget := filepath.Clean(filepath.Join(downloadRoot, id)); strings.HasPrefix(dlTarget, downloadRoot+string(filepath.Separator)) {
			_ = os.RemoveAll(dlTarget)
		}
		payload, _ := json.Marshal(map[string]any{
			"type":       "modio",
			"level":      "warn",
			"instanceId": m.instanceID,
			"name":       m.name,
			"message":    fmt.Sprintf("Purged corrupt mod cache for id=%s. Restart server to re-download.", id),
		})
		m.hub.Broadcast(payload)
	}
}

func (m *Manager) trackInstallProgress(line string) {
	if match := modBeginInstallPattern.FindStringSubmatch(line); len(match) > 1 {
		id := strings.TrimSpace(match[1])
		if id != "" {
			m.mu.Lock()
			m.activeInstallMod = id
			m.mu.Unlock()
		}
		return
	}
	if match := modInstalledPattern.FindStringSubmatch(line); len(match) > 1 {
		id := strings.TrimSpace(match[1])
		if id == "" {
			return
		}
		m.mu.Lock()
		if m.activeInstallMod == id {
			m.activeInstallMod = ""
		}
		delete(m.installCrashes, id)
		m.mu.Unlock()
	}
}

func (m *Manager) recoverFromCrash(waitErr error) {
	if waitErr == nil {
		return
	}
	lower := strings.ToLower(waitErr.Error())
	if !strings.Contains(lower, "139") && !strings.Contains(lower, "segmentation fault") {
		return
	}
	m.mu.Lock()
	crashModID := strings.TrimSpace(m.activeInstallMod)
	m.activeInstallMod = ""
	crashCount := 0
	if crashModID != "" {
		m.installCrashes[crashModID]++
		crashCount = m.installCrashes[crashModID]
	}
	m.mu.Unlock()
	if crashModID == "" {
		return
	}

	_, _ = m.mods.SetEnabled(crashModID, false)
	modioRoot := filepath.Clean(filepath.Join("/home", "steam", "mod.io", "common", "254", "mods"))
	if modioTarget := filepath.Clean(filepath.Join(modioRoot, crashModID)); strings.HasPrefix(modioTarget, modioRoot+string(filepath.Separator)) {
		_ = os.RemoveAll(modioTarget)
	}
	downloadRoot := filepath.Clean(filepath.Join(filepath.Dir(m.binaryPath), "..", "..", "Saved", "PersistentDownloadDir", "Mods"))
	if dlTarget := filepath.Clean(filepath.Join(downloadRoot, crashModID)); strings.HasPrefix(dlTarget, downloadRoot+string(filepath.Separator)) {
		_ = os.RemoveAll(dlTarget)
	}
	payload, _ := json.Marshal(map[string]any{
		"type":       "modio",
		"level":      "danger",
		"instanceId": m.instanceID,
		"name":       m.name,
		"message":    fmt.Sprintf("Server crashed during mod install for id=%s (count=%d). Mod auto-disabled and quarantined.", crashModID, crashCount),
	})
	m.hub.Broadcast(payload)
}

func (m *Manager) recoverFromModioStateCrash(waitErr error) {
	if waitErr == nil {
		return
	}
	lower := strings.ToLower(waitErr.Error())
	if !strings.Contains(lower, "134") && !strings.Contains(lower, "sigabrt") && !strings.Contains(lower, "abort") {
		return
	}
	gameLogPath := filepath.Clean(filepath.Join(filepath.Dir(m.binaryPath), "..", "..", "Saved", "Logs", "Insurgency.log"))
	b, err := os.ReadFile(gameLogPath)
	if err != nil {
		return
	}
	text := strings.ToLower(string(b))
	if !strings.Contains(text, "initializeuserdataop") || !strings.Contains(text, "sigabrt") {
		return
	}

	ts := time.Now().UTC().Format("20060102-150405")
	userJSON := filepath.Join("/home", "steam", "mod.io", "254", "ModServer", "user.json")
	stateJSON := filepath.Join("/home", "steam", "mod.io", "common", "254", "metadata", "state.json")

	if _, statErr := os.Stat(userJSON); statErr == nil {
		_ = os.Rename(userJSON, userJSON+".bad."+ts)
	}
	if _, statErr := os.Stat(stateJSON); statErr == nil {
		_ = os.Rename(stateJSON, stateJSON+".bad."+ts)
	}
	if err := os.MkdirAll(filepath.Dir(stateJSON), 0o755); err == nil {
		_ = os.WriteFile(stateJSON, []byte("{\"version\":1,\"Mods\":[]}\n"), 0o600)
	}
	_ = chownPathRecursive(filepath.Join("/home", "steam", "mod.io"))

	payload, _ := json.Marshal(map[string]any{
		"type":       "modio",
		"level":      "warn",
		"instanceId": m.instanceID,
		"name":       m.name,
		"message":    "Detected Mod.io userdata crash signature; quarantined user.json/state.json and created a clean metadata state. Restart to continue auth bootstrap.",
	})
	m.hub.Broadcast(payload)
}

func defaultString(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func sanitizeManagedArgs(extraArgs []string) []string {
	out := make([]string, 0, len(extraArgs))
	for _, raw := range extraArgs {
		arg := strings.TrimSpace(raw)
		lower := strings.ToLower(arg)
		if lower == "-mods" ||
			strings.HasPrefix(lower, "-cmdmodlist=") ||
			strings.HasPrefix(lower, "-moddownloadtravelto=") ||
			strings.HasPrefix(lower, "-modioemail=") ||
			strings.HasPrefix(lower, "-modiosecuritycode=") ||
			strings.HasPrefix(lower, "-securitycode=") {
			continue
		}
		out = append(out, raw)
	}
	return out
}

func gameCommand(binary string, args ...string) string {
	parts := []string{shellQuote(binary)}
	for _, arg := range args {
		parts = append(parts, shellQuote(arg))
	}
	return fmt.Sprintf("export HOME=/home/steam; export USER=steam; export LOGNAME=steam; exec %s", strings.Join(parts, " "))
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func chownPathRecursive(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	cmd := exec.Command("chown", "-R", "steam:steam", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to prepare path %s: %v: %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func ensureMachineIDs() error {
	id, err := readOrCreateMachineID("/etc/machine-id")
	if err != nil {
		return err
	}
	if err := readOrWriteMachineID("/var/lib/dbus/machine-id", id); err != nil {
		return err
	}
	steamFallback := "/home/steam/.steam/machine-id"
	if err := readOrWriteMachineID(steamFallback, id); err != nil {
		return err
	}
	if err := chownPathRecursive(filepath.Dir(steamFallback)); err != nil {
		return err
	}
	return nil
}

func (m *Manager) refreshProcessStateLocked() {
	if !m.status.Running || m.status.PID <= 0 {
		return
	}
	if processAlive(m.status.PID) {
		return
	}
	m.status.Running = false
	if strings.TrimSpace(m.status.LastExit) == "" {
		m.status.LastExit = "process not running"
	}
	m.cmd = nil
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func readOrCreateMachineID(path string) (string, error) {
	if body, err := os.ReadFile(path); err == nil {
		trimmed := strings.TrimSpace(string(body))
		if trimmed != "" {
			return trimmed, nil
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id := hex.EncodeToString(buf)
	if err := readOrWriteMachineID(path, id); err != nil {
		return "", err
	}
	return id, nil
}

func readOrWriteMachineID(path, id string) error {
	if body, err := os.ReadFile(path); err == nil {
		if strings.TrimSpace(string(body)) != "" {
			return nil
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(id+"\n"), 0o644)
}

func readThreadCount(pid int) int {
	body, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "Threads:") {
			count := 0
			fmt.Sscanf(line, "Threads:\t%d", &count)
			if count == 0 {
				fmt.Sscanf(line, "Threads: %d", &count)
			}
			return count
		}
	}
	return 0
}

// readSystemCPUPercent reads the overall system CPU utilisation from /proc/stat.
// It performs two instant reads 200ms apart in a goroutine and caches the result
// so that HTTP handlers are never blocked.
var (
	cachedCPU     float64
	cachedCPUOnce sync.Once
)

func initCPUSampler() {
	go func() {
		for {
			idle1, total1 := readProcStat()
			time.Sleep(1 * time.Second)
			idle2, total2 := readProcStat()
			diffIdle := float64(idle2 - idle1)
			diffTotal := float64(total2 - total1)
			if diffTotal > 0 {
				cachedCPU = math.Round((1.0-diffIdle/diffTotal)*1000) / 10
				if cachedCPU < 0 {
					cachedCPU = 0
				}
				if cachedCPU > 100 {
					cachedCPU = 100
				}
			}
		}
	}()
}

func readProcStat() (idle, total uint64) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 {
		return 0, 0
	}
	// First line: cpu <user> <nice> <system> <idle> <iowait> <irq> <softirq> <steal>
	fields := strings.Fields(lines[0])
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0
	}
	var vals [8]uint64
	for i := 1; i < len(fields) && i <= 8; i++ {
		fmt.Sscanf(fields[i], "%d", &vals[i-1])
	}
	for _, v := range vals {
		total += v
	}
	idle = vals[3] + vals[4] // idle + iowait
	return idle, total
}

func readCPUPercent(_ int) float64 {
	cachedCPUOnce.Do(initCPUSampler)
	return cachedCPU
}


func (m *Manager) broadcastStatus() {
	if m.hub == nil {
		return
	}
	status := Status{
		Running:     m.status.Running,
		PID:         m.status.PID,
		Threads:     readThreadCount(m.status.PID),
		CpuPercent:  readCPUPercent(m.status.PID),
		StartedAt:   m.status.StartedAt,
		LastExit:    m.status.LastExit,
		LastCommand: append([]string{}, m.status.LastCommand...),
	}
	payload, _ := json.Marshal(map[string]any{
		"type":       "serverStatus",
		"instanceId": m.instanceID,
		"name":       m.name,
		"status":     status,
	})
	m.hub.Broadcast(payload)
}
