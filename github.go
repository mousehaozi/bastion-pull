package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	maxAttempts = 8
	idleTimeout = 60 * time.Second
)

// retryDelay is a variable so tests can shorten it.
var retryDelay = 3 * time.Second

type Release struct {
	ID      int64          `json:"id"`
	TagName string         `json:"tag_name"`
	Name    string         `json:"name"`
	Body    string         `json:"body"`
	HTMLURL string         `json:"html_url"`
	Assets  []ReleaseAsset `json:"assets"`
}

type ReleaseAsset struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	State  string `json:"state"`
	Digest string `json:"digest"`
}

type Client struct {
	base  string
	token string
	http  *http.Client
}

func newClient(base, token string) *Client {
	// Clone keeps ProxyFromEnvironment, so HTTPS_PROXY works.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 60 * time.Second
	return &Client{base: base, token: token, http: &http.Client{Transport: tr}}
}

func (c *Client) newRequest(ctx context.Context, url, accept string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "bastion-pull/"+version)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return req, nil
}

// latest returns the latest published release. When etag matches, notModified is true
// (GitHub answers 304 and this does not count against the rate limit).
func (c *Client) latest(ctx context.Context, repo, etag string) (rel *Release, newETag string, notModified bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := c.newRequest(ctx, fmt.Sprintf("%s/repos/%s/releases/latest", c.base, repo), "application/vnd.github+json")
	if err != nil {
		return nil, "", false, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil, etag, true, nil
	case http.StatusOK:
		var r Release
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			return nil, "", false, fmt.Errorf("decode release: %w", err)
		}
		return &r, resp.Header.Get("ETag"), false, nil
	case http.StatusNotFound:
		return nil, "", false, errors.New("404: no published release, or the token cannot see this repository")
	case http.StatusUnauthorized:
		return nil, "", false, errors.New("401: token is invalid or expired")
	default:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, "", false, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
}

type fatalError struct{ err error }

func (f *fatalError) Error() string { return f.err.Error() }
func (f *fatalError) Unwrap() error { return f.err }

// download fetches an asset to dst with retries and resume (HTTP Range).
func (c *Client) download(ctx context.Context, repo string, a ReleaseAsset, dst string) error {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err := c.downloadOnce(ctx, repo, a, dst)
		if err == nil {
			return nil
		}
		lastErr = err
		var fatal *fatalError
		if ctx.Err() != nil || errors.As(err, &fatal) {
			return err
		}
		slog.Warn("download interrupted, will retry", "asset", a.Name, "attempt", attempt, "error", err)
		select {
		case <-time.After(time.Duration(attempt) * retryDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("download %s failed after %d attempts: %w", a.Name, maxAttempts, lastErr)
}

type idleReader struct {
	r io.Reader
	t *time.Timer
}

func (i *idleReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	i.t.Reset(idleTimeout)
	return n, err
}

func (c *Client) downloadOnce(ctx context.Context, repo string, a ReleaseAsset, dst string) error {
	part := dst + ".part"
	var have int64
	if fi, err := os.Stat(part); err == nil {
		have = fi.Size()
	}
	if have > a.Size {
		os.Remove(part)
		have = 0
	}

	if have < a.Size || a.Size == 0 {
		ctx2, cancel := context.WithCancel(ctx)
		defer cancel()
		req, err := c.newRequest(ctx2, fmt.Sprintf("%s/repos/%s/releases/assets/%d", c.base, repo, a.ID), "application/octet-stream")
		if err != nil {
			return err
		}
		if have > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		flags := os.O_CREATE | os.O_WRONLY
		switch resp.StatusCode {
		case http.StatusOK:
			flags |= os.O_TRUNC // server ignored Range (or fresh start): rewrite from zero
		case http.StatusPartialContent:
			flags |= os.O_APPEND
		case http.StatusRequestedRangeNotSatisfiable:
			os.Remove(part)
			return errors.New("range not satisfiable, restarting from zero")
		case http.StatusUnauthorized, http.StatusNotFound:
			return &fatalError{fmt.Errorf("download %s: status %d (check token permissions: Contents read)", a.Name, resp.StatusCode)}
		default:
			return fmt.Errorf("download %s: unexpected status %d", a.Name, resp.StatusCode)
		}

		f, err := os.OpenFile(part, flags, 0o644)
		if err != nil {
			return &fatalError{err}
		}
		timer := time.AfterFunc(idleTimeout, cancel)
		_, copyErr := io.Copy(f, &idleReader{r: resp.Body, t: timer})
		timer.Stop()
		if cerr := f.Close(); copyErr == nil {
			copyErr = cerr
		}
		if copyErr != nil {
			return copyErr
		}
	}

	fi, err := os.Stat(part)
	if err != nil {
		return err
	}
	if a.Size > 0 && fi.Size() != a.Size {
		return fmt.Errorf("incomplete download: have %d of %d bytes", fi.Size(), a.Size)
	}
	if want, ok := strings.CutPrefix(a.Digest, "sha256:"); ok {
		got, err := fileSHA256(part)
		if err != nil {
			return err
		}
		if got != want {
			os.Remove(part)
			return fmt.Errorf("sha256 mismatch for %s", a.Name)
		}
	}
	return os.Rename(part, dst)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
