package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const defaultPort = 5080

type Config struct {
	Root        string   `json:"root"`
	Roots       []string `json:"roots"`
	Port        int      `json:"port"`
	AdminUser   string   `json:"admin_user"`
	LibraryName string   `json:"library_name"`
	// AutoScanMinutes is the periodic incremental rescan interval. nil means
	// the default of 10 minutes; 0 disables automatic scanning.
	AutoScanMinutes *int `json:"auto_scan_minutes,omitempty"`
}

// autoScanInterval resolves the configured period. Automatic scans are
// incremental in effect: unchanged archives keep their cached page counts and
// a scan that finds no changes never replaces the served snapshot.
func autoScanInterval(cfg *Config) time.Duration {
	const def = 10 * time.Minute
	if cfg.AutoScanMinutes == nil {
		return def
	}
	if *cfg.AutoScanMinutes <= 0 {
		return 0
	}
	return time.Duration(*cfg.AutoScanMinutes) * time.Minute
}

func configPath(configDir string) string { return filepath.Join(configDir, "config.json") }
func usersPath(configDir string) string  { return filepath.Join(configDir, "users.json") }

func loadConfig(configDir string) (*Config, error) {
	cfg := &Config{Port: defaultPort, LibraryName: "漫画库", AdminUser: "admin"}
	raw, err := os.ReadFile(configPath(configDir))
	if err == nil {
		if err := json.Unmarshal(raw, cfg); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if cfg.Port == 0 {
		cfg.Port = defaultPort
	}
	if cfg.LibraryName == "" {
		cfg.LibraryName = "漫画库"
	}
	return cfg, nil
}

func saveConfig(configDir string, cfg *Config) error {
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := configPath(configDir) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, configPath(configDir))
}

// resolveRoots returns the directories currently granted by fnOS. Older saved
// roots are intentionally ignored: revoking a directory must take effect.
func resolveRoots(cfg *Config, envPaths string) []string {
	set := map[string]bool{}
	var out []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || set[p] {
			return
		}
		if st, err := os.Stat(p); err != nil || !st.IsDir() {
			return
		}
		set[p] = true
		out = append(out, p)
	}
	for _, p := range strings.Split(envPaths, ":") {
		add(p)
	}
	sort.Strings(out)
	return out
}

type User struct {
	Name  string `json:"name"`
	Hash  string `json:"hash"`
	Admin bool   `json:"admin"`
	// Restricted narrows a Basic Auth reader to the directory subtrees listed
	// in Dirs (paths relative to the library root; a grant covers everything
	// beneath it). When false — also the zero value for older users.json
	// files — the user sees the whole library.
	Restricted bool     `json:"restricted,omitempty"`
	Dirs       []string `json:"dirs,omitempty"`
	// Gateway is true only when fnOS's gateway authenticated the request, in
	// which case Admin reflects the NAS account's admin flag.
	Gateway bool `json:"-"`
}

type UserStore struct {
	Users []User `json:"users"`
}

func loadUsers(configDir string) (*UserStore, error) {
	store := &UserStore{}
	raw, err := os.ReadFile(usersPath(configDir))
	if err == nil {
		if err := json.Unmarshal(raw, store); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return store, nil
}

func (s *UserStore) save(configDir string) error {
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := usersPath(configDir) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, usersPath(configDir))
}

func (s *UserStore) find(name string) *User {
	for i := range s.Users {
		if s.Users[i].Name == name {
			return &s.Users[i]
		}
	}
	return nil
}

func (s *UserStore) remove(name string) bool {
	for i := range s.Users {
		if s.Users[i].Name == name {
			s.Users = append(s.Users[:i], s.Users[i+1:]...)
			return true
		}
	}
	return false
}
