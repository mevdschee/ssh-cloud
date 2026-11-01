// Package config stores connection profiles and settings in
// %APPDATA%\SSH-Cloud\config.json (the user config dir on other systems).
package config

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/mevdschee/ssh-cloud/internal/remote"
)

// Profile is one saved connection.
type Profile struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     int    `json:"port,omitempty"`
	User     string `json:"user"`
	Root     string `json:"root,omitempty"`
	KeyFile  string `json:"keyFile,omitempty"`
	UseAgent bool   `json:"useAgent"`
}

// Config is the whole settings file.
type Config struct {
	Profiles       []Profile `json:"profiles"`
	LastProfile    string    `json:"lastProfile,omitempty"`
	DownloadFolder string    `json:"downloadFolder,omitempty"`
}

// Dir returns the SSH-Cloud settings directory.
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "SSH-Cloud"), nil
}

// Load reads the config file from dir. A missing file is an empty config.
func Load(dir string) (*Config, error) {
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Save writes the config file atomically.
func (c *Config) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "config.json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "config.json"))
}

// Profile returns the profile with the given name.
func (c *Config) Profile(name string) (Profile, bool) {
	for _, p := range c.Profiles {
		if p.Name == name {
			return p, true
		}
	}
	return Profile{}, false
}

// Put adds or replaces a profile by name and makes it the last used one.
func (c *Config) Put(p Profile) {
	for i := range c.Profiles {
		if c.Profiles[i].Name == p.Name {
			c.Profiles[i] = p
			c.LastProfile = p.Name
			return
		}
	}
	c.Profiles = append(c.Profiles, p)
	c.LastProfile = p.Name
}

// DisplayName is the name shown for a profile: its name, or user@host.
func (p Profile) DisplayName() string {
	if p.Name != "" {
		return p.Name
	}
	return p.User + "@" + p.Host
}

// RemoteConfig converts a profile to a remote config. known_hosts is the
// user's OpenSSH file (~/.ssh/known_hosts), shared with other SSH tools.
func (p Profile) RemoteConfig() remote.Config {
	home, _ := os.UserHomeDir()
	return remote.Config{
		Host:           p.Host,
		Port:           p.Port,
		User:           p.User,
		Root:           p.Root,
		KeyFile:        p.KeyFile,
		UseAgent:       p.UseAgent,
		KnownHostsFile: filepath.Join(home, ".ssh", "known_hosts"),
	}
}

// TransferLogFile is the transfer list file of a profile.
func (p Profile) TransferLogFile(dir string) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.', r == '@':
			return r
		}
		return '_'
	}, p.DisplayName())
	return filepath.Join(dir, "transfers-"+name+".jsonl")
}
