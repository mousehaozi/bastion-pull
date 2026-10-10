package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func makeTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

type fakeGitHub struct {
	srv       *httptest.Server
	releaseID int64
	web, jar  []byte
	notMod    int32
	breakOnce int32 // first asset download is cut short once
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{releaseID: 1}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", 401)
			return
		}
		etag := fmt.Sprintf(`"rel-%d"`, f.releaseID)
		if r.Header.Get("If-None-Match") == etag {
			atomic.AddInt32(&f.notMod, 1)
			w.WriteHeader(304)
			return
		}
		sum := sha256.Sum256(f.web)
		w.Header().Set("ETag", etag)
		fmt.Fprintf(w, `{"id":%d,"tag_name":"v%d","assets":[
			{"id":10,"name":"web.tar.gz","size":%d,"state":"uploaded","digest":"sha256:%s"},
			{"id":11,"name":"app.jar","size":%d,"state":"uploaded"}]}`,
			f.releaseID, f.releaseID, len(f.web), hex.EncodeToString(sum[:]), len(f.jar))
	})
	mux.HandleFunc("/repos/o/r/releases/assets/", func(w http.ResponseWriter, r *http.Request) {
		body := f.web
		if strings.HasSuffix(r.URL.Path, "/11") {
			body = f.jar
		}
		if atomic.CompareAndSwapInt32(&f.breakOnce, 1, 0) && r.Header.Get("Range") == "" {
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			w.Write(body[:len(body)/2]) // then the handler returns: short body -> client error
			if h, ok := w.(http.Hijacker); ok {
				c, _, _ := h.Hijack()
				c.Close()
			}
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body)) // supports Range
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func testConfig(f *fakeGitHub, dir string) *Config {
	return &Config{
		Token: "tok", APIURL: f.srv.URL, Interval: time.Minute,
		StateFile: filepath.Join(dir, "state", "state.json"),
		Projects: []Project{{
			Name: "t", Repo: "o/r",
			Assets: []Asset{
				{Name: "web.tar.gz", ExtractTo: filepath.Join(dir, "www")},
				{Name: "*.jar", CopyTo: filepath.Join(dir, "bin", "app.jar")},
			},
			Restart: "echo restarted >> " + filepath.Join(dir, "restart.log"),
		}},
	}
}

func TestDeployFlow(t *testing.T) {
	retryDelay = time.Millisecond
	dir := t.TempDir()
	f := newFakeGitHub(t)
	f.web = makeTarGz(t, map[string]string{"index.html": "v1", "assets/a.js": "js1"})
	f.jar = []byte("jar-v1")
	f.breakOnce = 1

	// pre-existing content must be replaced entirely
	os.MkdirAll(filepath.Join(dir, "www"), 0o755)
	os.WriteFile(filepath.Join(dir, "www", "stale.html"), []byte("old"), 0o644)

	app, err := newApp(testConfig(f, dir))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if n := app.checkAll(ctx); n != 0 {
		t.Fatalf("first pass failed")
	}
	read := func(p string) string { b, _ := os.ReadFile(filepath.Join(dir, p)); return string(b) }
	if read("www/index.html") != "v1" || read("www/assets/a.js") != "js1" || read("bin/app.jar") != "jar-v1" {
		t.Fatal("files not deployed")
	}
	if _, err := os.Stat(filepath.Join(dir, "www", "stale.html")); err == nil {
		t.Fatal("stale file survived")
	}
	if strings.Count(read("restart.log"), "restarted") != 1 {
		t.Fatal("expected one restart")
	}

	// nothing changed: 304, no restart
	app.checkAll(ctx)
	if atomic.LoadInt32(&f.notMod) != 1 || strings.Count(read("restart.log"), "restarted") != 1 {
		t.Fatalf("expected a 304 and no second restart")
	}

	// a new instance (restart of bastion-pull) must not redeploy the same release
	app2, _ := newApp(testConfig(f, dir))
	app2.checkAll(ctx)
	if strings.Count(read("restart.log"), "restarted") != 1 {
		t.Fatal("redeployed after restart")
	}

	// new release
	f.releaseID = 2
	f.web = makeTarGz(t, map[string]string{"index.html": "v2"})
	f.jar = []byte("jar-v2")
	app2.checkAll(ctx)
	if read("www/index.html") != "v2" || read("bin/app.jar") != "jar-v2" || strings.Count(read("restart.log"), "restarted") != 2 {
		t.Fatal("update not applied")
	}
}

func TestFailedRestartIsRetried(t *testing.T) {
	dir := t.TempDir()
	f := newFakeGitHub(t)
	f.web = makeTarGz(t, map[string]string{"index.html": "v1"})
	f.jar = []byte("j")
	cfg := testConfig(f, dir)
	cfg.Projects[0].Restart = "exit 3"
	app, _ := newApp(cfg)
	if app.checkAll(context.Background()) != 1 {
		t.Fatal("expected failure")
	}
	if len(app.state.Projects) != 0 {
		t.Fatal("state must not be saved on failure")
	}
	cfg.Projects[0].Restart = "true"
	if app.checkAll(context.Background()) != 0 || len(app.state.Projects) != 1 {
		t.Fatal("expected retry to succeed")
	}
}

func TestExtractRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	arc := filepath.Join(dir, "evil.tar.gz")
	os.WriteFile(arc, makeTarGz(t, map[string]string{"../escape.txt": "x"}), 0o644)
	dst := filepath.Join(dir, "out")
	if err := extractReplace(arc, "evil.tar.gz", dst); err == nil {
		t.Fatal("expected error")
	}
	if _, err := os.Stat(filepath.Join(dir, "escape.txt")); err == nil {
		t.Fatal("escaped the target directory")
	}
	if _, err := os.Stat(dst); err == nil {
		t.Fatal("target should not be created on failure")
	}
}

func TestMatchAsset(t *testing.T) {
	rel := &Release{Assets: []ReleaseAsset{{Name: "a-1.jar"}, {Name: "b.tar.gz"}}}
	if a, err := matchAsset(rel, "a-*.jar"); err != nil || a.Name != "a-1.jar" {
		t.Fatal("glob should match")
	}
	if _, err := matchAsset(rel, "nope"); err == nil {
		t.Fatal("expected no match")
	}
	if _, err := matchAsset(rel, "*"); err == nil {
		t.Fatal("expected ambiguity")
	}
}

func TestLoadConfigValidation(t *testing.T) {
	write := func(s string) string {
		p := filepath.Join(t.TempDir(), "c.yaml")
		os.WriteFile(p, []byte(s), 0o600)
		return p
	}
	if _, err := loadConfig(write("token: x\nprojects:\n  - repo: bad\n    assets: [{name: a, copy_to: /x}]\n")); err == nil {
		t.Fatal("bad repo accepted")
	}
	if _, err := loadConfig(write("token: x\nprojects:\n  - repo: o/r\n    assets: [{name: a}]\n")); err == nil {
		t.Fatal("asset without target accepted")
	}
	if _, err := loadConfig(write("token: x\ntypo: 1\nprojects:\n  - repo: o/r\n    assets: [{name: a, copy_to: /x}]\n")); err == nil {
		t.Fatal("unknown key accepted")
	}
	c, err := loadConfig(write("token: x\ninterval: 30s\nprojects:\n  - repo: o/r\n    assets: [{name: a, copy_to: /x, mode: '0755'}]\n"))
	if err != nil || c.Interval != 30*time.Second || c.Projects[0].Assets[0].mode != 0o755 {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestPartialReleases(t *testing.T) {
	dir := t.TempDir()
	web := makeTarGz(t, map[string]string{"index.html": "w"})
	jar := []byte("jar")
	// newest first, as GitHub returns them: 3 carries only web, 2 only the jar, 1 is already deployed
	releases := `[
	 {"id":3,"tag_name":"v3","body":"- web change","assets":[{"id":30,"name":"web.tar.gz","size":%d,"state":"uploaded"}]},
	 {"id":2,"tag_name":"v2","body":"- api change","assets":[{"id":20,"name":"app.jar","size":%d,"state":"uploaded"}]},
	 {"id":1,"tag_name":"v1","assets":[]}]`
	mux := http.NewServeMux()
	var latest atomic.Int64
	latest.Store(3)
	mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if latest.Load() == 3 {
			fmt.Fprintf(w, `{"id":3,"tag_name":"v3","assets":[{"id":30,"name":"web.tar.gz","size":%d,"state":"uploaded"}]}`, len(web))
		} else {
			fmt.Fprintf(w, `{"id":4,"tag_name":"v4","assets":[{"id":40,"name":"web.tar.gz","size":%d,"state":"uploaded"}]}`, len(web))
		}
	})
	mux.HandleFunc("/repos/o/r/releases", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, releases, len(web), len(jar))
	})
	mux.HandleFunc("/repos/o/r/releases/assets/", func(w http.ResponseWriter, r *http.Request) {
		body := web
		if strings.HasSuffix(r.URL.Path, "/20") {
			body = jar
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	log := filepath.Join(dir, "restart.log")
	cfg := &Config{
		Token: "tok", APIURL: srv.URL, Interval: time.Minute,
		StateFile: filepath.Join(dir, "state.json"),
		Projects: []Project{{
			Name: "t", Repo: "o/r", Partial: true,
			Assets: []Asset{
				{Name: "web.tar.gz", ExtractTo: filepath.Join(dir, "www")},
				{Name: "app.jar", CopyTo: filepath.Join(dir, "app.jar"), Restart: "echo api >> " + log},
			},
		}},
	}
	app, err := newApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	app.state.Projects["o/r"] = projectState{ReleaseID: 1, Tag: "v1"}

	if n := app.checkAll(context.Background()); n != 0 {
		t.Fatal("pass failed")
	}
	read := func(p string) string { b, _ := os.ReadFile(filepath.Join(dir, p)); return string(b) }
	if read("www/index.html") != "w" || read("app.jar") != "jar" {
		t.Fatal("assets from both pending releases must be deployed")
	}
	if strings.Count(read("restart.log"), "api") != 1 {
		t.Fatal("jar restart should run exactly once")
	}

	// a web-only release must not restart the backend
	latest.Store(4)
	app.state.Projects["o/r"] = projectState{ReleaseID: 3, Tag: "v3"}
	releases = `[{"id":4,"tag_name":"v4","assets":[{"id":40,"name":"web.tar.gz","size":%[1]d,"state":"uploaded"}]}]`
	app.etags = map[string]string{}
	if n := app.checkAll(context.Background()); n != 0 {
		t.Fatal("second pass failed")
	}
	if strings.Count(read("restart.log"), "api") != 1 {
		t.Fatal("backend restarted although only web changed")
	}
}
