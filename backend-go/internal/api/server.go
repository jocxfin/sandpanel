package api

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"sandpanel/backend/internal/buffer"
	"sandpanel/backend/internal/catalog"
	"sandpanel/backend/internal/config"
	"sandpanel/backend/internal/configdoc"
	"sandpanel/backend/internal/fleet"
	"sandpanel/backend/internal/ini"
	"sandpanel/backend/internal/modio"
	"sandpanel/backend/internal/mods"
	"sandpanel/backend/internal/monitor"
	"sandpanel/backend/internal/process"
	"sandpanel/backend/internal/query"
	"sandpanel/backend/internal/rcon"
	"sandpanel/backend/internal/state"
	"sandpanel/backend/internal/steamcmd"
	"sandpanel/backend/internal/ws"
)

type Server struct {
	mux         *http.ServeMux
	configRoot  string
	store       *state.Store
	fleet       *fleet.Manager
	monitors    *monitor.Manager
	hub         *ws.Hub
	rconFactory func() *rcon.Client
	steam       *steamcmd.Manager
	wrapperLogs *buffer.Ring
	loginLimiter *rateLimiter
	sessionSecret []byte
}

type ConfigUpdateRequest struct {
	Raw     string      `json:"raw"`
	Updates []ini.Field `json:"updates"`
}

func New(configRoot string, store *state.Store, fleetManager *fleet.Manager, monitorManager *monitor.Manager, hub *ws.Hub, rconFactory func() *rcon.Client, steam *steamcmd.Manager) *Server {
	s := &Server{
		mux:          http.NewServeMux(),
		configRoot:   configRoot,
		store:        store,
		fleet:        fleetManager,
		monitors:     monitorManager,
		hub:          hub,
		rconFactory:  rconFactory,
		steam:        steam,
		wrapperLogs:  buffer.New(4000),
		loginLimiter: newRateLimiter(10, 60),
		sessionSecret: mustSessionSecret(),
	}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler { return s.withAccessLog(s.mux) }

func (s *Server) routes() {
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	s.mux.HandleFunc("/api/auth/login", s.loginHandler)
	s.mux.HandleFunc("/api/auth/logout", s.logoutHandler)
	s.mux.HandleFunc("/api/auth/me", s.withSession(s.meHandler))
	s.mux.HandleFunc("/api/auth/change-password", s.withSession(s.changePasswordHandler))
	s.mux.HandleFunc("/api/auth/me/", s.withSession(s.meHandler))
	s.mux.HandleFunc("/api/config/files", s.withSession(s.listConfigFiles))
	s.mux.HandleFunc("/api/catalog", s.withSession(s.catalogHandler))
	s.mux.HandleFunc("/api/config/file/", s.withSession(s.fileRouter))
	s.mux.HandleFunc("/api/config/parse", s.withSession(s.parseRaw))
	s.mux.HandleFunc("/api/setup/status", s.withSession(s.setupStatusHandler))
	s.mux.HandleFunc("/api/server/start", s.withSession(s.startServer))
	s.mux.HandleFunc("/api/server/stop", s.withSession(s.stopServer))
	s.mux.HandleFunc("/api/server/restart", s.withSession(s.restartServer))
	s.mux.HandleFunc("/api/server/status", s.withSession(s.serverStatus))
	s.mux.HandleFunc("/api/server/players", s.withSession(s.defaultPlayersHandler))
	s.mux.HandleFunc("/api/rcon/exec", s.withSession(s.execRCON))
	s.mux.HandleFunc("/api/command", s.withSession(s.commandHandler))
	s.mux.HandleFunc("/api/mods", s.withSession(s.modsHandler))
	s.mux.HandleFunc("/api/mods/state", s.withSession(s.modsStateHandler))
	s.mux.HandleFunc("/api/mods/add", s.withSession(s.modsAddHandler))
	s.mux.HandleFunc("/api/mods/enable", s.withSession(s.modsEnableHandler))
	s.mux.HandleFunc("/api/mods/details", s.withSession(s.modDetailsHandler))
	s.mux.HandleFunc("/api/modio/explore", s.withSession(s.modioExploreHandler))
	s.mux.HandleFunc("/api/modio/settings", s.withSession(s.modioSettingsHandler))
	s.mux.HandleFunc("/api/modio/request-code", s.withSession(s.modioRequestCodeHandler))
	s.mux.HandleFunc("/api/server/query", s.withSession(s.queryStatus))
	s.mux.HandleFunc("/api/steamcmd/status", s.withSession(s.steamcmdStatus))
	s.mux.HandleFunc("/api/steamcmd/install", s.withSession(s.steamcmdInstall))
	s.mux.HandleFunc("/api/steamcmd/stop", s.withSession(s.steamcmdStop))
	s.mux.HandleFunc("/api/steamcmd/check-update", s.withSession(s.steamcmdCheckUpdate))
	s.mux.HandleFunc("/api/steamcmd/run", s.withSession(s.steamcmdRun))
	s.mux.HandleFunc("/api/profiles", s.withSession(s.profilesHandler))
	s.mux.HandleFunc("/api/profiles/", s.withSession(s.profileRouter))
	s.mux.HandleFunc("/api/instances", s.withSession(s.instancesHandler))
	s.mux.HandleFunc("/api/monitors", s.withSession(s.monitorsHandler))
	s.mux.HandleFunc("/api/monitors/", s.withSession(s.monitorRouter))
	s.mux.HandleFunc("/api/users", s.withSession(s.usersHandler))
	s.mux.HandleFunc("/api/users/", s.withSession(s.userRouter))
	s.mux.HandleFunc("/api/settings", s.withSession(s.settingsHandler))
	s.mux.HandleFunc("/api/players", s.withSession(s.playersHandler))
	s.mux.HandleFunc("/api/players/live", s.withSession(s.livePlayersHandler))
	s.mux.HandleFunc("/api/logs/wrapper", s.withSession(s.wrapperLogsHandler))
	s.mux.HandleFunc("/api/logs/steamcmd", s.withSession(s.steamcmdLogsHandler))
	s.mux.HandleFunc("/api/logs/profile/", s.withSession(s.profileLogsHandler))
	s.mux.HandleFunc("/api/generate-password", s.withSession(s.generatePasswordHandler))
	s.mux.HandleFunc("/api/generate-session-secret", s.withSession(s.generateSessionSecretHandler))
	s.mux.HandleFunc("/api/download/config/", s.withSession(s.downloadConfigHandler))
	s.mux.HandleFunc("/api/download/logs/", s.withSession(s.downloadLogsHandler))
	s.mux.HandleFunc("/api/download/logs-archive", s.withSession(s.downloadLogsArchiveHandler))
	s.mux.HandleFunc("/api/download/wrapper-logs", s.withSession(s.downloadWrapperLogsHandler))
	s.mux.HandleFunc("/api/wrapper/restart", s.withSession(s.wrapperRestartHandler))
	s.mux.HandleFunc("/api/wrapper/update", s.withSession(s.wrapperUpdateHandler))
	s.mux.HandleFunc("/api/moderation/kick", s.withSession(s.kickHandler))
	s.mux.HandleFunc("/api/moderation/ban", s.withSession(s.banHandler))
	s.mux.HandleFunc("/api/moderation/unban", s.withSession(s.unbanHandler))
	s.mux.HandleFunc("/api/mutator-mods", s.withSession(s.mutatorModsHandler))
	s.mux.HandleFunc("/socket.io/", s.withSession(s.socketIOHandler))
	s.mux.HandleFunc("/ws/logs", s.withSession(s.logsWS))
}

func mustSessionSecret() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

func (s *Server) withSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.sessionFromRequest(r); !ok {
			writeErr(w, 401, "authentication required")
			return
		}
		next(w, r)
	}
}

func (s *Server) sessionFromRequest(r *http.Request) (string, bool) {
	cookie, err := r.Cookie("sandpanel_session")
	if err != nil {
		return "", false
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 2 {
		return "", false
	}
	encoded := parts[0]
	signature := parts[1]
	mac := hmac.New(sha256.New, s.sessionSecret)
	mac.Write([]byte(encoded))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(signature), []byte(expected)) {
		return "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", false
	}
	var payload struct {
		UserID string `json:"uid"`
		ExpMS  int64  `json:"exp"`
	}
	if err := json.Unmarshal(decoded, &payload); err != nil {
		return "", false
	}
	if payload.UserID == "" || payload.ExpMS <= time.Now().UnixMilli() {
		return "", false
	}
	if _, ok := s.store.GetUser(payload.UserID); !ok {
		return "", false
	}
	return payload.UserID, true
}

func (s *Server) auditf(format string, args ...any) {
	s.wrapperLogs.Add(fmt.Sprintf(format, args...))
}

func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		s.auditf("HTTP %s %s status=%d duration_ms=%d remote=%s", r.Method, r.URL.RequestURI(), rec.status, time.Since(started).Milliseconds(), r.RemoteAddr)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response does not support hijacking")
	}
	return hijacker.Hijack()
}

func (s *statusRecorder) Flush() {
	flusher, ok := s.ResponseWriter.(http.Flusher)
	if ok {
		flusher.Flush()
	}
}

func (s *Server) loginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	ip := remoteIP(r)
	if !s.loginLimiter.allow(ip) {
		writeErr(w, 429, "too many login attempts, try again later")
		return
	}
	var req struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r.Body, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	user, err := s.store.Authenticate(req.Name, req.Password)
	if err != nil {
		s.auditf("AUTH FAIL user=%s remote=%s", req.Name, r.RemoteAddr)
		writeErr(w, 401, err.Error())
		return
	}
	s.auditf("AUTH OK user=%s role=%s remote=%s", user.Name, user.Role, r.RemoteAddr)
	payload, _ := json.Marshal(map[string]any{"uid": user.ID, "exp": time.Now().Add(12 * time.Hour).UnixMilli()})
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, s.sessionSecret)
	mac.Write([]byte(encoded))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	isSecure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	http.SetCookie(w, &http.Cookie{
		Name:     "sandpanel_session",
		Value:    encoded + "." + signature,
		Path:     "/",
		HttpOnly: true,
		Secure:   isSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   12 * 60 * 60,
	})
	writeJSON(w, 200, user)
}

func (s *Server) logoutHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	isSecure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	http.SetCookie(w, &http.Cookie{
		Name:     "sandpanel_session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   isSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) changePasswordHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	var req struct {
		UserID      string `json:"userId"`
		NewPassword string `json:"newPassword"`
	}
	if err := decodeJSON(r.Body, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if strings.TrimSpace(req.UserID) == "" || strings.TrimSpace(req.NewPassword) == "" {
		writeErr(w, 400, "userId and newPassword are required")
		return
	}
	if len(req.NewPassword) < 8 {
		writeErr(w, 400, "password must be at least 8 characters")
		return
	}
	user, err := s.store.ChangePassword(req.UserID, req.NewPassword)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.auditf("PASSWORD CHANGE user=%s", user.Name)
	writeJSON(w, 200, user)
}

func (s *Server) meHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	id, ok := s.sessionFromRequest(r)
	if !ok {
		writeErr(w, 401, "authentication required")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/auth/me/") {
		raw := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/auth/me/"), "/")
		if raw != "" {
			id = raw
		}
	}
	if id == "" {
		writeErr(w, 400, "invalid user id")
		return
	}
	user, ok := s.store.GetUser(id)
	if !ok {
		writeErr(w, 404, "user not found")
		return
	}
	writeJSON(w, 200, user)
}

func (s *Server) listConfigFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	root, _, err := s.resolveConfigRoot(r.URL.Query().Get("profile"))
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := []string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		lower := strings.ToLower(name)
		if strings.HasSuffix(lower, ".ini") || strings.HasSuffix(lower, ".txt") || strings.HasSuffix(lower, ".json") {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	writeJSON(w, 200, map[string]any{"files": out})
}

func (s *Server) catalogHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	extraMutators := []string{}
	for _, profile := range s.store.ListProfiles() {
		extraMutators = append(extraMutators, profile.Mutators...)
	}
	writeJSON(w, 200, catalog.Load(s.configRoot, extraMutators))
}

func (s *Server) fileRouter(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/config/file/")
	if name == "" || strings.Contains(name, "..") || strings.Contains(name, "/") {
		writeErr(w, 400, "invalid file")
		return
	}
	root, _, err := s.resolveConfigRoot(r.URL.Query().Get("profile"))
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	path, err := safePath(root, name)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	switch r.Method {
	case http.MethodGet:
		b, err := os.ReadFile(path)
		if err != nil {
			writeErr(w, 404, err.Error())
			return
		}
		parsed, err := configdoc.Parse(name, string(b))
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, parsed)
	case http.MethodPut:
		var req ConfigUpdateRequest
		if err := decodeJSON(r.Body, &req); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		if req.Raw == "" {
			writeErr(w, 400, "raw is required")
			return
		}
		var parsed configdoc.Parsed
		if len(req.Updates) > 0 {
			var err error
			parsed, err = configdoc.Apply(name, req.Raw, req.Updates)
			if err != nil {
				writeErr(w, 400, err.Error())
				return
			}
		} else {
			var err error
			parsed, err = configdoc.Parse(name, req.Raw)
			if err != nil {
				writeErr(w, 400, err.Error())
				return
			}
		}
		if err := os.WriteFile(path, []byte(parsed.Raw), 0o644); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, parsed)
	default:
		writeErr(w, 405, "method not allowed")
	}
}

func (s *Server) parseRaw(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	var req struct {
		Name string `json:"name"`
		Raw  string `json:"raw"`
	}
	if err := decodeJSON(r.Body, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	parsed, err := configdoc.Parse(req.Name, req.Raw)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, parsed)
}

func (s *Server) setupStatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	steam := s.steam.Status()
	defaultProfile, _ := s.store.GetProfile("default")
	_, configErr := os.Stat(defaultProfile.ConfigRoot)
	steamcmdPath := os.Getenv("STEAMCMD_BIN")
	if strings.TrimSpace(steamcmdPath) == "" {
		steamcmdPath = "/home/steam/steamcmd/steamcmd.sh"
	}
	gameBinary := os.Getenv("GAME_BINARY")
	if strings.TrimSpace(gameBinary) == "" {
		gameBinary = "/opt/insurgency/Insurgency/Binaries/Linux/InsurgencyServer-Linux-Shipping"
	}
	_, steamcmdErr := os.Stat(steamcmdPath)
	_, gameBinaryErr := os.Stat(gameBinary)
	writeJSON(w, 200, map[string]any{
		"steamcmd":              steam,
		"configRoot":            defaultProfile.ConfigRoot,
		"configRootPresent":     configErr == nil,
		"steamcmdBinary":        steamcmdPath,
		"steamcmdBinaryPresent": steamcmdErr == nil,
		"gameBinary":            gameBinary,
		"gameBinaryPresent":     gameBinaryErr == nil,
		"profiles":              s.store.ListProfiles(),
	})
}

func (s *Server) startServer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	var req process.StartRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	profile, ok := s.store.GetProfile("default")
	if !ok {
		writeErr(w, 500, "default profile missing")
		return
	}
	req.ExtraArgs = s.injectRuntimeTokens(req.ExtraArgs)
	if err := s.fleet.Start(r.Context(), profile, req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.auditf("SERVER START profile=%s map=%s scenario=%s", profile.ID, req.Map, req.Scenario)
	statuses := s.fleet.ListStatuses([]state.Profile{profile})
	writeJSON(w, 200, statuses[profile.ID])
}

func (s *Server) stopServer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	if err := s.fleet.Stop("default"); err != nil && !strings.Contains(err.Error(), "unknown profile") {
		writeErr(w, 500, err.Error())
		return
	}
	if profile, ok := s.store.GetProfile("default"); ok {
		s.auditf("SERVER STOP profile=%s", profile.ID)
		statuses := s.fleet.ListStatuses([]state.Profile{profile})
		writeJSON(w, 200, statuses[profile.ID])
		return
	}
	writeJSON(w, 200, map[string]any{"running": false})
}

func (s *Server) restartServer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	if err := s.fleet.Stop("default"); err != nil && !strings.Contains(err.Error(), "unknown profile") {
		writeErr(w, 500, err.Error())
		return
	}
	var req process.StartRequest
	if err := decodeJSON(r.Body, &req); err != nil && err != io.EOF {
		writeErr(w, 400, err.Error())
		return
	}
	profile, ok := s.store.GetProfile("default")
	if !ok {
		writeErr(w, 500, "default profile missing")
		return
	}
	req.ExtraArgs = s.injectRuntimeTokens(req.ExtraArgs)
	if err := s.fleet.Start(r.Context(), profile, req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	statuses := s.fleet.ListStatuses([]state.Profile{profile})
	writeJSON(w, 200, statuses[profile.ID])
}

func (s *Server) serverStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	root := s.configRoot
	if p, ok := s.store.GetProfile("default"); ok && strings.TrimSpace(p.ConfigRoot) != "" {
		root = p.ConfigRoot
	}
	ids, _ := mods.New(root).EnabledIDs()
	profiles := s.store.ListProfiles()
	statuses := s.fleet.ListStatuses(profiles)
	server := statuses["default"]
	// Use live player data already populated by the monitor watcher
	// (avoids a redundant RCON call that races and times out).
	allBySource := s.store.CurrentPlayers()
	var livePlayers []state.LivePlayer
	for _, players := range allBySource {
		livePlayers = append(livePlayers, players...)
	}
	if livePlayers == nil {
		livePlayers = []state.LivePlayer{}
	}
	result := map[string]any{"server": server, "instances": statuses, "mods": ids, "players": livePlayers, "time": time.Now().UTC()}
	if lastJoin := s.store.LastPlayerJoinedAt(); lastJoin != nil {
		result["lastPlayerJoinedAt"] = lastJoin.UTC()
	}
	writeJSON(w, 200, result)
}

func (s *Server) defaultPlayersHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	// Try RCON-based observation first; fall back to store data if RCON is unavailable.
	livePlayers, bots, err := s.observeProfilePlayers("default", "Default")
	if err != nil || livePlayers == nil {
		allBySource := s.store.CurrentPlayers()
		var flat []state.LivePlayer
		for _, players := range allBySource {
			flat = append(flat, players...)
		}
		if flat == nil {
			flat = []state.LivePlayer{}
		}
		writeJSON(w, 200, map[string]any{"players": flat, "bots": bots})
		return
	}
	writeJSON(w, 200, map[string]any{"players": livePlayers, "bots": bots})
}

func (s *Server) modsHandler(w http.ResponseWriter, r *http.Request) {
	manager, err := s.modsManagerForProfile(r.URL.Query().Get("profile"))
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	switch r.Method {
	case http.MethodGet:
		subs, err := manager.LoadSubscriptions()
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		ids := make([]string, 0, len(subs))
		for _, sub := range subs {
			if sub.Enabled {
				ids = append(ids, sub.ID)
			}
		}
		writeJSON(w, 200, map[string]any{"mods": ids, "subscriptions": subs})
	case http.MethodPut:
		var req struct {
			IDs []string `json:"ids"`
		}
		if err := decodeJSON(r.Body, &req); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		subs, err := manager.SetEnabledIDs(req.IDs)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		enabled := make([]string, 0, len(subs))
		for _, sub := range subs {
			if sub.Enabled {
				enabled = append(enabled, sub.ID)
			}
		}
		s.auditf("MODS SAVE count=%d", len(req.IDs))
		writeJSON(w, 200, map[string]any{"mods": enabled, "subscriptions": subs})
	default:
		writeErr(w, 405, "method not allowed")
	}
}

func (s *Server) modsStateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	manager, err := s.modsManagerForProfile(r.URL.Query().Get("profile"))
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	subs, err := manager.LoadSubscriptions()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	enabled := make([]string, 0, len(subs))
	for _, sub := range subs {
		if sub.Enabled {
			enabled = append(enabled, sub.ID)
		}
	}
	writeJSON(w, 200, map[string]any{"mods": enabled, "subscriptions": subs})
}

func (s *Server) modsAddHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	manager, err := s.modsManagerForProfile(r.URL.Query().Get("profile"))
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(r.Body, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := modio.New(s.configRoot).Subscribe(req.ID); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	subs, err := manager.Add(req.ID)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.auditf("MODS ADD id=%s subscribed=true", strings.TrimSpace(req.ID))
	writeJSON(w, 200, map[string]any{"subscriptions": subs})
}

func (s *Server) modsEnableHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	manager, err := s.modsManagerForProfile(r.URL.Query().Get("profile"))
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	var req struct {
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
	}
	if err := decodeJSON(r.Body, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	modioClient := modio.New(s.configRoot)
	if req.Enabled {
		err = modioClient.Subscribe(req.ID)
	} else {
		err = modioClient.Unsubscribe(req.ID)
		if err == nil {
			err = modioClient.ClearModCache(req.ID)
		}
	}
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	subs, err := manager.SetEnabled(req.ID, req.Enabled)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.auditf("MODS TOGGLE id=%s enabled=%t", strings.TrimSpace(req.ID), req.Enabled)
	writeJSON(w, 200, map[string]any{"subscriptions": subs})
}

func (s *Server) modDetailsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	manager, err := s.modsManagerForProfile(r.URL.Query().Get("profile"))
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	subs, err := manager.LoadSubscriptions()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	ids := make([]string, 0, len(subs))
	for _, sub := range subs {
		ids = append(ids, sub.ID)
	}
	profileID := strings.TrimSpace(r.URL.Query().Get("profile"))
	root := s.configRoot
	if profileID != "" && profileID != "default" {
		profile, ok := s.store.GetProfile(profileID)
		if !ok {
			writeErr(w, 404, "profile not found")
			return
		}
		root = profile.ConfigRoot
	}
	modItems := make([]modio.ModSummary, 0)
	fetchErr := ""
	if len(ids) > 0 {
		modItems, err = modio.New(root).FetchMods(ids)
		if err != nil {
			fetchErr = err.Error()
		}
	}
	byID := map[string]modio.ModSummary{}
	for _, item := range modItems {
		byID[strconv.Itoa(item.ID)] = item
	}
	out := make([]map[string]any, 0, len(subs))
	for _, sub := range subs {
		obj := map[string]any{
			"id":      sub.ID,
			"enabled": sub.Enabled,
		}
		if detail, ok := byID[sub.ID]; ok {
			obj["name"] = detail.Name
			obj["summary"] = detail.Summary
			obj["profileUrl"] = detail.ProfileURL
			obj["author"] = detail.Author
			obj["logo"] = detail.Logo
			obj["subscribers"] = detail.Subscribers
			obj["downloads"] = detail.Downloads
			obj["rating"] = detail.Rating
			obj["tags"] = detail.Tags
			obj["dateUpdated"] = detail.DateUpdated
		} else {
			obj["name"] = "Mod " + sub.ID
		}
		out = append(out, obj)
	}
	resp := map[string]any{"mods": out}
	if fetchErr != "" {
		resp["fetchError"] = fetchErr
	}
	writeJSON(w, 200, resp)
}

func (s *Server) modsManagerForProfile(profileID string) (*mods.Manager, error) {
	root, _, err := s.resolveConfigRoot(profileID)
	if err != nil {
		return nil, err
	}
	return mods.New(root), nil
}

func (s *Server) modioSettingsHandler(w http.ResponseWriter, r *http.Request) {
	manager := modio.New(s.configRoot)
	switch r.Method {
	case http.MethodGet:
		settings, err := manager.GetSettings()
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, settings)
	case http.MethodPut:
		var req modio.Settings
		if err := decodeJSON(r.Body, &req); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		if _, err := manager.UpdateSettings(req); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		s.auditf("MODIO SETTINGS UPDATED terms=%t", req.TermsAccepted)
		settings, err := manager.GetSettings()
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, settings)
	default:
		writeErr(w, 405, "method not allowed")
	}
}

func (s *Server) modioExploreHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	page, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("page")))
	pageSize, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("pageSize")))
	query := modio.ExploreQuery{
		Search:   strings.TrimSpace(r.URL.Query().Get("q")),
		Sort:     strings.TrimSpace(r.URL.Query().Get("sort")),
		Page:     page,
		PageSize: pageSize,
	}
	result, err := modio.New(s.configRoot).ExploreMods(query)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, result)
}

func (s *Server) modioRequestCodeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	var req struct {
		Email string `json:"email"`
	}
	if err := decodeJSON(r.Body, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	email := strings.TrimSpace(req.Email)
	if email == "" {
		writeErr(w, 400, "email is required")
		return
	}
	resp, err := modio.New(s.configRoot).RequestSecurityCode(email)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.auditf("MODIO SECURITY CODE REQUEST email=%s", email)
	msg := "Security code requested from mod.io. Check email, then boot once with the code."
	if raw, ok := resp["message"].(string); ok && strings.TrimSpace(raw) != "" {
		msg = strings.TrimSpace(raw)
	}
	writeJSON(w, 200, map[string]any{
		"status":   "requested",
		"running":  false,
		"message":  msg,
		"provider": resp,
	})
}

func (s *Server) queryStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	profile, ok := s.store.GetProfile("default")
	if !ok {
		writeErr(w, 500, "default profile missing")
		return
	}
	c := query.New("127.0.0.1", profile.QueryPort, 3*time.Second)
	info, infoErr := c.Info()
	rules, rulesErr := c.Rules(-1)
	obj := map[string]any{"info": info, "rules": rules}
	if infoErr != nil {
		obj["infoError"] = infoErr.Error()
	}
	if rulesErr != nil {
		obj["rulesError"] = rulesErr.Error()
	}
	writeJSON(w, 200, obj)
}

func (s *Server) steamcmdStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	writeJSON(w, 200, s.steam.Status())
}

func (s *Server) steamcmdInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	var req struct {
		Validate       bool   `json:"validate"`
		SteamGuardCode string `json:"steamGuardCode"`
	}
	if err := decodeJSON(r.Body, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.steam.Install(req.Validate, req.SteamGuardCode); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.auditf("STEAMCMD INSTALL validate=%t auth=%t guard=%t", req.Validate, strings.TrimSpace(s.store.Settings().SteamUsername) != "", strings.TrimSpace(req.SteamGuardCode) != "")
	writeJSON(w, 200, s.steam.Status())
}

func (s *Server) steamcmdCheckUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	var req struct {
		SteamGuardCode string `json:"steamGuardCode"`
	}
	if err := decodeJSON(r.Body, &req); err != nil && err != io.EOF {
		writeErr(w, 400, err.Error())
		return
	}
	st, err := s.steam.CheckForUpdate(req.SteamGuardCode)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, st)
}

func (s *Server) steamcmdRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	var req struct {
		Args []string `json:"args"`
	}
	if err := decodeJSON(r.Body, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if len(req.Args) == 0 {
		writeErr(w, 400, "args are required")
		return
	}
	for _, arg := range req.Args {
		if !isAllowedSteamCMDArg(arg) {
			writeErr(w, 400, fmt.Sprintf("disallowed steamcmd argument: %s", arg))
			return
		}
	}
	if err := s.steam.Run(req.Args); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.auditf("STEAMCMD RUN %s", strings.Join(req.Args, " "))
	writeJSON(w, 200, s.steam.Status())
}

func (s *Server) steamcmdStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	if err := s.steam.Stop(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.auditf("STEAMCMD STOP")
	writeJSON(w, 200, s.steam.Status())
}

func (s *Server) profilesHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, map[string]any{"profiles": s.store.ListProfiles()})
	case http.MethodPost:
		var req state.Profile
		if err := decodeJSON(r.Body, &req); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			writeErr(w, 400, "name is required")
			return
		}
		if req.GamePort == 0 {
			req.GamePort = 27102
		}
		if req.QueryPort == 0 {
			req.QueryPort = 27131
		}
		if req.RCONPort == 0 {
			req.RCONPort = 27015
		}
		if req.RCONPassword == "" {
			req.RCONPassword = config.GenerateRandomPassword()
		}
		if req.DefaultMap == "" {
			req.DefaultMap = "Hideout"
		}
		if req.Scenario == "" {
			req.Scenario = "Scenario_Hideout_Checkpoint_Security"
		}
		base := filepath.Dir(s.configRoot)
		if req.ID == "" {
			req.ID = ""
		}
		saved, err := s.store.UpsertProfile(req)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if saved.ConfigRoot == "" {
			saved.ConfigRoot = filepath.Join(base, "profiles", saved.ID, "config")
		}
		if saved.LogRoot == "" {
			saved.LogRoot = filepath.Join(base, "profiles", saved.ID, "logs")
		}
		if err := fleet.CloneProfile(s.configRoot, saved.ConfigRoot); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		saved, err = s.store.UpsertProfile(saved)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		// Auto-enable mods required by selected mutators.
		if len(saved.Mutators) > 0 {
			if autoErr := s.ensureMutatorModsEnabled(saved.ConfigRoot, saved.Mutators); autoErr != nil {
				fmt.Fprintf(os.Stderr, "[SandPanel] auto-enable mutator mods: %v\n", autoErr)
			}
		}
		// Sync RCON password to Game.ini and matching monitors.
		if saved.RCONPassword != "" {
			syncRCONPasswordToGameIni(saved.ConfigRoot, saved.RCONPassword)
			s.syncMonitorPasswords(saved)
		}
		writeJSON(w, 200, saved)
	default:
		writeErr(w, 405, "method not allowed")
	}
}

func (s *Server) profileRouter(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/profiles/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeErr(w, 400, "invalid profile path")
		return
	}
	id := parts[0]
	profile, ok := s.store.GetProfile(id)
	if !ok {
		writeErr(w, 404, "profile not found")
		return
	}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, 200, profile)
		case http.MethodPut:
			var req state.Profile
			if err := decodeJSON(r.Body, &req); err != nil {
				writeErr(w, 400, err.Error())
				return
			}
			req.ID = id
			if req.ConfigRoot == "" {
				req.ConfigRoot = profile.ConfigRoot
			}
			if req.LogRoot == "" {
				req.LogRoot = profile.LogRoot
			}
			saved, err := s.store.UpsertProfile(req)
			if err != nil {
				writeErr(w, 500, err.Error())
				return
			}
			// Auto-enable mods required by selected mutators.
			if len(saved.Mutators) > 0 {
				if autoErr := s.ensureMutatorModsEnabled(saved.ConfigRoot, saved.Mutators); autoErr != nil {
					// Log but don't fail the save.
					fmt.Fprintf(os.Stderr, "[SandPanel] auto-enable mutator mods: %v\n", autoErr)
				}
			}
			// Sync RCON password to Game.ini and matching monitors.
			if saved.RCONPassword != "" {
				syncRCONPasswordToGameIni(saved.ConfigRoot, saved.RCONPassword)
				s.syncMonitorPasswords(saved)
			}
			writeJSON(w, 200, saved)
		case http.MethodDelete:
			if err := s.fleet.Stop(id); err != nil && !strings.Contains(err.Error(), "unknown profile") {
				writeErr(w, 500, err.Error())
				return
			}
			if err := s.store.DeleteProfile(id); err != nil {
				writeErr(w, 500, err.Error())
				return
			}
			writeJSON(w, 200, map[string]string{"deleted": id})
		default:
			writeErr(w, 405, "method not allowed")
		}
		return
	}
	action := parts[1]
	switch action {
	case "start":
		if r.Method != http.MethodPost {
			writeErr(w, 405, "method not allowed")
			return
		}
		var req process.StartRequest
		if err := decodeJSON(r.Body, &req); err != nil && err != io.EOF {
			writeErr(w, 400, err.Error())
			return
		}
		req.ExtraArgs = s.injectRuntimeTokens(req.ExtraArgs)
		if err := s.fleet.Start(r.Context(), profile, req); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		status, _ := s.fleet.Status(id)
		writeJSON(w, 200, status)
	case "restart":
		if r.Method != http.MethodPost {
			writeErr(w, 405, "method not allowed")
			return
		}
		_ = s.fleet.Stop(id)
		time.Sleep(1500 * time.Millisecond)
		var req process.StartRequest
		if err := decodeJSON(r.Body, &req); err != nil && err != io.EOF {
			writeErr(w, 400, err.Error())
			return
		}
		req.ExtraArgs = s.injectRuntimeTokens(req.ExtraArgs)
		if err := s.fleet.Start(r.Context(), profile, req); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		status, _ := s.fleet.Status(id)
		writeJSON(w, 200, status)
	case "stop":
		if r.Method != http.MethodPost {
			writeErr(w, 405, "method not allowed")
			return
		}
		if err := s.fleet.Stop(id); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		status, _ := s.fleet.Status(id)
		writeJSON(w, 200, status)
	case "status":
		if r.Method != http.MethodGet {
			writeErr(w, 405, "method not allowed")
			return
		}
		status, err := s.fleet.Status(id)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, status)
	case "players":
		if r.Method != http.MethodGet {
			writeErr(w, 405, "method not allowed")
			return
		}
		livePlayers, bots, err := s.observeProfilePlayers(id, profile.Name)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"players": livePlayers, "bots": bots})
	default:
		writeErr(w, 404, "unknown profile action")
	}
}

func (s *Server) injectRuntimeTokens(extraArgs []string) []string {
	out := append([]string{}, extraArgs...)
	settings := s.store.Settings()

	if token := strings.TrimSpace(settings.GameStatsToken); token != "" {
		if !hasArgPrefix(out, "-GameStatsToken=") {
			out = append(out, "-GameStatsToken="+token)
		}
	}
	if token := strings.TrimSpace(settings.SteamServerToken); token != "" {
		if !hasArgPrefix(out, "+sv_setsteamaccount") && !hasArgPrefix(out, "-SteamServerToken=") {
			out = append(out, "+sv_setsteamaccount", token)
		}
	}
	return out
}

func hasArgPrefix(args []string, prefix string) bool {
	needle := strings.ToLower(strings.TrimSpace(prefix))
	for _, raw := range args {
		arg := strings.ToLower(strings.TrimSpace(raw))
		if strings.HasPrefix(arg, needle) {
			return true
		}
	}
	return false
}

func (s *Server) instancesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	writeJSON(w, 200, map[string]any{"instances": s.fleet.ListStatuses(s.store.ListProfiles())})
}

func (s *Server) monitorsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, map[string]any{"monitors": s.store.ListMonitors(), "status": s.monitors.List()})
	case http.MethodPost:
		var req state.MonitorConfig
		if err := decodeJSON(r.Body, &req); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		if req.Name == "" || req.Host == "" || req.QueryPort == 0 || req.RCONPort == 0 {
			writeErr(w, 400, "name, host, queryPort and rconPort are required")
			return
		}
		saved, err := s.store.UpsertMonitor(req)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		s.monitors.Upsert(saved)
		writeJSON(w, 200, saved)
	default:
		writeErr(w, 405, "method not allowed")
	}
}

func (s *Server) monitorRouter(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/monitors/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeErr(w, 400, "invalid monitor path")
		return
	}
	id := parts[0]
	cfg, ok := s.store.GetMonitor(id)
	if !ok {
		writeErr(w, 404, "monitor not found")
		return
	}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, 200, cfg)
		case http.MethodPut:
			var req state.MonitorConfig
			if err := decodeJSON(r.Body, &req); err != nil {
				writeErr(w, 400, err.Error())
				return
			}
			req.ID = id
			saved, err := s.store.UpsertMonitor(req)
			if err != nil {
				writeErr(w, 500, err.Error())
				return
			}
			s.monitors.Upsert(saved)
			writeJSON(w, 200, saved)
		case http.MethodDelete:
			s.monitors.Delete(id)
			if err := s.store.DeleteMonitor(id); err != nil {
				writeErr(w, 500, err.Error())
				return
			}
			writeJSON(w, 200, map[string]string{"deleted": id})
		default:
			writeErr(w, 405, "method not allowed")
		}
		return
	}
	action := parts[1]
	switch action {
	case "start":
		if r.Method != http.MethodPost {
			writeErr(w, 405, "method not allowed")
			return
		}
		if err := s.monitors.Start(id); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		status, _ := s.monitors.Status(id)
		writeJSON(w, 200, status)
	case "stop":
		if r.Method != http.MethodPost {
			writeErr(w, 405, "method not allowed")
			return
		}
		if err := s.monitors.Stop(id); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		status, _ := s.monitors.Status(id)
		writeJSON(w, 200, status)
	case "status":
		if r.Method != http.MethodGet {
			writeErr(w, 405, "method not allowed")
			return
		}
		status, err := s.monitors.Status(id)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, status)
	default:
		writeErr(w, 404, "unknown monitor action")
	}
}

func (s *Server) usersHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, map[string]any{"users": s.store.ListUsers()})
	case http.MethodPost:
		var req state.User
		if err := decodeJSON(r.Body, &req); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		if req.Name == "" || req.Role == "" {
			writeErr(w, 400, "name and role are required")
			return
		}
		user, err := s.store.UpsertUser(req)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, user)
	default:
		writeErr(w, 405, "method not allowed")
	}
}

func (s *Server) userRouter(w http.ResponseWriter, r *http.Request) {
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/users/"), "/")
	if id == "" {
		writeErr(w, 400, "invalid user id")
		return
	}
	switch r.Method {
	case http.MethodGet:
		user, ok := s.store.GetUser(id)
		if !ok {
			writeErr(w, 404, "user not found")
			return
		}
		writeJSON(w, 200, user)
	case http.MethodDelete:
		if err := s.store.DeleteUser(id); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"deleted": id})
	case http.MethodPut:
		var req state.User
		if err := decodeJSON(r.Body, &req); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		req.ID = id
		user, err := s.store.UpsertUser(req)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, user)
	default:
		writeErr(w, 405, "method not allowed")
	}
}

func (s *Server) settingsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		settings := s.store.Settings()
		writeJSON(w, 200, redactSettings(settings))
	case http.MethodPut:
		var req state.Settings
		if err := decodeJSON(r.Body, &req); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		settings, err := s.store.UpdateSettings(req)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, settings)
	default:
		writeErr(w, 405, "method not allowed")
	}
}

func (s *Server) playersHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	writeJSON(w, 200, map[string]any{"players": s.store.ListPlayerRecords()})
}

func (s *Server) livePlayersHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	// Flatten the map[sourceID][]LivePlayer into a single flat []LivePlayer.
	allBySource := s.store.CurrentPlayers()
	var flat []state.LivePlayer
	for _, players := range allBySource {
		flat = append(flat, players...)
	}
	if flat == nil {
		flat = []state.LivePlayer{}
	}
	writeJSON(w, 200, map[string]any{"live": flat})
}

func (s *Server) wrapperLogsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	writeJSON(w, 200, map[string]any{"logs": s.wrapperLogs.Snapshot(limitParam(r, 500))})
}

func (s *Server) steamcmdLogsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	writeJSON(w, 200, map[string]any{"logs": s.steam.Logs(limitParam(r, 500))})
}

func (s *Server) profileLogsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/logs/profile/"), "/")
	if id == "" {
		writeErr(w, 400, "invalid profile id")
		return
	}
	kind := r.URL.Query().Get("kind")
	if kind == "" {
		kind = "server"
	}
	logs, err := s.fleet.Logs(id, kind, limitParam(r, 500))
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"logs": logs, "kind": kind})
}

func (s *Server) generatePasswordHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	writeJSON(w, 200, map[string]string{"password": generateHex(24)})
}

func (s *Server) generateSessionSecretHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	writeJSON(w, 200, map[string]string{"sessionSecret": generateHex(32)})
}

func (s *Server) downloadConfigHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	name := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/download/config/"), "/")
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "..") {
		writeErr(w, 400, "invalid config file")
		return
	}
	root, _, err := s.resolveConfigRoot(r.URL.Query().Get("profile"))
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	http.ServeFile(w, r, filepath.Join(root, name))
}

func (s *Server) downloadLogsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/download/logs/"), "/")
	if id == "" {
		id = "default"
	}
	profile, ok := s.store.GetProfile(id)
	if !ok {
		writeErr(w, 404, "profile not found")
		return
	}
	http.ServeFile(w, r, filepath.Join(profile.LogRoot, "server.log"))
}

func (s *Server) downloadLogsArchiveHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	profileID := strings.TrimSpace(r.URL.Query().Get("profile"))
	files := map[string]string{}
	if profileID != "" {
		profile, ok := s.store.GetProfile(profileID)
		if !ok {
			writeErr(w, 404, "profile not found")
			return
		}
		if entries, err := os.ReadDir(profile.LogRoot); err == nil {
			for _, entry := range entries {
				if entry.IsDir() {
					continue
				}
				files[filepath.Join(profileID, entry.Name())] = filepath.Join(profile.LogRoot, entry.Name())
			}
		}
	} else {
		for _, profile := range s.store.ListProfiles() {
			if entries, err := os.ReadDir(profile.LogRoot); err == nil {
				for _, entry := range entries {
					if entry.IsDir() {
						continue
					}
					files[filepath.Join(profile.ID, entry.Name())] = filepath.Join(profile.LogRoot, entry.Name())
				}
			}
		}
	}
	if len(files) == 0 {
		writeErr(w, 404, "no log files found")
		return
	}
	sendZip(w, "sandpanel-server-logs.zip", files)
}

func (s *Server) downloadWrapperLogsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	temp := filepath.Join(os.TempDir(), "sandpanel-wrapper.log")
	lines := s.wrapperLogs.Snapshot(5000)
	buf := &bytes.Buffer{}
	for _, line := range lines {
		fmt.Fprintf(buf, "[%s] %s\n", line.Time.Format(time.RFC3339), line.Line)
	}
	if err := os.WriteFile(temp, buf.Bytes(), 0o644); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer os.Remove(temp)
	sendZip(w, "sandpanel-wrapper-logs.zip", map[string]string{"wrapper.log": temp})
}

func (s *Server) wrapperRestartHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	workspace := strings.TrimSpace(os.Getenv("WORKSPACE_ROOT"))
	if workspace == "" {
		writeErr(w, 400, "workspace root is not configured")
		return
	}
	s.auditf("WRAPPER RESTART REQUESTED workspace=%s", workspace)
	writeJSON(w, 200, map[string]string{"status": "restart requested"})
	go func() {
		time.Sleep(500 * time.Millisecond)
		s.runComposeCommand(workspace, "restart", "backend", "frontend")
	}()
}

func (s *Server) wrapperUpdateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	workspace := strings.TrimSpace(os.Getenv("WORKSPACE_ROOT"))
	if workspace == "" {
		writeErr(w, 400, "workspace root is not configured")
		return
	}
	s.auditf("WRAPPER UPDATE REQUESTED workspace=%s", workspace)
	writeJSON(w, 200, map[string]string{"status": "update requested"})
	go func() {
		time.Sleep(500 * time.Millisecond)
		if _, err := os.Stat(filepath.Join(workspace, ".git")); err == nil {
			if err := s.runLoggedCommand("git", "-C", workspace, "pull", "--ff-only"); err != nil {
				return
			}
		} else {
			s.auditf("WRAPPER UPDATE git metadata missing, skipping pull and rebuilding from mounted workspace")
		}
		if err := s.runComposeCommand(workspace, "build", "backend", "frontend"); err != nil {
			return
		}
		_ = s.runComposeCommand(workspace, "up", "-d", "backend", "frontend")
	}()
}

func (s *Server) kickHandler(w http.ResponseWriter, r *http.Request) {
	s.moderationHandler(w, r, "kick")
}

func (s *Server) banHandler(w http.ResponseWriter, r *http.Request) {
	s.moderationHandler(w, r, "banid")
}

func (s *Server) unbanHandler(w http.ResponseWriter, r *http.Request) {
	s.moderationHandler(w, r, "unban")
}

func (s *Server) moderationHandler(w http.ResponseWriter, r *http.Request, verb string) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	var req struct {
		TargetType string `json:"targetType"`
		TargetID   string `json:"targetId"`
		SteamID    string `json:"steamId"`
		Reason     string `json:"reason"`
	}
	if err := decodeJSON(r.Body, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.SteamID == "" {
		writeErr(w, 400, "steamId is required")
		return
	}
	if !isValidSteamID(req.SteamID) {
		writeErr(w, 400, "invalid steamId format")
		return
	}
	reason := sanitizeRCONString(strings.TrimSpace(req.Reason))
	if reason == "" {
		reason = "Managed by SandPanel"
	}
	command := ""
	if verb == "banid" {
		command = fmt.Sprintf(`banid %s 0 "%s"`, req.SteamID, reason)
	} else if verb == "unban" {
		command = fmt.Sprintf(`unban %s`, req.SteamID)
	} else {
		command = fmt.Sprintf(`kick %s "%s"`, req.SteamID, reason)
	}
	switch req.TargetType {
	case "monitor":
		resp, err := s.execTargetCommand("monitor", req.TargetID, "", 0, "", command)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"response": resp})
	default:
		resp, err := s.execTargetCommand(req.TargetType, req.TargetID, "", 0, "", command)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		if verb == "banid" {
			_ = s.appendBan(req.SteamID, reason)
			s.propagateLocalModeration(command)
		}
		if verb == "unban" {
			_ = s.removeBan(req.SteamID)
			s.propagateLocalModeration(command)
		}
		writeJSON(w, 200, map[string]string{"response": resp})
	}
}

func (s *Server) execRCON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	var req struct {
		Command string `json:"command"`
	}
	if err := decodeJSON(r.Body, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if strings.TrimSpace(req.Command) == "" {
		writeErr(w, 400, "command is required")
		return
	}
	resp, err := s.fleet.ExecRCON("default", req.Command)
	if err != nil {
		client := s.rconFactory()
		defer client.Close()
		resp, err = client.Exec(req.Command)
	}
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"response": resp})
}

func (s *Server) execTargetCommand(targetType, targetID, host string, port int, password, command string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(targetType)) {
	case "", "local":
		resp, err := s.fleet.ExecRCON("default", command)
		if err == nil {
			return resp, nil
		}
		client := s.rconFactory()
		defer client.Close()
		return client.Exec(command)
	case "profile":
		if strings.TrimSpace(targetID) == "" {
			targetID = "default"
		}
		return s.fleet.ExecRCON(targetID, command)
	case "monitor":
		cfg, ok := s.store.GetMonitor(targetID)
		if !ok {
			return "", fmt.Errorf("monitor not found")
		}
		return monitor.Exec(cfg, command)
	case "direct":
		if strings.TrimSpace(host) == "" || port <= 0 || strings.TrimSpace(password) == "" {
			return "", fmt.Errorf("host, port and password are required for direct targets")
		}
		client := rcon.New(host, port, password, 4*time.Second)
		defer client.Close()
		return client.Exec(command)
	default:
		return "", fmt.Errorf("unknown target type %s", targetType)
	}
}

func (s *Server) commandHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	var req struct {
		Kind       string `json:"kind"`
		Command    string `json:"command"`
		TargetType string `json:"targetType"`
		TargetID   string `json:"targetId"`
		Host       string `json:"host"`
		Port       int    `json:"port"`
		Password   string `json:"password"`
	}
	if err := decodeJSON(r.Body, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	command := strings.TrimSpace(req.Command)
	if command == "" {
		writeErr(w, 400, "command is required")
		return
	}
	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	if kind == "" {
		kind = "rcon"
	}
	if kind == "chat" {
		command = "say " + command
	}
	resp, err := s.execTargetCommand(req.TargetType, req.TargetID, req.Host, req.Port, req.Password, command)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"response": resp, "kind": kind})
}

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func (s *Server) logsWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.auditf("WS UPGRADE FAIL remote=%s err=%v conn=%q upgrade=%q version=%q", r.RemoteAddr, err, r.Header.Get("Connection"), r.Header.Get("Upgrade"), r.Header.Get("Sec-WebSocket-Version"))
		return
	}
	s.hub.Add(conn)
	defer s.hub.Remove(conn)
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

func (s *Server) socketIOHandler(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(r.URL.Query().Get("transport")) != "websocket" {
		writeErr(w, 400, "websocket transport required")
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.auditf("SOCKET.IO UPGRADE FAIL remote=%s err=%v", r.RemoteAddr, err)
		return
	}
	defer conn.Close()

	writeMu := &sync.Mutex{}
	writePacket := func(payload string) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(websocket.TextMessage, []byte(payload))
	}

	openPayload, _ := json.Marshal(map[string]any{
		"sid":          generateHex(10),
		"upgrades":     []string{},
		"pingInterval": 25000,
		"pingTimeout":  20000,
		"maxPayload":   1000000,
	})
	if err := writePacket("0" + string(openPayload)); err != nil {
		return
	}
	_ = writePacket("40")

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if err := writePacket("2"); err != nil {
					return
				}
			}
		}
	}()

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			close(done)
			return
		}
		msg := strings.TrimSpace(string(raw))
		switch {
		case msg == "2":
			_ = writePacket("3")
		case msg == "40":
			_ = writePacket("40")
		case strings.HasPrefix(msg, "40"):
			_ = writePacket("40")
		}
	}
}

func (s *Server) observeProfilePlayers(id, sourceName string) ([]state.LivePlayer, []rcon.Player, error) {
	players, bots, err := s.fleet.ListPlayers(id)
	if err != nil {
		return nil, nil, err
	}
	obs := make([]state.PlayerObservation, 0, len(players)+len(bots))
	for _, player := range players {
		obs = append(obs, state.PlayerObservation{
			SteamID:    player.SteamID,
			Name:       player.Name,
			Score:      player.Score,
			IP:         player.IP,
			PlatformID: player.PlatformID,
			IsBot:      false,
		})
	}
	for _, bot := range bots {
		obs = append(obs, state.PlayerObservation{
			SteamID:    bot.SteamID,
			Name:       bot.Name,
			Score:      bot.Score,
			IP:         bot.IP,
			PlatformID: bot.PlatformID,
			IsBot:      true,
		})
	}
	events, live, err := s.store.ObservePlayers(id, sourceName, obs)
	if err == nil {
		go s.SendJoinLeaveMessages(id, events, len(live))
	}
	return live, bots, err
}

// SendJoinLeaveMessages sends welcome/goodbye RCON messages for join/leave events.
func (s *Server) SendJoinLeaveMessages(profileID string, events []state.PlayerEvent, playerCount int) {
	profile, ok := s.store.GetProfile(profileID)
	if !ok {
		return
	}
	if profile.WelcomeMessage == "" && profile.WelcomeMessageAdmin == "" &&
		profile.GoodbyeMessage == "" && profile.GoodbyeMessageAdmin == "" {
		return
	}
	admins := loadAdminSteamIDs(profile.ConfigRoot)
	for _, event := range events {
		if event.Player.SteamID == "" {
			continue
		}
		isAdmin := admins[event.Player.SteamID]
		var template string
		switch event.Type {
		case "join":
			if isAdmin && profile.WelcomeMessageAdmin != "" {
				template = profile.WelcomeMessageAdmin
			} else if profile.WelcomeMessage != "" {
				template = profile.WelcomeMessage
			}
		case "leave":
			if isAdmin && profile.GoodbyeMessageAdmin != "" {
				template = profile.GoodbyeMessageAdmin
			} else if profile.GoodbyeMessage != "" {
				template = profile.GoodbyeMessage
			}
		}
		if template == "" {
			continue
		}
		msg := expandMessageTemplate(template, event.Player.Name, event.Player.SteamID, profile.Name, playerCount)
		_, _ = s.fleet.ExecRCON(profileID, "say "+msg)
	}
}

// expandMessageTemplate replaces template variables in welcome/goodbye messages.
func expandMessageTemplate(template, playerName, steamID, serverName string, playerCount int) string {
	r := strings.NewReplacer(
		"{player_name}", playerName,
		"{steam_id}", steamID,
		"{server_name}", serverName,
		"{player_count}", fmt.Sprint(playerCount),
	)
	return r.Replace(template)
}

// loadAdminSteamIDs reads the Admins.txt file and returns a set of admin SteamIDs.
func loadAdminSteamIDs(configRoot string) map[string]bool {
	admins := map[string]bool{}
	path := filepath.Join(configRoot, "Admins.txt")
	b, err := os.ReadFile(path)
	if err != nil {
		return admins
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}
		admins[line] = true
	}
	return admins
}

// syncRCONPasswordToGameIni updates the Password line under the [Rcon] section in Game.ini.
func syncRCONPasswordToGameIni(configRoot, password string) {
	gameIniPath := filepath.Join(configRoot, "Game.ini")
	raw, err := os.ReadFile(gameIniPath)
	if err != nil {
		return // Game.ini doesn't exist, nothing to sync
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	inRconSection := false
	passwordUpdated := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			inRconSection = strings.EqualFold(trimmed, "[Rcon]")
			continue
		}
		if inRconSection && strings.HasPrefix(trimmed, "Password") {
			// Preserve spacing/formatting
			eqIdx := strings.Index(line, "=")
			if eqIdx >= 0 {
				prefix := line[:eqIdx+1]
				// Check if there's a space after =
				rest := line[eqIdx+1:]
				if len(rest) > 0 && rest[0] == ' ' {
					lines[i] = prefix + " " + password
				} else {
					lines[i] = prefix + " " + password
				}
				passwordUpdated = true
			}
		}
	}
	if !passwordUpdated {
		return // No [Rcon] Password= line found, nothing to update
	}
	content := strings.Join(lines, "\n")
	_ = os.WriteFile(gameIniPath, []byte(content), 0o644)
}

// syncMonitorPasswords auto-updates monitors that share the same RCON port as the profile.
func (s *Server) syncMonitorPasswords(profile state.Profile) {
	for _, mon := range s.store.ListMonitors() {
		if mon.RCONPort == profile.RCONPort && mon.RCONPassword != profile.RCONPassword {
			mon.RCONPassword = profile.RCONPassword
			saved, err := s.store.UpsertMonitor(mon)
			if err == nil {
				s.monitors.Upsert(saved)
			}
		}
	}
}

func (s *Server) propagateLocalModeration(command string) {
	for _, profile := range s.store.ListProfiles() {
		if profile.ID == "default" {
			continue
		}
		_, _ = s.fleet.ExecRCON(profile.ID, command)
	}
}

func (s *Server) appendBan(steamID, reason string) error {
	type banEntry struct {
		PlayerID string `json:"playerId"`
		Reason   string `json:"reason"`
		Until    string `json:"until"`
	}
	paths := s.banPaths()
	for _, path := range paths {
		entries := []banEntry{}
		if raw, err := os.ReadFile(path); err == nil && bytes.TrimSpace(raw) != nil && len(bytes.TrimSpace(raw)) > 0 {
			_ = json.Unmarshal(raw, &entries)
		}
		found := false
		for i := range entries {
			if entries[i].PlayerID == steamID {
				entries[i].Reason = reason
				found = true
			}
		}
		if !found {
			entries = append(entries, banEntry{PlayerID: steamID, Reason: reason, Until: "0"})
		}
		body, _ := json.MarshalIndent(entries, "", "  ")
		if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) removeBan(steamID string) error {
	type banEntry struct {
		PlayerID string `json:"playerId"`
	}
	for _, path := range s.banPaths() {
		entries := []banEntry{}
		if raw, err := os.ReadFile(path); err == nil && len(bytes.TrimSpace(raw)) > 0 {
			_ = json.Unmarshal(raw, &entries)
		}
		filtered := make([]banEntry, 0, len(entries))
		for _, entry := range entries {
			if entry.PlayerID != steamID {
				filtered = append(filtered, entry)
			}
		}
		body, _ := json.MarshalIndent(filtered, "", "  ")
		if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) banPaths() []string {
	paths := []string{filepath.Join(s.configRoot, "Bans.json")}
	for _, profile := range s.store.ListProfiles() {
		if profile.ConfigRoot == "" {
			continue
		}
		paths = append(paths, filepath.Join(profile.ConfigRoot, "Bans.json"))
	}
	sort.Strings(paths)
	uniq := []string{}
	last := ""
	for _, path := range paths {
		if path == last {
			continue
		}
		last = path
		uniq = append(uniq, path)
	}
	return uniq
}

func (s *Server) resolveConfigRoot(profileID string) (string, string, error) {
	id := strings.TrimSpace(profileID)
	if id == "" || id == "default" {
		return s.configRoot, "default", nil
	}
	profile, ok := s.store.GetProfile(id)
	if !ok {
		return "", "", fmt.Errorf("profile not found")
	}
	if strings.TrimSpace(profile.ConfigRoot) == "" {
		return "", "", fmt.Errorf("profile config root is not set")
	}
	return profile.ConfigRoot, profile.ID, nil
}

func limitParam(r *http.Request, fallback int) int {
	if fallback <= 0 {
		fallback = 500
	}
	value := r.URL.Query().Get("limit")
	if value == "" {
		return fallback
	}
	n := 0
	fmt.Sscanf(value, "%d", &n)
	if n <= 0 {
		return fallback
	}
	return n
}

func sendZip(w http.ResponseWriter, name string, files map[string]string) {
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	zw := zip.NewWriter(w)
	keys := make([]string, 0, len(files))
	for key := range files {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, entryName := range keys {
		path := files[entryName]
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		fw, err := zw.Create(entryName)
		if err != nil {
			continue
		}
		_, _ = fw.Write(body)
	}
	_ = zw.Close()
}

func (s *Server) runComposeCommand(workspace string, args ...string) error {
	fullArgs := append([]string{"-p", "sandpanel", "-f", filepath.Join(workspace, "docker-compose.yml")}, args...)
	return s.runLoggedCommand("docker-compose", fullArgs...)
}

func (s *Server) runLoggedCommand(name string, args ...string) error {
	s.auditf("EXEC %s %s", name, strings.Join(args, " "))
	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(output))
	if text != "" {
		for _, line := range strings.Split(text, "\n") {
			s.auditf("%s", line)
		}
	}
	if err != nil {
		s.auditf("EXEC ERROR %v", err)
		return err
	}
	return nil
}

func decodeJSON(r io.Reader, out any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	return nil
}

type rateLimiter struct {
	mu       sync.Mutex
	max      int
	windowS  int64
	counters map[string]*rateEntry
}

type rateEntry struct {
	count   int
	resetAt int64
}

func newRateLimiter(max int, windowSeconds int) *rateLimiter {
	return &rateLimiter{max: max, windowS: int64(windowSeconds), counters: map[string]*rateEntry{}}
}

func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now().Unix()
	entry, ok := rl.counters[key]
	if !ok || entry.resetAt <= now {
		rl.counters[key] = &rateEntry{count: 1, resetAt: now + rl.windowS}
		return true
	}
	if entry.count >= rl.max {
		return false
	}
	entry.count++
	return true
}

func remoteIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if parts := strings.SplitN(xff, ",", 2); len(parts) > 0 {
			ip := strings.TrimSpace(parts[0])
			if ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, obj any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(obj)
}

func generateHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("generated-%d", n)
	}
	return hex.EncodeToString(buf)
}

func redactSettings(s state.Settings) map[string]any {
	mask := func(v string) string {
		if v == "" {
			return ""
		}
		return "••••••••"
	}
	return map[string]any{
		"automaticUpdates":      s.AutomaticUpdates,
		"updateIntervalMinutes": s.UpdateIntervalMinutes,
		"sessionSecret":         mask(s.SessionSecret),
		"steamApiKey":           mask(s.SteamAPIKey),
		"steamUsername":         s.SteamUsername,
		"steamPassword":         mask(s.SteamPassword),
		"gameStatsToken":        mask(s.GameStatsToken),
		"steamServerToken":      mask(s.SteamServerToken),
	}
}

var steamIDPattern = regexp.MustCompile(`^[0-9]{1,20}$`)

func isValidSteamID(id string) bool {
	return steamIDPattern.MatchString(strings.TrimSpace(id))
}

func sanitizeRCONString(s string) string {
	s = strings.ReplaceAll(s, `"`, `'`)
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

var allowedSteamCMDPrefixes = []string{
	"+login", "+quit", "+force_install_dir", "+app_update", "+app_info_print",
	"+app_info_update", "+app_status", "+set_steam_guard_code", "+app_set_config",
	"validate",
}

func isAllowedSteamCMDArg(arg string) bool {
	trimmed := strings.TrimSpace(arg)
	if trimmed == "" {
		return false
	}
	for _, prefix := range allowedSteamCMDPrefixes {
		if strings.EqualFold(trimmed, prefix) || strings.HasPrefix(strings.ToLower(trimmed), strings.ToLower(prefix)+" ") {
			return true
		}
	}
	if !strings.HasPrefix(trimmed, "+") && !strings.HasPrefix(trimmed, "-") {
		return true
	}
	return false
}

// ensureMutatorModsEnabled reads mutator_mods.json, finds which mod IDs
// provide the given mutators, and enables those mods in the specified config root.
// This is called when saving a profile with mutators so tat the required mods
// are automatically subscribed/enabled.
func (s *Server) ensureMutatorModsEnabled(configRoot string, mutators []string) error {
	filePath := filepath.Join(s.configRoot, "mutator_mods.json")
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no mutator_mods.json, nothing to auto-enable
		}
		return err
	}
	var entries []struct {
		ModID    string   `json:"modId"`
		ModName  string   `json:"modName"`
		Mutators []string `json:"mutators"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return err
	}

	// Build mutator name → mod ID lookup.
	mutatorToMod := map[string]string{}
	for _, entry := range entries {
		for _, m := range entry.Mutators {
			mutatorToMod[strings.ToLower(m)] = entry.ModID
		}
	}

	// Find required mod IDs.
	requiredModIDs := map[string]bool{}
	for _, mut := range mutators {
		if modID, ok := mutatorToMod[strings.ToLower(strings.TrimSpace(mut))]; ok {
			requiredModIDs[modID] = true
		}
	}

	if len(requiredModIDs) == 0 {
		return nil
	}

	// Use the correct config root's mod manager.
	root := configRoot
	if root == "" {
		root = s.configRoot
	}
	manager := mods.New(root)
	subs, err := manager.LoadSubscriptions()
	if err != nil {
		return err
	}

	// Check which required mods are not yet enabled.
	changed := false
	existingIDs := map[string]int{}
	for i, sub := range subs {
		existingIDs[sub.ID] = i
	}
	for modID := range requiredModIDs {
		if idx, ok := existingIDs[modID]; ok {
			if !subs[idx].Enabled {
				subs[idx].Enabled = true
				changed = true
			}
		} else {
			subs = append(subs, mods.Subscription{ID: modID, Enabled: true})
			changed = true
		}
	}

	if changed {
		if err := manager.SaveSubscriptions(subs); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) mutatorModsHandler(w http.ResponseWriter, r *http.Request) {
	filePath := filepath.Join(s.configRoot, "mutator_mods.json")
	switch r.Method {
	case http.MethodGet:
		data, err := os.ReadFile(filePath)
		if err != nil {
			if os.IsNotExist(err) {
				writeJSON(w, 200, []any{})
				return
			}
			writeErr(w, 500, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(data)
	case http.MethodPut:
		data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		var entries []json.RawMessage
		if err := json.Unmarshal(data, &entries); err != nil {
			writeErr(w, 400, "invalid JSON array: "+err.Error())
			return
		}
		if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if err := os.WriteFile(filePath, data, 0o644); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(data)
	default:
		writeErr(w, 405, "method not allowed")
	}
}

// safePath joins root and name, cleans the result, and verifies it stays
// within root. This prevents directory-traversal attacks where a manipulated
// name could escape the intended directory.
func safePath(root, name string) (string, error) {
	cleanRoot := filepath.Clean(root)
	joined := filepath.Join(cleanRoot, name)
	cleaned := filepath.Clean(joined)
	if !strings.HasPrefix(cleaned, cleanRoot+string(filepath.Separator)) && cleaned != cleanRoot {
		return "", fmt.Errorf("path escapes root directory")
	}
	return cleaned, nil
}
