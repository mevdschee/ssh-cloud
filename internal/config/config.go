// Package config stores connection profiles and settings in
// %APPDATA%\SSH-Cloud\config.json (the user config dir on other systems).
package config

import (
	"crypto/rand"
	"encoding/hex"
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
	// ID is stable for the life of the profile (the name can change); it
	// names the profile's transfer list file.
	ID   string `json:"id"`
	Name string `json:"name"`
	Host string `json:"host"`
	Port int    `json:"port,omitempty"`
	User string `json:"user"`
	Root string `json:"root,omitempty"`
	// Auth is remote.AuthAuto, AuthKeyFile, AuthPageant or AuthPassword.
	Auth    string `json:"auth,omitempty"`
	KeyFile string `json:"keyFile,omitempty"`
	// UseAgent is only read from configs written before Auth existed.
	UseAgent bool `json:"useAgent,omitempty"`
}

// Config is the whole settings file.
type Config struct {
	Profiles       []Profile `json:"profiles"`
	LastProfile    string    `json:"lastProfile,omitempty"`
	DownloadFolder string    `json:"downloadFolder,omitempty"`
	TreeWidth      int       `json:"treeWidth,omitempty"` // folder tree width at 96 DPI; 0 is the default
	ShowHidden     bool      `json:"showHidden,omitempty"` // show files and folders whose name starts with a dot
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
	for i := range c.Profiles {
		if c.Profiles[i].Auth == "" {
			c.Profiles[i].Auth = remote.AuthAuto
			if c.Profiles[i].KeyFile != "" {
				c.Profiles[i].Auth = remote.AuthKeyFile
			}
			c.Profiles[i].UseAgent = false
		}
		if c.Profiles[i].ID == "" {
			// Profiles from before IDs existed keep their transfer list,
			// which was named after the display name.
			c.Profiles[i].ID = sanitize(c.Profiles[i].DisplayName())
		}
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

// Profile returns the profile with the given ID.
func (c *Config) Profile(id string) (Profile, bool) {
	for _, p := range c.Profiles {
		if p.ID == id {
			return p, true
		}
	}
	return Profile{}, false
}

// Last returns the last used profile, if any.
func (c *Config) Last() (Profile, bool) {
	if p, ok := c.Profile(c.LastProfile); ok {
		return p, true
	}
	// Older configs stored the name.
	for _, p := range c.Profiles {
		if p.Name == c.LastProfile {
			return p, true
		}
	}
	return Profile{}, false
}

// Put adds a profile or replaces the one with the same ID. A profile without
// ID gets a new one. It returns the stored profile.
func (c *Config) Put(p Profile) Profile {
	if p.ID == "" {
		p.ID = newID()
	}
	for i := range c.Profiles {
		if c.Profiles[i].ID == p.ID {
			c.Profiles[i] = p
			return p
		}
	}
	c.Profiles = append(c.Profiles, p)
	return p
}

// Delete removes a profile by ID.
func (c *Config) Delete(id string) {
	for i := range c.Profiles {
		if c.Profiles[i].ID == id {
			c.Profiles = append(c.Profiles[:i], c.Profiles[i+1:]...)
			break
		}
	}
	if c.LastProfile == id {
		c.LastProfile = ""
	}
}

// SetLast remembers the profile to connect to at the next start.
func (c *Config) SetLast(id string) { c.LastProfile = id }

func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
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
		Auth:           p.Auth,
		KeyFile:        p.KeyFile,
		KnownHostsFile: filepath.Join(home, ".ssh", "known_hosts"),
	}
}

// TransferLogFile is the transfer list file of a profile.
func (p Profile) TransferLogFile(dir string) string {
	id := p.ID
	if id == "" {
		id = sanitize(p.DisplayName())
	}
	return filepath.Join(dir, "transfers-"+id+".jsonl")
}

func sanitize(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.', r == '@':
			return r
		}
		return '_'
	}, name)
}
