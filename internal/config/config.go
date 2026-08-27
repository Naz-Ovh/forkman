// Package config loads and saves forkman's JSON configuration file.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// CurrentVersion is the config schema version this build writes and accepts.
const CurrentVersion = 1

// DefaultConcurrency is used when concurrency is unset.
const DefaultConcurrency = 4

// Sync modes: "api" merges through GitHub's merge-upstream endpoint, "git"
// keeps local clones and pushes with plain git.
const (
	ModeAPI = "api"
	ModeGit = "git"
)

// Remote protocols used to build git remote URLs in git mode.
const (
	ProtoSSH   = "ssh"
	ProtoHTTPS = "https"
)

// DefaultGitCloneDir holds the forks when git mode has no cloneDir set.
const DefaultGitCloneDir = "~/src/forks"

// ErrNotFound is returned by Load when no config file exists.
var ErrNotFound = errors.New("config not found")

// ErrEmptyDir is returned by ResolveDir for a blank path.
var ErrEmptyDir = errors.New("clone directory must not be empty")

// Config is the on-disk configuration.
type Config struct {
	Version           int      `json:"version"`
	Org               string   `json:"org"`
	Excluded          []string `json:"excluded"`
	Concurrency       int      `json:"concurrency"`
	DefaultBranchOnly bool     `json:"defaultBranchOnly"`
	CloneDir          string   `json:"cloneDir"`
	SyncMode          string   `json:"syncMode"`
	Protocol          string   `json:"protocol"`
}

// VersionError reports a config file written by a different forkman schema.
type VersionError struct{ Found int }

func (e *VersionError) Error() string {
	return fmt.Sprintf("unsupported config version %d (this build supports %d)", e.Found, CurrentVersion)
}

// UnknownKeyError reports an unrecognised key in the config file.
type UnknownKeyError struct{ Key string }

func (e *UnknownKeyError) Error() string {
	return fmt.Sprintf("unknown config key %q", e.Key)
}

// ValueError reports an enumerated field set to an unsupported value.
type ValueError struct {
	Key     string
	Value   string
	Allowed []string
}

func (e *ValueError) Error() string {
	return fmt.Sprintf("invalid %s %q (want %s)", e.Key, e.Value, strings.Join(e.Allowed, " or "))
}

// Path returns the config file location: $XDG_CONFIG_HOME/forkman/config.json,
// falling back to ~/.config/forkman/config.json.
func Path() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "forkman", "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".config", "forkman", "config.json"), nil
}

var unknownFieldRe = regexp.MustCompile(`unknown field "([^"]*)"`)

// Load reads and validates the config at path. A missing file yields
// ErrNotFound. Unknown keys yield *UnknownKeyError and a version this build
// does not understand yields *VersionError.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		if m := unknownFieldRe.FindStringSubmatch(err.Error()); m != nil {
			return nil, &UnknownKeyError{Key: m[1]}
		}
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	// Reject trailing content so a truncated or doubled file is not accepted.
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("parse config %s: unexpected trailing data", path)
	}
	if c.Version != CurrentVersion {
		return nil, &VersionError{Found: c.Version}
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Save writes c to path atomically, creating the directory 0700 and the file
// 0600. It stamps the current schema version.
func Save(path string, c *Config) error {
	c.Version = CurrentVersion
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	// MkdirAll is a no-op for an existing directory with looser bits.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure config dir: %w", err)
	}
	buf, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	buf = append(buf, '\n')

	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("secure temp config: %w", err)
	}
	if _, err := tmp.Write(buf); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("install config: %w", err)
	}
	return nil
}

// Normalize clamps values into supported ranges. It deliberately does not
// touch Excluded: nil (key absent) and empty (selector already run) mean
// different things.
func (c *Config) Normalize() {
	switch {
	case c.Concurrency <= 0:
		c.Concurrency = DefaultConcurrency
	case c.Concurrency > 16:
		c.Concurrency = 16
	}
	// Only default-branch syncing is supported, so this is always on.
	c.DefaultBranchOnly = true

	if c.SyncMode == "" {
		c.SyncMode = ModeAPI
	}
	if c.Protocol == "" {
		c.Protocol = ProtoSSH
	}
	if c.CloneDir == "" && c.SyncMode == ModeGit {
		c.CloneDir = DefaultGitCloneDir
	}
	// cloneDir is the single source of truth for both `clone` and git-mode
	// `sync`, so it is always stored and reported as an absolute path.
	if dir, err := ResolveDir(c.CloneDir); err == nil {
		c.CloneDir = dir
	}
}

// Validate rejects unsupported values for the enumerated fields. Empty means
// "use the default" and is always allowed; Normalize fills those in.
func (c *Config) Validate() error {
	switch c.SyncMode {
	case "", ModeAPI, ModeGit:
	default:
		return &ValueError{Key: "syncMode", Value: c.SyncMode, Allowed: []string{ModeAPI, ModeGit}}
	}
	switch c.Protocol {
	case "", ProtoSSH, ProtoHTTPS:
	default:
		return &ValueError{Key: "protocol", Value: c.Protocol, Allowed: []string{ProtoSSH, ProtoHTTPS}}
	}
	return nil
}

// GitMode reports whether syncs go through local clones and plain git.
func (c *Config) GitMode() bool { return c.SyncMode == ModeGit }

// ResolveDir expands a leading ~, makes p absolute against the working
// directory and cleans it. A blank path yields ErrEmptyDir.
func ResolveDir(p string) (string, error) {
	if p = strings.TrimSpace(p); p == "" {
		return "", ErrEmptyDir
	}
	abs, err := filepath.Abs(ExpandHome(p))
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", p, err)
	}
	return filepath.Clean(abs), nil
}

// IsExcluded reports whether repoName matches an exclusion pattern.
// Matching is on the bare repository name, case-insensitive, with an
// optional trailing '*' wildcard.
func (c *Config) IsExcluded(repoName string) bool {
	name := strings.ToLower(repoName)
	for _, pat := range c.Excluded {
		p := strings.ToLower(strings.TrimSpace(pat))
		if p == "" {
			continue
		}
		if strings.HasSuffix(p, "*") {
			if strings.HasPrefix(name, strings.TrimSuffix(p, "*")) {
				return true
			}
			continue
		}
		if name == p {
			return true
		}
	}
	return false
}

// ExpandHome replaces a leading ~ with the user's home directory.
func ExpandHome(p string) string {
	if p == "" || p[0] != '~' {
		return p
	}
	if len(p) > 1 && p[1] != '/' && p[1] != filepath.Separator {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[1:])
}
