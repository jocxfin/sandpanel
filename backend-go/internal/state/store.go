package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

var validRoles = map[string]int{
	"user":      1,
	"moderator": 2,
	"admin":     3,
	"host":      4,
}

type Profile struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	ConfigRoot          string   `json:"configRoot"`
	LogRoot             string   `json:"logRoot"`
	GamePort            int      `json:"gamePort"`
	QueryPort           int      `json:"queryPort"`
	RCONPort            int      `json:"rconPort"`
	RCONPassword        string   `json:"rconPassword"`
	DefaultMap          string   `json:"defaultMap"`
	Scenario            string   `json:"scenario"`
	Mutators            []string `json:"mutators"`
	AdditionalArgs      []string `json:"additionalArgs"`
	Password            string   `json:"password,omitempty"`
	DefaultLighting     string   `json:"defaultLighting,omitempty"`
	WelcomeMessage      string   `json:"welcomeMessage,omitempty"`
	WelcomeMessageAdmin string   `json:"welcomeMessageAdmin,omitempty"`
	GoodbyeMessage      string   `json:"goodbyeMessage,omitempty"`
	GoodbyeMessageAdmin string   `json:"goodbyeMessageAdmin,omitempty"`
}

type MonitorConfig struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Host         string `json:"host"`
	QueryPort    int    `json:"queryPort"`
	RCONPort     int    `json:"rconPort"`
	RCONPassword string `json:"rconPassword"`
}

type User struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Role               string `json:"role"`
	PasswordHash       string `json:"passwordHash,omitempty"`
	Password           string `json:"password,omitempty"`
	MustChangePassword bool   `json:"mustChangePassword"`
}

type PublicUser struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Role               string `json:"role"`
	MustChangePassword bool   `json:"mustChangePassword"`
}

type Settings struct {
	AutomaticUpdates      bool   `json:"automaticUpdates"`
	UpdateIntervalMinutes int    `json:"updateIntervalMinutes"`
	SessionSecret         string `json:"sessionSecret"`
	SteamAPIKey           string `json:"steamApiKey"`
	SteamUsername         string `json:"steamUsername"`
	SteamPassword         string `json:"steamPassword"`
	GameStatsToken        string `json:"gameStatsToken"`
	SteamServerToken      string `json:"steamServerToken"`
}

type PlayerObservation struct {
	SteamID    string `json:"steamId"`
	Name       string `json:"name"`
	Score      int    `json:"score"`
	IP         string `json:"ip"`
	PlatformID string `json:"platformId,omitempty"`
	IsBot      bool   `json:"isBot"`
}

type PlayerRecord struct {
	SteamID                string    `json:"steamId"`
	Name                   string    `json:"name"`
	KnownIPs               []string  `json:"knownIps"`
	FirstSeenAt            time.Time `json:"firstSeenAt,omitempty"`
	LastSeenAt             time.Time `json:"lastSeenAt,omitempty"`
	LastServer             string    `json:"lastServer,omitempty"`
	LastScore              int       `json:"lastScore"`
	HighScore              int       `json:"highScore"`
	TotalScore             int       `json:"totalScore"`
	LastDurationSeconds    int       `json:"lastDurationSeconds"`
	TotalDurationSeconds   int       `json:"totalDurationSeconds"`
	LongestDurationSeconds int       `json:"longestDurationSeconds"`
	TotalKills             int       `json:"totalKills"`
	TotalDeaths            int       `json:"totalDeaths"`
	TotalObjectives        int       `json:"totalObjectives"`
}

type LivePlayer struct {
	PlayerRecord
	SourceID   string    `json:"sourceId"`
	SourceName string    `json:"sourceName"`
	JoinedAt   time.Time `json:"joinedAt"`
	CurrentIP  string    `json:"currentIp,omitempty"`
	IsBot      bool      `json:"isBot"`
}

type PlayerEvent struct {
	Type       string       `json:"type"`
	SourceID   string       `json:"sourceId"`
	SourceName string       `json:"sourceName"`
	Player     PlayerRecord `json:"player"`
	At         time.Time    `json:"at"`
}


type File struct {
	Profiles map[string]Profile       `json:"profiles"`
	Monitors map[string]MonitorConfig `json:"monitors"`
	Users    map[string]User          `json:"users"`
	Settings Settings                 `json:"settings"`
	Players  map[string]PlayerRecord  `json:"players"`
}

type liveSession struct {
	record    PlayerRecord
	ip        string
	isBot     bool
	start     time.Time
	lastScore int
}

type Store struct {
	mu   sync.Mutex
	path string
	data File
	live map[string]map[string]liveSession
}

func New(path string, defaultProfile Profile) (*Store, error) {
	s := &Store{
		path: path,
		live: map[string]map[string]liveSession{},
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err == nil {
		if err := s.load(); err != nil {
			return nil, err
		}
	} else {
		hostPass := generateToken(16)
		hash, err := hashPassword(hostPass)
		if err != nil {
			return nil, err
		}
		s.data = File{
			Profiles: map[string]Profile{defaultProfile.ID: defaultProfile},
			Monitors: map[string]MonitorConfig{},
			Users: map[string]User{
				"host": {
					ID:                 "host",
					Name:               "host",
					Role:               "host",
					PasswordHash:       hash,
					MustChangePassword: true,
				},
			},
			Players: map[string]PlayerRecord{},
			Settings: Settings{
				AutomaticUpdates:      false,
				UpdateIntervalMinutes: 3,
				SessionSecret:         generateToken(32),
				SteamAPIKey:           "",
			},
		}
		if err := s.save(); err != nil {
			return nil, err
		}
	}
	if len(s.data.Profiles) == 0 {
		s.data.Profiles = map[string]Profile{defaultProfile.ID: defaultProfile}
	}
	if s.data.Monitors == nil {
		s.data.Monitors = map[string]MonitorConfig{}
	}
	if s.data.Users == nil {
		s.data.Users = map[string]User{}
	}
	if s.data.Players == nil {
		s.data.Players = map[string]PlayerRecord{}
	}
	if s.data.Settings.UpdateIntervalMinutes <= 0 {
		s.data.Settings.UpdateIntervalMinutes = 3
	}
	if s.data.Settings.SessionSecret == "" {
		s.data.Settings.SessionSecret = generateToken(32)
	}
	if err := s.upgradeUsers(); err != nil {
		return nil, err
	}
	if err := s.save(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	if len(b) == 0 {
		s.data = File{
			Profiles: map[string]Profile{},
			Monitors: map[string]MonitorConfig{},
			Users:    map[string]User{},
			Players:  map[string]PlayerRecord{},
		}
		return nil
	}
	return json.Unmarshal(b, &s.data)
}

func (s *Store) save() error {
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o644)
}

func (s *Store) upgradeUsers() error {
	for id, user := range s.data.Users {
		if user.ID == "" {
			user.ID = id
		}
		user.Role = normalizeRole(user.Role)
		if user.Name == "" {
			user.Name = id
		}
		if user.PasswordHash == "" && user.Password != "" {
			hash, err := hashPassword(user.Password)
			if err != nil {
				return err
			}
			user.PasswordHash = hash
			user.Password = ""
			user.MustChangePassword = true
		}
		s.data.Users[id] = user
	}
	if len(s.data.Users) == 0 {
		hash, err := hashPassword("password")
		if err != nil {
			return err
		}
		s.data.Users["admin"] = User{
			ID:                 "admin",
			Name:               "admin",
			Role:               "host",
			PasswordHash:       hash,
			MustChangePassword: true,
		}
	}
	hasAdmin := false
	for _, user := range s.data.Users {
		if strings.EqualFold(user.Name, "admin") {
			hasAdmin = true
			break
		}
	}
	if !hasAdmin {
		hash, err := hashPassword("password")
		if err != nil {
			return err
		}
		s.data.Users["admin"] = User{
			ID:                 "admin",
			Name:               "admin",
			Role:               "host",
			PasswordHash:       hash,
			MustChangePassword: true,
		}
	}
	return nil
}

func (u User) Public() PublicUser {
	return PublicUser{
		ID:                 u.ID,
		Name:               u.Name,
		Role:               u.Role,
		MustChangePassword: u.MustChangePassword,
	}
}

func (s *Store) ListProfiles() []Profile {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Profile, 0, len(s.data.Profiles))
	for _, p := range s.data.Profiles {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Store) GetProfile(id string) (Profile, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.data.Profiles[id]
	return p, ok
}

func (s *Store) UpsertProfile(p Profile) (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.ID == "" {
		p.ID = generateToken(8)
	}
	s.data.Profiles[p.ID] = p
	return p, s.save()
}

func (s *Store) DeleteProfile(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Profiles, id)
	delete(s.live, id)
	return s.save()
}

func (s *Store) ListMonitors() []MonitorConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]MonitorConfig, 0, len(s.data.Monitors))
	for _, m := range s.data.Monitors {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Store) GetMonitor(id string) (MonitorConfig, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.data.Monitors[id]
	return m, ok
}

func (s *Store) UpsertMonitor(m MonitorConfig) (MonitorConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.ID == "" {
		m.ID = generateToken(8)
	}
	s.data.Monitors[m.ID] = m
	return m, s.save()
}

func (s *Store) DeleteMonitor(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Monitors, id)
	delete(s.live, id)
	return s.save()
}

func (s *Store) ListUsers() []PublicUser {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PublicUser, 0, len(s.data.Users))
	for _, u := range s.data.Users {
		out = append(out, u.Public())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Store) GetUser(id string) (PublicUser, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.data.Users[id]
	return u.Public(), ok
}

func (s *Store) Authenticate(name, password string) (PublicUser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.data.Users {
		if !strings.EqualFold(u.Name, strings.TrimSpace(name)) {
			continue
		}
		if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
			return PublicUser{}, fmt.Errorf("invalid credentials")
		}
		return u.Public(), nil
	}
	return PublicUser{}, fmt.Errorf("invalid credentials")
}

func (s *Store) ChangePassword(id, newPassword string) (PublicUser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.data.Users[id]
	if !ok {
		return PublicUser{}, fmt.Errorf("user not found")
	}
	hash, err := hashPassword(newPassword)
	if err != nil {
		return PublicUser{}, err
	}
	u.PasswordHash = hash
	u.Password = ""
	u.MustChangePassword = false
	s.data.Users[id] = u
	return u.Public(), s.save()
}

func (s *Store) Settings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Settings
}

func (s *Store) UpdateSettings(settings Settings) (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if settings.UpdateIntervalMinutes <= 0 {
		settings.UpdateIntervalMinutes = 3
	}
	if settings.SessionSecret == "" {
		settings.SessionSecret = s.data.Settings.SessionSecret
	}
	s.data.Settings = settings
	return s.data.Settings, s.save()
}

func (s *Store) UpsertUser(u User) (PublicUser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	u.Role = normalizeRole(u.Role)
	if u.Role == "" {
		return PublicUser{}, fmt.Errorf("invalid role")
	}
	u.Name = strings.TrimSpace(u.Name)
	if u.Name == "" {
		return PublicUser{}, fmt.Errorf("name is required")
	}
	for existingID, existing := range s.data.Users {
		if existingID != u.ID && strings.EqualFold(existing.Name, u.Name) {
			return PublicUser{}, fmt.Errorf("user name already exists")
		}
	}
	if u.ID == "" {
		u.ID = generateToken(8)
	}
	prev, existed := s.data.Users[u.ID]
	if existed && prev.Role == "host" && u.Role != "host" && s.hostCountLocked() == 1 {
		return PublicUser{}, fmt.Errorf("cannot demote the last host")
	}
	if u.PasswordHash == "" {
		u.PasswordHash = prev.PasswordHash
	}
	if u.Password != "" {
		hash, err := hashPassword(u.Password)
		if err != nil {
			return PublicUser{}, err
		}
		u.PasswordHash = hash
		u.Password = ""
		u.MustChangePassword = true
	}
	if u.PasswordHash == "" {
		return PublicUser{}, fmt.Errorf("password is required")
	}
	if !existed {
		u.MustChangePassword = true
	}
	s.data.Users[u.ID] = u
	return u.Public(), s.save()
}

func (s *Store) DeleteUser(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.data.Users[id]
	if !ok {
		return fmt.Errorf("user not found")
	}
	if u.Role == "host" && s.hostCountLocked() == 1 {
		return fmt.Errorf("cannot delete the last host")
	}
	delete(s.data.Users, id)
	return s.save()
}

func (s *Store) ObservePlayers(sourceID, sourceName string, players []PlayerObservation) ([]PlayerEvent, []LivePlayer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	if s.live[sourceID] == nil {
		s.live[sourceID] = map[string]liveSession{}
	}
	current := map[string]liveSession{}
	events := []PlayerEvent{}

	for _, player := range players {
		if strings.TrimSpace(player.SteamID) == "" {
			continue
		}
		record := s.data.Players[player.SteamID]
		if record.SteamID == "" {
			record.SteamID = player.SteamID
			record.FirstSeenAt = now
		}
		record.Name = player.Name
		record.LastSeenAt = now
		record.LastServer = sourceName
		record.LastScore = player.Score
		if player.Score > record.HighScore {
			record.HighScore = player.Score
		}
		if player.IP != "" && !contains(record.KnownIPs, player.IP) {
			record.KnownIPs = append(record.KnownIPs, player.IP)
			sort.Strings(record.KnownIPs)
		}
		prev, existed := s.live[sourceID][player.SteamID]
		if !existed {
			// New player — their current score is the baseline
			record.TotalScore += player.Score
			s.data.Players[player.SteamID] = record
			events = append(events, PlayerEvent{Type: "join", SourceID: sourceID, SourceName: sourceName, Player: record, At: now})
			prev = liveSession{record: record, start: now, ip: player.IP, isBot: player.IsBot, lastScore: player.Score}
		} else {
			// Existing player — only add the delta since last poll
			delta := player.Score - prev.lastScore
			if delta > 0 {
				record.TotalScore += delta
			} else if player.Score < prev.lastScore {
				// Score went down (round reset) — add the new score as fresh
				record.TotalScore += player.Score
			}
			s.data.Players[player.SteamID] = record
			prev.record = record
			prev.ip = player.IP
			prev.isBot = player.IsBot
			prev.lastScore = player.Score
		}
		current[player.SteamID] = prev
	}

	for steamID, prev := range s.live[sourceID] {
		if _, ok := current[steamID]; ok {
			continue
		}
		record := s.data.Players[steamID]
		record.LastSeenAt = now
		record.LastServer = sourceName
		duration := int(now.Sub(prev.start).Seconds())
		record.LastDurationSeconds = duration
		record.TotalDurationSeconds += duration
		if duration > record.LongestDurationSeconds {
			record.LongestDurationSeconds = duration
		}
		s.data.Players[steamID] = record
		events = append(events, PlayerEvent{Type: "leave", SourceID: sourceID, SourceName: sourceName, Player: record, At: now})
	}

	s.live[sourceID] = current
	livePlayers := make([]LivePlayer, 0, len(current))
	for _, item := range current {
		livePlayers = append(livePlayers, LivePlayer{
			PlayerRecord: item.record,
			SourceID:     sourceID,
			SourceName:   sourceName,
			JoinedAt:     item.start,
			CurrentIP:    item.ip,
			IsBot:        item.isBot,
		})
	}
	sort.Slice(livePlayers, func(i, j int) bool { return livePlayers[i].Name < livePlayers[j].Name })
	return events, livePlayers, s.save()
}

func (s *Store) ListPlayerRecords() []PlayerRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PlayerRecord, 0, len(s.data.Players))
	for _, p := range s.data.Players {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastSeenAt.Equal(out[j].LastSeenAt) {
			return out[i].Name < out[j].Name
		}
		return out[i].LastSeenAt.After(out[j].LastSeenAt)
	})
	return out
}

func (s *Store) CurrentPlayers() map[string][]LivePlayer {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]LivePlayer, len(s.live))
	for sourceID, sourcePlayers := range s.live {
		list := make([]LivePlayer, 0, len(sourcePlayers))
		for _, item := range sourcePlayers {
			list = append(list, LivePlayer{
				PlayerRecord: item.record,
				SourceID:     sourceID,
				SourceName:   item.record.LastServer,
				JoinedAt:     item.start,
				CurrentIP:    item.ip,
				IsBot:        item.isBot,
			})
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		out[sourceID] = list
	}
	return out
}

func (s *Store) LastPlayerJoinedAt() *time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	var latest *time.Time
	for _, sourcePlayers := range s.live {
		for _, item := range sourcePlayers {
			t := item.start
			if latest == nil || t.After(*latest) {
				latest = &t
			}
		}
	}
	return latest
}


func (s *Store) hostCountLocked() int {
	count := 0
	for _, u := range s.data.Users {
		if u.Role == "host" {
			count++
		}
	}
	return count
}

func normalizeRole(role string) string {
	role = strings.ToLower(strings.TrimSpace(role))
	if _, ok := validRoles[role]; ok {
		return role
	}
	return ""
}

// RoleLevel returns the privilege level for a role (higher = more privileged).
// Returns 0 for unknown roles.
func RoleLevel(role string) int {
	return validRoles[strings.ToLower(strings.TrimSpace(role))]
}

// UpdateLogStats is a stub for the log watcher integration.
// TODO: i will work on this later once log-based stats parsing is reliable
func (s *Store) UpdateLogStats(stats map[string]*LogPlayerStats) {}

func contains(items []string, needle string) bool {
	for _, item := range items {
		if item == needle {
			return true
		}
	}
	return false
}

func hashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func generateToken(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("id-%d", n)
	}
	return hex.EncodeToString(buf)
}
