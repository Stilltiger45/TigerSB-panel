// Package config loads and saves the panel configuration file.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
)

// DefaultPath is where the installer writes the configuration.
const DefaultPath = "/etc/tigersb/config.json"

// Role is the job a server has in the network.
type Role string

const (
	// RoleMain is the foreign exit server that hosts the panel.
	RoleMain Role = "main"
	// RoleRouter is the routing point (entry node in RU) that cascades to a main server.
	RoleRouter Role = "router"
)

// Config is the on-disk panel configuration.
type Config struct {
	Role       Role   `json:"role"`
	Listen     string `json:"listen"`
	SecretPath string `json:"secret_path"`
	Domain     string `json:"domain,omitempty"`
	Email      string `json:"email,omitempty"`
	DBPath     string `json:"db_path"`
	// MainServer is set on a routing point: the main server it cascades to.
	MainServer string `json:"main_server,omitempty"`
}

var secretRe = regexp.MustCompile(`^[a-f0-9]{32}$`)

// NewSecretPath returns a random 128-bit hex path segment.
func NewSecretPath() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Validate checks that the configuration is safe to run with.
func (c *Config) Validate() error {
	switch c.Role {
	case RoleMain, RoleRouter:
	default:
		return fmt.Errorf("неизвестная роль %q (ожидается main или router)", c.Role)
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("некорректный адрес listen %q: %w", c.Listen, err)
	}
	// The panel is reachable only through an SSH tunnel, so it must never
	// listen on a public interface.
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("панель должна слушать только loopback-адрес, а не %q", host)
	}
	if !secretRe.MatchString(c.SecretPath) {
		return errors.New("secret_path должен состоять из 32 hex-символов")
	}
	if c.DBPath == "" {
		return errors.New("не задан db_path")
	}
	if c.Role == RoleRouter && c.MainServer == "" {
		return errors.New("для точки маршрутизации нужен main_server")
	}
	return nil
}

// Load reads and validates the configuration at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("разбор %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Save validates c and writes it to path with 0600 permissions.
func Save(path string, c *Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
