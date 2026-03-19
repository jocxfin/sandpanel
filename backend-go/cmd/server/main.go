package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"sandpanel/backend/internal/api"
	"sandpanel/backend/internal/config"
	"sandpanel/backend/internal/fleet"
	"sandpanel/backend/internal/monitor"
	"sandpanel/backend/internal/process"
	"sandpanel/backend/internal/rcon"
	"sandpanel/backend/internal/state"
	"sandpanel/backend/internal/steamcmd"
	"sandpanel/backend/internal/ws"
)

func main() {
	cli := parseCLIArgs(os.Args[1:])
	applyLogLevel(cli.LogLevel)

	configRoot := env("CONFIG_ROOT", "/data/config")
	logRoot := env("LOG_ROOT", "/data/logs")
	binary := env("GAME_BINARY", "/opt/insurgency/Insurgency/Binaries/Linux/InsurgencyServer-Linux-Shipping")
	addr := env("BIND_ADDR", ":8080")
	rconHost := env("RCON_HOST", "127.0.0.1")
	rconPort := envInt("RCON_PORT", 27015)
	gamePort := envInt("GAME_PORT", 27102)
	queryPort := envInt("QUERY_PORT", 27131)
	steamcmdBin := env("STEAMCMD_BIN", "/home/steam/steamcmd/steamcmd.sh")
	steamInstallDir := env("STEAMCMD_INSTALL_DIR", "/opt/insurgency")
	rconPass := env("RCON_PASSWORD", "")
	dataRoot := env("DATA_ROOT", "/data")

	if err := config.EnsureConfigTree(configRoot); err != nil {
		log.Fatalf("failed to initialize configs: %v", err)
	}

	defaultProfile := state.Profile{
		ID:           "default",
		Name:         "Default",
		ConfigRoot:   configRoot,
		LogRoot:      logRoot,
		GamePort:     gamePort,
		QueryPort:    queryPort,
		RCONPort:     rconPort,
		RCONPassword: rconPass,
		DefaultMap:   "Hideout",
		Scenario:     "Scenario_Hideout_Checkpoint_Security",
		Mutators:     []string{},
	}
	store, err := state.New(filepath.Join(dataRoot, "state", "state.json"), defaultProfile)
	if err != nil {
		log.Fatalf("failed to initialize state: %v", err)
	}
	if err := ensureBuiltInProfiles(store, dataRoot, defaultProfile); err != nil {
		log.Fatalf("failed to provision built-in profiles: %v", err)
	}

	hub := ws.NewHub()
	fleetManager := fleet.New(binary, dataRoot, hub)
	monitorManager := monitor.New(hub, store)
	for _, cfg := range store.ListMonitors() {
		monitorManager.Upsert(cfg)
	}
	steam := steamcmd.New(steamcmdBin, steamInstallDir, func() (string, string) {
		settings := store.Settings()
		return settings.SteamUsername, settings.SteamPassword
	}, hub)
	srv := api.New(configRoot, store, fleetManager, monitorManager, hub, func() *rcon.Client {
		return rcon.New(rconHost, rconPort, rconPass, 4*time.Second)
	}, steam)
	go runAutomaticUpdates(store, fleetManager, steam)
	go runConfigWatch(store, hub)
	go autoStartProfiles(cli.StartProfiles, store, fleetManager)
	go autoInstallGame(binary, steam)

	// TODO: i will work on this later — log-based stats (kills, headshots, objectives)
	// need more work to properly parse and attribute events from the server log.
	// The log watcher code is in state/logwatcher.go, just not wired up yet.
	// serverLogPath := filepath.Join(logRoot, "server.log")
	// logWatcher := state.NewLogWatcher(store, serverLogPath)
	// logWatcher.Start()

	go runPlayerPolling(store, fleetManager, hub, srv)

	log.Printf("sandpanel backend listening on %s", addr)
	if err := http.ListenAndServe(addr, withCORS(srv.Handler())); err != nil {
		log.Fatal(err)
	}
}

type cliArgs struct {
	StartProfiles []string
	LogLevel      string
}

func parseCLIArgs(args []string) cliArgs {
	out := cliArgs{LogLevel: "info"}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--start", "-s":
			if i+1 < len(args) {
				name := strings.TrimSpace(args[i+1])
				if name != "" {
					out.StartProfiles = append(out.StartProfiles, name)
				}
				i++
			}
		case "--log-level", "-l":
			if i+1 < len(args) {
				level := strings.TrimSpace(strings.ToLower(args[i+1]))
				if level != "" {
					out.LogLevel = level
				}
				i++
			}
		}
	}
	return out
}

func applyLogLevel(level string) {
	level = strings.TrimSpace(strings.ToLower(level))
	switch level {
	case "", "info":
		log.SetFlags(log.LstdFlags)
	case "debug":
		log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)
	case "warn", "error", "fatal":
		log.SetFlags(log.LstdFlags)
	default:
		log.Printf("unknown log level %q, defaulting to info", level)
		log.SetFlags(log.LstdFlags)
	}
}

func autoStartProfiles(names []string, store *state.Store, fleetManager *fleet.Manager) {
	if len(names) == 0 {
		return
	}
	// Let API/server boot complete first so auto-start errors are visible in logs.
	time.Sleep(500 * time.Millisecond)
	profiles := store.ListProfiles()
	seen := map[string]struct{}{}
	for _, requested := range names {
		requested = strings.TrimSpace(requested)
		if requested == "" {
			continue
		}
		var profile state.Profile
		found := false
		for _, item := range profiles {
			if item.ID == requested || strings.EqualFold(item.Name, requested) {
				profile = item
				found = true
				break
			}
		}
		if !found {
			log.Printf("auto-start skipped, profile not found: %s", requested)
			continue
		}
		if _, ok := seen[profile.ID]; ok {
			continue
		}
		seen[profile.ID] = struct{}{}
		if err := fleetManager.Start(context.Background(), profile, process.StartRequest{}); err != nil {
			log.Printf("auto-start failed profile=%s (%s): %v", profile.ID, profile.Name, err)
			continue
		}
		log.Printf("auto-started profile=%s (%s)", profile.ID, profile.Name)
	}
}

func autoInstallGame(gameBinary string, steam *steamcmd.Manager) {
	if _, err := os.Stat(gameBinary); err == nil {
		return // game already installed
	}
	log.Printf("game binary not found at %s — starting automatic install via SteamCMD", gameBinary)
	// Wait for the HTTP server to be ready so the install progress is visible in the UI
	time.Sleep(2 * time.Second)
	if err := steam.Install(false, ""); err != nil {
		log.Printf("auto-install failed: %v", err)
	}
}

func ensureBuiltInProfiles(store *state.Store, dataRoot string, defaultProfile state.Profile) error {
	base := filepath.Join(dataRoot, "profiles")
	normalizedDefault := defaultProfile
	if existing, ok := store.GetProfile("default"); ok {
		normalizedDefault = existing
		if strings.TrimSpace(normalizedDefault.Name) == "" {
			normalizedDefault.Name = defaultProfile.Name
		}
		if strings.TrimSpace(normalizedDefault.ConfigRoot) == "" {
			normalizedDefault.ConfigRoot = defaultProfile.ConfigRoot
		}
		if strings.TrimSpace(normalizedDefault.LogRoot) == "" {
			normalizedDefault.LogRoot = defaultProfile.LogRoot
		}
		if normalizedDefault.GamePort == 0 {
			normalizedDefault.GamePort = defaultProfile.GamePort
		}
		if normalizedDefault.QueryPort == 0 {
			normalizedDefault.QueryPort = defaultProfile.QueryPort
		}
		if normalizedDefault.RCONPort == 0 {
			normalizedDefault.RCONPort = defaultProfile.RCONPort
		}
		if strings.TrimSpace(normalizedDefault.RCONPassword) == "" {
			normalizedDefault.RCONPassword = defaultProfile.RCONPassword
		}
		if strings.TrimSpace(normalizedDefault.DefaultMap) == "" {
			normalizedDefault.DefaultMap = defaultProfile.DefaultMap
		}
		if strings.TrimSpace(normalizedDefault.Scenario) == "" {
			normalizedDefault.Scenario = defaultProfile.Scenario
		}
		if normalizedDefault.Mutators == nil {
			normalizedDefault.Mutators = []string{}
		}
	} else {
		normalizedDefault.Mutators = []string{}
	}
	if _, err := store.UpsertProfile(normalizedDefault); err != nil {
		return err
	}
	if err := config.EnsureConfigTree(normalizedDefault.ConfigRoot); err != nil {
		return err
	}
	if err := os.MkdirAll(normalizedDefault.LogRoot, 0o755); err != nil {
		return err
	}

	basicProfile := state.Profile{
		ID:           "basic-default",
		Name:         "Basic Default",
		ConfigRoot:   filepath.Join(base, "basic-default", "config"),
		LogRoot:      filepath.Join(base, "basic-default", "logs"),
		GamePort:     normalizedDefault.GamePort + 120,
		QueryPort:    normalizedDefault.QueryPort + 120,
		RCONPort:     normalizedDefault.RCONPort + 20,
		RCONPassword: "basic-default-pass",
		DefaultMap:   "Farmhouse",
		Scenario:     "Scenario_Farmhouse_Checkpoint_Security",
		Mutators:     []string{},
	}
	_, exists := store.GetProfile(basicProfile.ID)
	if existing, ok := store.GetProfile(basicProfile.ID); ok {
		basicProfile = existing
		if strings.TrimSpace(basicProfile.Name) == "" {
			basicProfile.Name = "Basic Default"
		}
		if strings.TrimSpace(basicProfile.ConfigRoot) == "" {
			basicProfile.ConfigRoot = filepath.Join(base, basicProfile.ID, "config")
		}
		if strings.TrimSpace(basicProfile.LogRoot) == "" {
			basicProfile.LogRoot = filepath.Join(base, basicProfile.ID, "logs")
		}
		if basicProfile.GamePort == 0 {
			basicProfile.GamePort = normalizedDefault.GamePort + 120
		}
		if basicProfile.QueryPort == 0 {
			basicProfile.QueryPort = normalizedDefault.QueryPort + 120
		}
		if basicProfile.RCONPort == 0 {
			basicProfile.RCONPort = normalizedDefault.RCONPort + 20
		}
		if strings.TrimSpace(basicProfile.RCONPassword) == "" {
			basicProfile.RCONPassword = "basic-default-pass"
		}
		if strings.TrimSpace(basicProfile.DefaultMap) == "" {
			basicProfile.DefaultMap = "Farmhouse"
		}
		if strings.TrimSpace(basicProfile.Scenario) == "" {
			basicProfile.Scenario = "Scenario_Farmhouse_Checkpoint_Security"
		}
		if basicProfile.Mutators == nil {
			basicProfile.Mutators = []string{}
		}
	}
	if _, err := store.UpsertProfile(basicProfile); err != nil {
		return err
	}
	if err := config.EnsureConfigTree(basicProfile.ConfigRoot); err != nil {
		return err
	}
	if err := os.MkdirAll(basicProfile.LogRoot, 0o755); err != nil {
		return err
	}
	if !exists {
		if err := seedBasicProfileConfig(basicProfile.ConfigRoot); err != nil {
			return err
		}
	}
	return nil
}

func seedBasicProfileConfig(root string) error {
	type fileDef struct {
		name string
		body string
	}
	files := []fileDef{
		{name: "Mods.txt", body: "\n"},
		{name: "ModsState.json", body: "[]\n"},
		{name: "MapCycle.txt", body: "Farmhouse?Scenario=Scenario_Farmhouse_Checkpoint_Security\n"},
		{name: "ModScenarios.txt", body: "\n"},
		{name: "Motd.txt", body: "Basic default profile\n"},
		{name: "Notes.txt", body: "Auto-created starter profile.\n"},
	}
	for _, file := range files {
		path := filepath.Join(root, file.name)
		if err := os.WriteFile(path, []byte(file.body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func envInt(k string, fallback int) int {
	if v := os.Getenv(k); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil {
			return n
		}
	}
	return fallback
}

func runAutomaticUpdates(store *state.Store, fleetManager *fleet.Manager, steam *steamcmd.Manager) {
	for {
		settings := store.Settings()
		interval := settings.UpdateIntervalMinutes
		if interval <= 0 {
			interval = 3
		}
		if settings.AutomaticUpdates {
			status, err := steam.CheckForUpdate("")
			if err == nil && status.UpdateAvailable && !status.Running {
				running := false
				for _, st := range fleetManager.ListStatuses(store.ListProfiles()) {
					if st.Running {
						running = true
						break
					}
				}
				if !running {
					_ = steam.Install(false, "")
				}
			}
		}
		time.Sleep(time.Duration(interval) * time.Minute)
	}
}

func runConfigWatch(store *state.Store, hub *ws.Hub) {
	last := map[string]string{}
	for {
		for _, profile := range store.ListProfiles() {
			if strings.TrimSpace(profile.ConfigRoot) == "" {
				continue
			}
			entries, err := os.ReadDir(profile.ConfigRoot)
			if err != nil {
				continue
			}
			for _, entry := range entries {
				if entry.IsDir() {
					continue
				}
				name := entry.Name()
				lower := strings.ToLower(name)
				if !(strings.HasSuffix(lower, ".ini") || strings.HasSuffix(lower, ".txt") || strings.HasSuffix(lower, ".json")) {
					continue
				}
				body, err := os.ReadFile(filepath.Join(profile.ConfigRoot, name))
				if err != nil {
					continue
				}
				sum := sha1.Sum(body)
				hash := hex.EncodeToString(sum[:])
				key := profile.ID + ":" + name
				if last[key] != "" && last[key] != hash {
					payload, _ := json.Marshal(map[string]any{
						"type":    "configChanged",
						"file":    name,
						"profile": profile.ID,
						"time":    time.Now().UTC(),
					})
					hub.Broadcast(payload)
				}
				last[key] = hash
			}
		}
		time.Sleep(2 * time.Second)
	}
}

func runPlayerPolling(store *state.Store, fleetManager *fleet.Manager, hub *ws.Hub, apiSrv *api.Server) {
	for {
		time.Sleep(10 * time.Second)
		profiles := store.ListProfiles()
		statuses := fleetManager.ListStatuses(profiles)
		for _, profile := range profiles {
			st, ok := statuses[profile.ID]
			fleetRunning := ok && st.Running

			var players, bots []rcon.Player
			var err error

			if fleetRunning {
				// Server is tracked by fleet — use fleet's RCON connection
				players, bots, err = fleetManager.ListPlayers(profile.ID)
				if err != nil {
					fmt.Fprintf(os.Stderr, "[SandPanel] player poll fleet RCON error for %s: %v\n", profile.ID, err)
					continue
				}
			} else {
				// Server may be running but fleet doesn't know (e.g. after container restart).
				// Try direct RCON connection using profile's credentials.
				if profile.RCONPassword == "" || profile.RCONPort == 0 {
					continue
				}
				client := rcon.New("127.0.0.1", profile.RCONPort, profile.RCONPassword, 4*time.Second)
				players, bots, err = client.ListPlayers()
				client.Close()
				if err != nil {
					// Not an error worth logging — server is simply not running
					continue
				}
				fmt.Fprintf(os.Stderr, "[SandPanel] discovered orphan server for profile %s via direct RCON (%d players, %d bots)\n",
					profile.ID, len(players), len(bots))
			}

			obs := make([]state.PlayerObservation, 0, len(players)+len(bots))
			for _, p := range players {
				obs = append(obs, state.PlayerObservation{
					SteamID:    p.SteamID,
					Name:       p.Name,
					Score:      p.Score,
					IP:         p.IP,
					PlatformID: p.PlatformID,
					IsBot:      false,
				})
			}
			for _, b := range bots {
				obs = append(obs, state.PlayerObservation{
					SteamID:    b.SteamID,
					Name:       b.Name,
					Score:      b.Score,
					IP:         b.IP,
					PlatformID: b.PlatformID,
					IsBot:      true,
				})
			}
			events, live, err := store.ObservePlayers(profile.ID, profile.Name, obs)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[SandPanel] observe players error for %s: %v\n", profile.ID, err)
				continue
			}
			if len(events) > 0 {
				go apiSrv.SendJoinLeaveMessages(profile.ID, events, len(live))
				payload, _ := json.Marshal(map[string]any{
					"type":       "playerUpdate",
					"profileId":  profile.ID,
					"events":     events,
					"liveCount":  len(live),
				})
				hub.Broadcast(payload)
			} else if len(live) > 0 {
				payload, _ := json.Marshal(map[string]any{
					"type":       "playerUpdate",
					"profileId":  profile.ID,
					"events":     events,
					"liveCount":  len(live),
				})
				hub.Broadcast(payload)
			}
		}
	}
}

