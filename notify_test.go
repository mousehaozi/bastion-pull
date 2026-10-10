package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakePush struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []map[string]string
	fail bool
}

func newFakePush(t *testing.T) *fakePush {
	f := &fakePush{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		m := map[string]string{}
		json.Unmarshal(b, &m)
		f.mu.Lock()
		f.reqs = append(f.reqs, m)
		fail := f.fail
		f.mu.Unlock()
		if fail {
			w.Write([]byte(`{"code":999,"msg":"bad token"}`))
			return
		}
		w.Write([]byte(`{"code":200,"msg":"ok","data":"x"}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePush) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.reqs) }

func TestPushPlusTargets(t *testing.T) {
	retryDelay = time.Millisecond
	f := newFakePush(t)
	cfg := NotifyConfig{PushPlus: &PushPlusConfig{
		Token: "T", APIURL: f.srv.URL, Channel: "wechat",
		Targets: []PPTarget{
			{Name: "dev", Topic: "devgroup"},
			{To: StringList{"u1", "u2"}},
			{}, // the token owner
		},
	}}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	cfg.PushPlus.notifyDeployed(context.Background(), Project{Name: "demo"},
		&Release{TagName: "v1", HTMLURL: "https://x/y", Body: "- fix login\n- add <b>export</b>"}, []string{"app.jar"})

	if len(f.reqs) != 3 {
		t.Fatalf("want 3 requests, got %d", len(f.reqs))
	}
	if f.reqs[0]["topic"] != "devgroup" || f.reqs[0]["to"] != "" {
		t.Fatalf("group request wrong: %v", f.reqs[0])
	}
	if f.reqs[1]["to"] != "u1,u2" || f.reqs[1]["topic"] != "" {
		t.Fatalf("friend request wrong: %v", f.reqs[1])
	}
	if f.reqs[2]["topic"] != "" || f.reqs[2]["to"] != "" || f.reqs[2]["token"] != "T" {
		t.Fatalf("self request wrong: %v", f.reqs[2])
	}
	c := f.reqs[0]["content"]
	if f.reqs[0]["template"] != "html" || !strings.Contains(c, "demo") || !strings.Contains(c, "fix login") || !strings.Contains(c, "app.jar") {
		t.Fatalf("content missing parts: %s", c)
	}
	if strings.Contains(c, "<b>export</b>") || !strings.Contains(c, "&lt;b&gt;") {
		t.Fatalf("release notes must be HTML-escaped: %s", c)
	}
}

func TestPushPlusFailureIsReported(t *testing.T) {
	retryDelay = time.Millisecond
	f := newFakePush(t)
	f.fail = true
	p := &PushPlusConfig{Token: "T", APIURL: f.srv.URL}
	if err := (&NotifyConfig{PushPlus: p}).validate(); err != nil {
		t.Fatal(err)
	}
	if err := p.sendTest(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	if f.count() != 3 {
		t.Fatalf("expected 3 attempts, got %d", f.count())
	}
}

func TestNotifyConfigValidation(t *testing.T) {
	bad := NotifyConfig{PushPlus: &PushPlusConfig{Token: "T", Targets: []PPTarget{{Topic: "g", To: StringList{"u"}}}}}
	if bad.validate() == nil {
		t.Fatal("topic+to must be rejected")
	}
	t.Setenv("PUSHPLUS_TOKEN", "")
	empty := NotifyConfig{PushPlus: &PushPlusConfig{Targets: []PPTarget{{Topic: "g"}}}}
	if err := empty.validate(); err != nil || empty.PushPlus != nil {
		t.Fatal("empty token must disable notifications without failing")
	}
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte(`token: x
notify:
  pushplus:
    token: pp
    targets:
      - {name: g, topic: grp}
      - {to: "a, b"}
      - {to: [c, d]}
projects:
  - repo: o/r
    notify: false
    assets: [{name: a, copy_to: /x}]
`), 0o600)
	c, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	tg := c.Notify.PushPlus.Targets
	if len(tg) != 3 || strings.Join(tg[1].To, "|") != "a|b" || strings.Join(tg[2].To, "|") != "c|d" {
		t.Fatalf("targets parsed wrong: %+v", tg)
	}
	if c.Projects[0].Notify == nil || *c.Projects[0].Notify {
		t.Fatal("project notify:false not parsed")
	}
}

func TestSummarizeNotes(t *testing.T) {
	body := "Commit: 0123456789abcdef0123456789abcdef01234567\n## What's Changed\n* feat: login by @bob in https://github.com/o/r/pull/3\n* fix: crash by @amy in https://github.com/o/r/pull/4\n\n## New Contributors\n* @bob made their first contribution\n\n**Full Changelog**: https://github.com/o/r/compare/a...b"
	got := summarizeNotes(body, 8)
	if len(got) != 2 || got[0] != "feat: login" || got[1] != "fix: crash" {
		t.Fatalf("got %q", got)
	}
	var many []string
	for i := 0; i < 12; i++ {
		many = append(many, "- item")
	}
	got = summarizeNotes(strings.Join(many, "\n"), 8)
	if len(got) != 9 || !strings.Contains(got[8], "12") {
		t.Fatalf("expected cap with summary line, got %q", got)
	}
	if summarizeNotes("Commit: 0123456789abcdef", 8) != nil {
		t.Fatal("commit-only notes should be empty")
	}
}

// A deployment sends notifications once; unchanged polls and notify:false projects send nothing.
func TestDeployTriggersNotification(t *testing.T) {
	retryDelay = time.Millisecond
	dir := t.TempDir()
	f := newFakeGitHub(t)
	f.web = makeTarGz(t, map[string]string{"index.html": "v1"})
	f.jar = []byte("j")
	pp := newFakePush(t)
	cfg := testConfig(f, dir)
	cfg.Notify.PushPlus = &PushPlusConfig{Token: "T", APIURL: pp.srv.URL, Targets: []PPTarget{{Topic: "a"}, {Topic: "b"}}}
	if err := cfg.Notify.validate(); err != nil {
		t.Fatal(err)
	}
	app, _ := newApp(cfg)
	app.checkAll(context.Background())
	if pp.count() != 2 {
		t.Fatalf("want 2 notifications after deploy, got %d", pp.count())
	}
	app.checkAll(context.Background())
	if pp.count() != 2 {
		t.Fatal("no notification expected without a new release")
	}

	off := false
	f.releaseID = 2
	cfg.Projects[0].Notify = &off
	app.checkAll(context.Background())
	if pp.count() != 2 {
		t.Fatal("notify:false must suppress notifications")
	}
}
