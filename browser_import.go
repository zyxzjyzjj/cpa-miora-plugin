package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The plugin runs inside the operator's CLIProxyAPI process, which on a desktop
// deployment shares the machine with the browser the user already signed in with.
// Reading the Chromium Local Storage leveldb lets the panel import that session
// with one click instead of asking for a token nobody remembers. No password,
// cookie jar or browser profile credential store is touched.

// browserRoot describes one Chromium-family browser installation.
type browserRoot struct {
	Name string
	Dir  string
}

// browserCandidate is one discovered Miora session.
type browserCandidate struct {
	Browser      string `json:"browser"`
	Profile      string `json:"profile"`
	AuthToken    string `json:"-"`
	RefreshToken string `json:"-"`
	Email        string `json:"email"`
	UserID       string `json:"user_id"`
	Plan         string `json:"plan"`
	ExpiresAt    string `json:"expires_at"`
	Valid        bool   `json:"valid"`
	Message      string `json:"message"`
}

var (
	jwtPattern     = regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{16,}`)
	refreshPattern = regexp.MustCompile(`(?:def50200|def5020)[0-9a-fA-F]{100,900}`)
	emailPattern   = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	userIDPattern  = regexp.MustCompile(`(?i)"id"\s*:\s*"?(\d{6,25})"?`)
)

func browserRoots() []browserRoot {
	localAppData := os.Getenv("LOCALAPPDATA")
	appData := os.Getenv("APPDATA")
	home, _ := os.UserHomeDir()
	roots := []browserRoot{}
	add := func(name, dir string) {
		if strings.TrimSpace(dir) == "" {
			return
		}
		if info, errStat := os.Stat(dir); errStat == nil && info.IsDir() {
			roots = append(roots, browserRoot{Name: name, Dir: dir})
		}
	}
	if localAppData != "" {
		add("edge", filepath.Join(localAppData, "Microsoft", "Edge", "User Data"))
		add("chrome", filepath.Join(localAppData, "Google", "Chrome", "User Data"))
		add("chrome-beta", filepath.Join(localAppData, "Google", "Chrome Beta", "User Data"))
		add("brave", filepath.Join(localAppData, "BraveSoftware", "Brave-Browser", "User Data"))
		add("chromium", filepath.Join(localAppData, "Chromium", "User Data"))
		add("vivaldi", filepath.Join(localAppData, "Vivaldi", "User Data"))
		add("yandex", filepath.Join(localAppData, "Yandex", "YandexBrowser", "User Data"))
		add("360", filepath.Join(localAppData, "360Chrome", "Chrome", "User Data"))
		add("qq", filepath.Join(localAppData, "Tencent", "QQBrowser", "User Data"))
	}
	if appData != "" {
		add("opera", filepath.Join(appData, "Opera Software", "Opera Stable"))
		add("opera-gx", filepath.Join(appData, "Opera Software", "Opera GX Stable"))
	}
	if home != "" {
		add("chromium", filepath.Join(home, ".config", "chromium", "Default"))
		add("chrome", filepath.Join(home, "Library", "Application Support", "Google", "Chrome"))
	}
	return roots
}

// browserProfileDirs lists the profile directories of one browser installation.
func browserProfileDirs(root browserRoot) []browserProfile {
	entries, errRead := os.ReadDir(root.Dir)
	if errRead != nil {
		return nil
	}
	profiles := make([]browserProfile, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name != "Default" && !strings.HasPrefix(name, "Profile ") && !strings.EqualFold(name, "Guest Profile") {
			continue
		}
		dir := filepath.Join(root.Dir, name)
		if _, errStat := os.Stat(filepath.Join(dir, "Local Storage", "leveldb")); errStat == nil {
			profiles = append(profiles, browserProfile{Browser: root.Name, Name: name, LevelDB: filepath.Join(dir, "Local Storage", "leveldb")})
		}
	}
	// Opera keeps Local Storage directly in the profile directory.
	if direct := filepath.Join(root.Dir, "Local Storage", "leveldb"); len(profiles) == 0 {
		if _, errStat := os.Stat(direct); errStat == nil {
			profiles = append(profiles, browserProfile{Browser: root.Name, Name: "Default", LevelDB: direct})
		}
	}
	return profiles
}

type browserProfile struct {
	Browser string
	Name    string
	LevelDB string
}

// scanBrowserSessions discovers Miora sessions in local browser profiles and
// validates each one against the upstream, so the panel never offers a dead token.
func scanBrowserSessions(cfg *Config) ([]browserCandidate, error) {
	profiles := selectedProfiles(cfg)
	candidates := make([]browserCandidate, 0, 8)
	seen := map[string]struct{}{}
	for _, profile := range profiles {
		rawCreds := extractMioraSessions(profile.LevelDB)
		for _, raw := range rawCreds {
			key := raw.AuthToken
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			candidate := browserCandidate{
				Browser:      profile.Browser,
				Profile:      profile.Name,
				AuthToken:    raw.AuthToken,
				RefreshToken: raw.RefreshToken,
				Email:        raw.Email,
				UserID:       raw.UserID,
				Plan:         raw.Plan,
			}
			if expiry, ok := jwtExpiry(raw.AuthToken); ok {
				candidate.ExpiresAt = expiry.UTC().Format(time.RFC3339)
			}
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("没有在本机 Edge/Chrome 的 Local Storage 里找到 miora.design 登录态；请先在浏览器登录 miora.design，或在下方手动粘贴 authToken")
	}

	// Validate at most six candidates: enough to cover several profiles without
	// hammering the upstream from a single panel click.
	limit := len(candidates)
	if limit > 6 {
		limit = 6
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for index := 0; index < limit; index++ {
		cred := &credential{AuthToken: candidates[index].AuthToken, RefreshToken: candidates[index].RefreshToken}
		imported, errImport := importCredential(ctx, cfg, cred)
		if errImport != nil {
			candidates[index].Message = errImport.Error()
			continue
		}
		candidates[index].Valid = true
		candidates[index].Email = imported.Email
		candidates[index].UserID = imported.UserID
		candidates[index].Plan = imported.Plan
		candidates[index].Message = "登录态有效"
	}
	for index := limit; index < len(candidates); index++ {
		candidates[index].Message = "未校验（候选过多，以列表顺序为准）"
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Valid != candidates[j].Valid {
			return candidates[i].Valid
		}
		return candidates[i].Email < candidates[j].Email
	})
	return candidates, nil
}

func selectedProfiles(cfg *Config) []browserProfile {
	roots := browserRoots()
	filter := map[string]struct{}{}
	if cfg != nil {
		for _, item := range cfg.BrowserProfiles {
			filter[strings.ToLower(strings.TrimSpace(item))] = struct{}{}
		}
	}
	profiles := make([]browserProfile, 0, 12)
	for _, root := range roots {
		for _, profile := range browserProfileDirs(root) {
			if len(filter) > 0 {
				if _, ok := filter[strings.ToLower(profile.Browser+":"+profile.Name)]; !ok {
					continue
				}
			}
			profiles = append(profiles, profile)
		}
	}
	return profiles
}

type rawSession struct {
	AuthToken    string
	RefreshToken string
	Email        string
	UserID       string
	Plan         string
}

// extractMioraSessions scans one Local Storage leveldb directory. Values are
// stored inline for ASCII payloads, which is what the Miora token pair is, so a
// bounded byte scan of the .ldb/.log files finds them without a leveldb library.
func extractMioraSessions(levelDBDir string) []rawSession {
	entries, errRead := os.ReadDir(levelDBDir)
	if errRead != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if ext != ".ldb" && ext != ".log" {
			continue
		}
		names = append(names, entry.Name())
	}
	// Newest files first: the freshest session lives in the newest log/table.
	sort.Sort(sort.Reverse(sort.StringSlice(names)))

	sessions := make([]rawSession, 0, 4)
	seen := map[string]struct{}{}
	for _, name := range names {
		path := filepath.Join(levelDBDir, name)
		data, errReadFile := readFileLimited(path, 96<<20)
		if errReadFile != nil || len(data) == 0 {
			continue
		}
		text := string(data)
		if !strings.Contains(text, "miora") {
			continue
		}
		for _, chunk := range mioraChunks(text) {
			session := parseSessionChunk(chunk)
			if session.AuthToken == "" {
				continue
			}
			if _, ok := seen[session.AuthToken]; ok {
				continue
			}
			seen[session.AuthToken] = struct{}{}
			sessions = append(sessions, session)
		}
	}
	return sessions
}

// mioraChunks returns the byte windows that mention miora.design, so unrelated
// origins in the same table cannot contribute tokens.
func mioraChunks(text string) []string {
	const window = 6000
	chunks := make([]string, 0, 4)
	offset := 0
	seen := map[int]struct{}{}
	for {
		index := strings.Index(text[offset:], "miora.design")
		if index < 0 {
			break
		}
		position := offset + index
		start := position - 256
		if start < 0 {
			start = 0
		}
		end := position + window
		if end > len(text) {
			end = len(text)
		}
		if _, ok := seen[start/512]; !ok {
			seen[start/512] = struct{}{}
			chunks = append(chunks, text[start:end])
		}
		offset = position + len("miora.design")
	}
	return chunks
}

func parseSessionChunk(chunk string) rawSession {
	session := rawSession{}
	if index := strings.Index(chunk, "authToken"); index >= 0 {
		window := chunk[index:]
		if end := len(window); end > 2000 {
			window = window[:2000]
		}
		if match := jwtPattern.FindString(window); match != "" {
			session.AuthToken = match
		}
	}
	if session.AuthToken == "" {
		return session
	}
	if index := strings.Index(chunk, "refreshToken"); index >= 0 {
		window := chunk[index:]
		if end := len(window); end > 2000 {
			window = window[:2000]
		}
		if match := refreshPattern.FindString(window); match != "" {
			session.RefreshToken = match
		}
	}
	if match := emailPattern.FindString(chunk); match != "" {
		session.Email = match
	}
	if match := userIDPattern.FindStringSubmatch(chunk); len(match) > 1 {
		session.UserID = match[1]
	}
	if index := strings.Index(chunk, `"plan"`); index >= 0 {
		window := chunk[index:]
		if end := len(window); end > 120 {
			window = window[:120]
		}
		parts := strings.SplitN(window, ":", 2)
		if len(parts) == 2 {
			session.Plan = strings.Trim(strings.TrimSpace(strings.SplitN(parts[1], ",", 2)[0]), `"`)
		}
	}
	return session
}

func readFileLimited(path string, limit int64) ([]byte, error) {
	file, errOpen := os.Open(path)
	if errOpen != nil {
		return nil, errOpen
	}
	defer func() { _ = file.Close() }()
	info, errStat := file.Stat()
	if errStat != nil {
		return nil, errStat
	}
	size := info.Size()
	if size > limit {
		size = limit
	}
	buf := make([]byte, size)
	read, errRead := file.Read(buf)
	if errRead != nil && read == 0 {
		return nil, errRead
	}
	return buf[:read], nil
}
