package main

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeSMTP is a minimal plaintext SMTP server that records envelope recipients and the DATA body.
func fakeSMTP(t *testing.T) (port int, got chan []string) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = make(chan []string, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				r := bufio.NewReader(conn)
				say := func(s string) { conn.Write([]byte(s + "\r\n")) }
				say("220 fake ESMTP")
				var rec []string
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					line = strings.TrimSpace(line)
					switch cmd := strings.ToUpper(line); {
					case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
						say("250 fake")
					case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
						rec = append(rec, line)
						say("250 ok")
					case cmd == "DATA":
						say("354 go")
						var body []string
						for {
							l, _ := r.ReadString('\n')
							if strings.TrimSpace(l) == "." {
								break
							}
							body = append(body, strings.TrimRight(l, "\r\n"))
						}
						rec = append(rec, strings.Join(body, "\n"))
						say("250 queued")
					case cmd == "QUIT":
						say("221 bye")
						got <- rec
						return
					default:
						say("250 ok")
					}
				}
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, got
}

func TestEmailMultipleRecipients(t *testing.T) {
	port, got := fakeSMTP(t)
	nc := &NotifyConfig{Email: &EmailConfig{
		Host: "127.0.0.1", Port: port, Security: "none",
		From: "Bastion <bot@example.com>", To: StringList{"a@example.com", "Bob <b@example.com>"},
	}}
	if err := nc.validate(); err != nil {
		t.Fatal(err)
	}
	if n := nc.sendTest(context.Background()); n != 0 {
		t.Fatalf("%d failures", n)
	}
	select {
	case rec := <-got:
		all := strings.Join(rec, "\n")
		for _, want := range []string{"MAIL FROM:<bot@example.com>", "RCPT TO:<a@example.com>", "RCPT TO:<b@example.com>",
			"To: a@example.com, Bob <b@example.com>", "Subject: =?UTF-8?b?"} {
			if !strings.Contains(all, want) {
				t.Errorf("missing %q in:\n%s", want, all)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no mail received")
	}
}

func TestEmailValidation(t *testing.T) {
	for name, e := range map[string]EmailConfig{
		"no host":    {From: "a@b.c", To: StringList{"x@y.z"}},
		"no to":      {Host: "h", From: "a@b.c"},
		"bad from":   {Host: "h", From: "nope", To: StringList{"x@y.z"}},
		"bad to":     {Host: "h", From: "a@b.c", To: StringList{"nope"}},
		"bad secure": {Host: "h", From: "a@b.c", To: StringList{"x@y.z"}, Security: "tls1"},
	} {
		if err := e.validate(); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	e := EmailConfig{Host: "h", Username: "a@b.c", To: StringList{"x@y.z"}, Port: 465}
	if err := e.validate(); err != nil || e.Security != "ssl" || e.From != "a@b.c" {
		t.Errorf("defaults wrong: %v %+v", err, e)
	}
}
