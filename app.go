package main

import (
	"bytes"
	"context"
	"errors"
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

	// releases to merge: just the latest one, or (partial mode) everything since the last deployment
	pending := []*Release{rel}
	if cur, ok := a.state.Projects[p.Repo]; ok && p.Partial {
		if list, err := a.gh.releasesAfter(ctx, p.Repo, cur.ReleaseID); err != nil {
			slog.Warn("cannot list releases, using the latest only", "project", p.Name, "error", err)
		} else if len(list) > 0 {
			pending = list
		}
	}

	// 1. resolve every configured asset (newest pending release that carries it) before downloading
	type job struct {
		asset Asset
		ra    ReleaseAsset
		file  string
	}
	var jobs []job
	for _, ca := range p.Assets {
		var found bool
		var ra ReleaseAsset
		var lastErr error
		for i := len(pending) - 1; i >= 0; i-- {
			m, err := matchAsset(pending[i], ca.Name)
			if err == nil {
				ra, found = m, true
				break
			}
			if !errors.Is(err, errNoMatch) {
				return fmt.Errorf("release %s: %w", pending[i].TagName, err)
			}
			lastErr = err
		}
		switch {
		case found:
			jobs = append(jobs, job{asset: ca, ra: ra})
		case p.Partial:
			slog.Debug("asset not in the pending releases, skipped", "asset", ca.Name)
		default:
			return fmt.Errorf("release %s: %w", rel.TagName, lastErr)
		}
	}

	if len(jobs) > 0 {
		// 2. download everything (resumable) before touching the live files
		dlDir := filepath.Join(filepath.Dir(a.cfg.StateFile), "downloads", strings.ReplaceAll(p.Repo, "/", "_"))
		if err := os.MkdirAll(dlDir, 0o755); err != nil {
			return err
		}
		defer os.RemoveAll(dlDir)
		for i := range jobs {
			jobs[i].file = filepath.Join(dlDir, fmt.Sprintf("%d-%s", jobs[i].ra.ID, jobs[i].ra.Name))
			slog.Info("downloading", "project", p.Name, "asset", jobs[i].ra.Name, "size", jobs[i].ra.Size)
			if err := a.gh.download(ctx, p.Repo, jobs[i].ra, jobs[i].file); err != nil {
				return err
			}
		}

		// 3. replace
		for _, j := range jobs {
			var err error
			if j.asset.ExtractTo != "" {
				slog.Info("extracting", "asset", j.ra.Name, "to", j.asset.ExtractTo)
				err = extractReplace(j.file, j.ra.Name, j.asset.ExtractTo)
			} else {
				slog.Info("copying", "asset", j.ra.Name, "to", j.asset.CopyTo)
				err = copyFileAtomic(j.file, j.asset.CopyTo, j.asset.mode)
			}
			if err != nil {
				return fmt.Errorf("apply %s: %w", j.ra.Name, err)
			}
		}

		// 4. restart: per-asset commands for what changed (each once), then the project-level one
		done := map[string]bool{}
		for _, j := range jobs {
			if j.asset.Restart != "" && !done[j.asset.Restart] {
				done[j.asset.Restart] = true
				if err := runRestart(ctx, j.asset.Restart); err != nil {
					return fmt.Errorf("restart command for %s: %w", j.ra.Name, err)
				}
			}
		}
		if p.Restart != "" && !done[p.Restart] {
			if err := runRestart(ctx, p.Restart); err != nil {
				return fmt.Errorf("restart command: %w", err)
			}
		}
	} else {
		slog.Info("release carries none of the configured assets, nothing to deploy", "project", p.Name, "tag", rel.TagName)
	}

	// 5. remember; only now, so any earlier failure is retried on the next tick
	a.state.Projects[p.Repo] = projectState{ReleaseID: rel.ID, Tag: rel.TagName, DeployedAt: time.Now()}
	if err := a.state.save(a.cfg.StateFile); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	a.etags[p.Repo] = etag
	slog.Info("deployed", "project", p.Name, "tag", rel.TagName, "assets", len(jobs))

	// 6. tell people; a failed notification never fails the (already finished) deployment
	if len(jobs) > 0 && a.cfg.Notify.PushPlus != nil && (p.Notify == nil || *p.Notify) {
		names := make([]string, len(jobs))
		for i, j := range jobs {
			names[i] = j.ra.Name
		}
		merged := *rel
		if len(pending) > 1 {
			var bodies []string
			for _, r := range pending {
				if strings.TrimSpace(r.Body) != "" {
					bodies = append(bodies, r.Body)
				}
			}
			merged.Body = strings.Join(bodies, "\n")
		}
		a.cfg.Notify.PushPlus.notifyDeployed(ctx, p, &merged, names, "")
	}
	return nil
}

var errNoMatch = errors.New("no asset matches")

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
		return ReleaseAsset{}, fmt.Errorf("%w %q (available: %s)", errNoMatch, pattern, strings.Join(names, ", "))
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

// notifyLatest sends the real notification for each project's latest release without deploying
// anything. It is meant for checking the notification setup. Returns how many projects failed.
func (a *App) notifyLatest(ctx context.Context) (failed int) {
	pp := a.cfg.Notify.PushPlus
	for _, p := range a.cfg.Projects {
		if p.Notify != nil && !*p.Notify {
			slog.Info("notifications disabled for project, skipping", "project", p.Name)
			continue
		}
		rel, _, _, err := a.gh.latest(ctx, p.Repo, "")
		if err != nil {
			failed++
			slog.Error("cannot fetch latest release", "project", p.Name, "error", err)
			continue
		}
		var names []string
		for _, ca := range p.Assets {
			if m, err := matchAsset(rel, ca.Name); err == nil {
				names = append(names, m.Name)
			}
		}
		pp.notifyDeployed(ctx, p, rel, names, "【测试】")
	}
	return failed
}
