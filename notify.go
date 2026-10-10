package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// updateTmpl is compiled into the binary.
//
//go:embed update.html
var updateHTML string

var updateTmpl = template.Must(template.New("update").Parse(updateHTML))

type NotifyConfig struct {
	PushPlus *PushPlusConfig `yaml:"pushplus"`
}

// PushPlusConfig configures https://www.pushplus.plus notifications.
type PushPlusConfig struct {
	Token   string     `yaml:"token"`
	Channel string     `yaml:"channel"` // optional: wechat (default), app, webhook, ...
	APIURL  string     `yaml:"api_url"`
	Targets []PPTarget `yaml:"targets"` // empty = send to the token owner only

	client *http.Client
}

// PPTarget is one recipient. Set topic (a group code) OR to (friend tokens), not both.
// With neither, the message goes to the token owner.
type PPTarget struct {
	Name  string     `yaml:"name"`
	Topic string     `yaml:"topic"`
	To    StringList `yaml:"to"`
}

// StringList accepts either "a,b" or [a, b] in YAML.
type StringList []string

func (s *StringList) UnmarshalYAML(n *yaml.Node) error {
	var raw []string
	if n.Kind == yaml.ScalarNode {
		raw = strings.Split(n.Value, ",")
	} else if err := n.Decode(&raw); err != nil {
		return err
	}
	for _, v := range raw {
		if v = strings.TrimSpace(v); v != "" {
			*s = append(*s, v)
		}
	}
	return nil
}

func (c *NotifyConfig) validate() error {
	p := c.PushPlus
	if p == nil {
		return nil
	}
	if p.Token == "" {
		p.Token = os.Getenv("PUSHPLUS_TOKEN")
	}
	if p.Token == "" {
		slog.Warn("notify.pushplus.token is empty: update notifications are disabled")
		c.PushPlus = nil
		return nil
	}
	if p.APIURL == "" {
		p.APIURL = "https://www.pushplus.plus/send"
	}
	for i, t := range p.Targets {
		if t.Topic != "" && len(t.To) > 0 {
			return fmt.Errorf("notify.pushplus.targets[%d]: set either topic or to, not both", i)
		}
		if len(t.To) > 100 {
			return fmt.Errorf("notify.pushplus.targets[%d]: at most 100 friend tokens per target", i)
		}
	}
	tr := http.DefaultTransport.(*http.Transport).Clone() // keeps HTTPS_PROXY support
	p.client = &http.Client{Transport: tr, Timeout: 20 * time.Second}
	return nil
}

func (t PPTarget) label() string {
	switch {
	case t.Name != "":
		return t.Name
	case t.Topic != "":
		return "group " + t.Topic
	case len(t.To) > 0:
		return "friends " + strings.Join(t.To, ",")
	}
	return "self"
}

type updateEvent struct {
	Project string
	Tag     string
	Time    string
	Host    string
	URL     string
	Notes   []string
	Assets  []string
}

func renderUpdate(ev updateEvent) (string, error) {
	var b bytes.Buffer
	if err := updateTmpl.Execute(&b, ev); err != nil {
		return "", err
	}
	return b.String(), nil
}

func (p *PushPlusConfig) notifyDeployed(ctx context.Context, proj Project, rel *Release, assets []string) {
	host, _ := os.Hostname()
	ev := updateEvent{
		Project: proj.Name, Tag: rel.TagName, Time: time.Now().Format("2006-01-02 15:04:05"),
		Host: host, URL: rel.HTMLURL, Notes: summarizeNotes(rel.Body, 8), Assets: assets,
	}
	content, err := renderUpdate(ev)
	if err != nil {
		slog.Warn("render notification failed", "error", err)
		return
	}
	p.sendAll(ctx, fmt.Sprintf("【%s】已更新 %s", proj.Name, rel.TagName), content)
}

func (p *PushPlusConfig) sendTest(ctx context.Context) error {
	host, _ := os.Hostname()
	content, err := renderUpdate(updateEvent{
		Project: "测试 / test", Tag: "v0.0.0-test", Time: time.Now().Format("2006-01-02 15:04:05"), Host: host,
		Notes: []string{"这是一条来自 bastion-pull 的测试消息", "This is a test message"}, Assets: []string{"app.jar", "web.tar.gz"},
	})
	if err != nil {
		return err
	}
	if n := p.sendAll(ctx, "【bastion-pull】测试通知", content); n > 0 {
		return fmt.Errorf("%d target(s) failed", n)
	}
	return nil
}

// sendAll sends to every target and returns how many failed.
func (p *PushPlusConfig) sendAll(ctx context.Context, title, content string) (failed int) {
	targets := p.Targets
	if len(targets) == 0 {
		targets = []PPTarget{{}}
	}
	for _, t := range targets {
		var err error
		for attempt := 1; attempt <= 3; attempt++ {
			if err = p.send(ctx, t, title, content); err == nil || ctx.Err() != nil {
				break
			}
			select {
			case <-time.After(time.Duration(attempt) * retryDelay):
			case <-ctx.Done():
			}
		}
		if err != nil {
			failed++
			slog.Warn("notification failed", "target", t.label(), "error", err)
		} else {
			slog.Info("notification sent", "target", t.label())
		}
	}
	return failed
}

func (p *PushPlusConfig) send(ctx context.Context, t PPTarget, title, content string) error {
	body := map[string]string{"token": p.Token, "title": title, "content": content, "template": "html"}
	if p.Channel != "" {
		body["channel"] = p.Channel
	}
	if t.Topic != "" {
		body["topic"] = t.Topic
	} else if len(t.To) > 0 {
		body["to"] = strings.Join(t.To, ",")
	}
	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.APIURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var r struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("unexpected response (HTTP %d): %.200s", resp.StatusCode, raw)
	}
	if r.Code != 200 {
		return fmt.Errorf("pushplus code %d: %s", r.Code, r.Msg)
	}
	return nil
}

var (
	byUserRe     = regexp.MustCompile(`\s+by @\S+( in \S+)?$`)
	commitOnlyRe = regexp.MustCompile(`^Commit: [0-9a-f]{7,40}$`)
)

// summarizeNotes turns release notes (plain text or GitHub-generated markdown) into a few short lines.
func summarizeNotes(body string, maxItems int) []string {
	var out []string
	total := 0
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r", ""), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "", commitOnlyRe.MatchString(line):
			continue
		case strings.HasPrefix(line, "**Full Changelog**"):
			continue
		case strings.HasPrefix(line, "#"):
			if strings.Contains(strings.ToLower(line), "new contributors") {
				return capItems(out, total, maxItems)
			}
			continue
		}
		line = strings.TrimLeft(line, "*-+ ")
		line = byUserRe.ReplaceAllString(line, "")
		if line == "" {
			continue
		}
		total++
		if len(out) < maxItems {
			if utf8.RuneCountInString(line) > 100 {
				line = string([]rune(line)[:100]) + "…"
			}
			out = append(out, line)
		}
	}
	return capItems(out, total, maxItems)
}

func capItems(out []string, total, maxItems int) []string {
	if total > maxItems {
		out = append(out, fmt.Sprintf("……等共 %d 项更新", total))
	}
	return out
}
