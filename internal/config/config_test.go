package config

import (
	"os"
	"path/filepath"
	"testing"
)

func valid() *Config {
	return &Config{Role: RoleMain, Listen: "127.0.0.1:34567", SecretPath: "0123456789abcdef0123456789abcdef", DBPath: "/tmp/x.db"}
}

func TestValidate(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	cases := map[string]func(c *Config){
		"unknown role":        func(c *Config) { c.Role = "exit" },
		"public listen":       func(c *Config) { c.Listen = "0.0.0.0:34567" },
		"hostname listen":     func(c *Config) { c.Listen = "localhost:34567" },
		"short secret":        func(c *Config) { c.SecretPath = "abc" },
		"router without main": func(c *Config) { c.Role = RoleRouter },
		"no db":               func(c *Config) { c.DBPath = "" },
	}
	for name, mutate := range cases {
		c := valid()
		mutate(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	c := valid()
	c.Listen = "[::1]:34567"
	if err := c.Validate(); err != nil {
		t.Errorf("IPv6 loopback rejected: %v", err)
	}
}

func TestSaveLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	c := valid()
	c.Domain = "example.com"
	if err := Save(path, c); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("config must be 0600, got %v", st.Mode().Perm())
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if *got != *c {
		t.Fatalf("round trip mismatch: %+v vs %+v", got, c)
	}
}

func TestNewSecretPath(t *testing.T) {
	a, _ := NewSecretPath()
	b, _ := NewSecretPath()
	if !secretRe.MatchString(a) || a == b {
		t.Fatalf("bad secrets: %q %q", a, b)
	}
}
