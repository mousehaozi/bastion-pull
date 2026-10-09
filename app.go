package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

type App struct {
	cfg   *Config
	gh    *Client
	state *State
	// etags live in memory only: after a failed deployment we must not get 304 forever.
	etags map[string]string
}

func newApp(cfg *Config) (*App, error) {
	st, err := loadState(cfg.StateFile)
	if err != nil {
		return nil, fmt.Errorf("read state file %s: %w", cfg.StateFile, err)
	}
	return &App{cfg: cfg, gh: newClient(cfg.APIURL, cfg.Token), state: st, etags: map[string]string{}}, nil
}

// checkAll runs one pass over all projects and returns how many failed.
func (a *App) checkAll(ctx context.Context) (failed int) {
	for _, p := range a.cfg.Projects {
		if ctx.Err() != nil {
			return failed
		}
		if err := a.checkProject(ctx, p); err != nil {
			failed++
			delete(a.etags, p.Repo)
			slog.Error("check failed", "project", p.Name, "error", err)
		}
	}
	return failed
}

func (a *App) checkProject(ctx context.Context, p Project) error {
	rel, etag, notModified, err := a.gh.latest(ctx, p.Repo, a.etags[p.Repo])
	if err != nil {
		return err
	}
	if notModified {
		slog.Debug("no change", "project", p.Name)
		return nil
	}
	if cur, ok := a.state.Projects[p.Repo]; ok && cur.ReleaseID == rel.ID {
		a.etags[p.Repo] = etag
		slog.Debug("already deployed", "project", p.Name, "tag", rel.TagName)
		return nil
	}

	slog.Info("new release found", "project", p.Name, "tag", rel.TagName)

	// 1. resolve every configured asset before downloading anything
	matched := make([]ReleaseAsset, len(p.Assets))
	for i, ca := range p.Assets {
		m, err := matchAsset(rel, ca.Name)
		if err != nil {
			return fmt.Errorf("release %s: %w", rel.TagName, err)
		}
		matched[i] = m
	}

	// 2. download everything (resumable) before touching the live files
	dlDir := filepath.Join(filepath.Dir(a.cfg.StateFile), "downloads", strings.ReplaceAll(p.Repo, "/", "_"))
	if err := os.MkdirAll(dlDir, 0o755); err != nil {
		return err
	}
	files := make([]string, len(p.Assets))
	for i, ra := range matched {
		files[i] = filepath.Join(dlDir, fmt.Sprintf("%d-%s", ra.ID, ra.Name))
		slog.Info("downloading", "project", p.Name, "asset", ra.Name, "size", ra.Size)
		if err := a.gh.download(ctx, p.Repo, ra, files[i]); err != nil {
			return err
		}
	}

	// 3. replace
	for i, ca := range p.Assets {
		var err error
		if ca.ExtractTo != "" {
			slog.Info("extracting", "asset", matched[i].Name, "to", ca.ExtractTo)
			err = extractReplace(files[i], matched[i].Name, ca.ExtractTo)
		} else {
			slog.Info("copying", "asset", matched[i].Name, "to", ca.CopyTo)
			err = copyFileAtomic(files[i], ca.CopyTo, ca.mode)
		}
		if err != nil {
			return fmt.Errorf("apply %s: %w", matched[i].Name, err)
		}
	}

	// 4. restart
	if p.Restart != "" {
		if err := runRestart(ctx, p.Restart); err != nil {
			return fmt.Errorf("restart command: %w", err)
		}
	}

	// 5. remember; only now, so any earlier failure is retried on the next tick
	a.state.Projects[p.Repo] = projectState{ReleaseID: rel.ID, Tag: rel.TagName, DeployedAt: time.Now()}
	if err := a.state.save(a.cfg.StateFile); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	a.etags[p.Repo] = etag
	os.RemoveAll(dlDir)
	slog.Info("deployed", "project", p.Name, "tag", rel.TagName)
	return nil
}

func matchAsset(rel *Release, pattern string) (ReleaseAsset, error) {
	var found []ReleaseAsset
	var names []string
	for _, ra := range rel.Assets {
		names = append(names, ra.Name)
		if ra.State != "" && ra.State != "uploaded" {
			continue
		}
		if ok, _ := path.Match(pattern, ra.Name); ok {
			found = append(found, ra)
		}
	}
	switch len(found) {
	case 0:
		return ReleaseAsset{}, fmt.Errorf("no asset matches %q (available: %s)", pattern, strings.Join(names, ", "))
	case 1:
		return found[0], nil
	default:
		return ReleaseAsset{}, fmt.Errorf("pattern %q matches %d assets, make it more specific", pattern, len(found))
	}
}

func runRestart(ctx context.Context, command string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	slog.Info("running restart command", "cmd", command)
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if s := strings.TrimSpace(out.String()); s != "" {
		slog.Info("restart output", "output", s)
	}
	return err
}
