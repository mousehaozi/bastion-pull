package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Token     string        `yaml:"token"`
	APIURL    string        `yaml:"api_url"`
	Interval  time.Duration `yaml:"interval"`
	StateFile string        `yaml:"state_file"`
	Projects  []Project     `yaml:"projects"`
}

type Project struct {
	Name    string  `yaml:"name"`
	Repo    string  `yaml:"repo"`
	Assets  []Asset `yaml:"assets"`
	Restart string  `yaml:"restart"`
}

type Asset struct {
	// Name is the release asset file name; shell-style globs such as "app-*.jar" are allowed.
	Name      string `yaml:"name"`
	ExtractTo string `yaml:"extract_to"`
	CopyTo    string `yaml:"copy_to"`
	Mode      string `yaml:"mode"`

	mode os.FileMode
}

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func loadConfig(path string) (*Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", abs, err)
	}

	if cfg.Token == "" {
		cfg.Token = os.Getenv("GITHUB_TOKEN")
	}
	if cfg.Token == "" {
		slog.Warn("no token configured: only public repositories work, and the API rate limit is very low")
	}
	if fi, err := os.Stat(abs); err == nil && fi.Mode().Perm()&0o007 != 0 && cfg.Token != "" {
		slog.Warn("config file is readable by other users, consider chmod 600", "file", abs)
	}
	if cfg.APIURL == "" {
		cfg.APIURL = "https://api.github.com"
	}
	cfg.APIURL = strings.TrimRight(cfg.APIURL, "/")
	if cfg.Interval == 0 {
		cfg.Interval = time.Minute
	}
	if cfg.Interval < 10*time.Second {
		return nil, fmt.Errorf("interval must be at least 10s, got %s", cfg.Interval)
	}
	if cfg.StateFile == "" {
		cfg.StateFile = filepath.Join(filepath.Dir(abs), "state.json")
	}
	if len(cfg.Projects) == 0 {
		return nil, fmt.Errorf("no projects configured")
	}

	seen := map[string]bool{}
	for i := range cfg.Projects {
		p := &cfg.Projects[i]
		if !repoRe.MatchString(p.Repo) {
			return nil, fmt.Errorf("projects[%d]: repo must look like owner/name, got %q", i, p.Repo)
		}
		if seen[p.Repo] {
			return nil, fmt.Errorf("projects[%d]: repo %s is listed twice", i, p.Repo)
		}
		seen[p.Repo] = true
		if p.Name == "" {
			p.Name = p.Repo
		}
		if len(p.Assets) == 0 {
			return nil, fmt.Errorf("project %s: no assets configured", p.Name)
		}
		for j := range p.Assets {
			a := &p.Assets[j]
			if a.Name == "" {
				return nil, fmt.Errorf("project %s: assets[%d]: name is required", p.Name, j)
			}
			if (a.ExtractTo == "") == (a.CopyTo == "") {
				return nil, fmt.Errorf("project %s: asset %s: set exactly one of extract_to / copy_to", p.Name, a.Name)
			}
			if a.Mode != "" {
				m, err := strconv.ParseUint(a.Mode, 8, 32)
				if err != nil {
					return nil, fmt.Errorf("project %s: asset %s: mode must be octal like 0755", p.Name, a.Name)
				}
				a.mode = os.FileMode(m)
			}
		}
	}
	return &cfg, nil
}

func dirOf(p string) string { return filepath.Dir(p) }
