package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"
)

// EmailConfig sends notifications over SMTP. One message is sent, addressed to every recipient in To.
type EmailConfig struct {
	Host     string     `yaml:"host"`
	Port     int        `yaml:"port"`     // default: 465 for ssl, 587 for starttls, 25 for none
	Security string     `yaml:"security"` // ssl | starttls | none; default ssl on port 465, otherwise starttls
	Username string     `yaml:"username"` // empty = no authentication
	Password string     `yaml:"password"` // or env SMTP_PASSWORD
	From     string     `yaml:"from"`     // "addr" or "Name <addr>"; default: username
	To       StringList `yaml:"to"`       // one or more recipients (comma separated or a list)

	fromAddr string
	rcpts    []string
}

func (e *EmailConfig) validate() error {
	if e.Host == "" {
		return fmt.Errorf("notify.email.host is required")
	}
	if e.Password == "" {
		e.Password = os.Getenv("SMTP_PASSWORD")
	}
	if e.From == "" {
		e.From = e.Username
	}
	from, err := mail.ParseAddress(e.From)
	if err != nil {
		return fmt.Errorf("notify.email.from: %w", err)
	}
	e.fromAddr = from.Address
	if len(e.To) == 0 {
		return fmt.Errorf("notify.email.to needs at least one recipient")
	}
	e.rcpts = nil
	for i, t := range e.To {
		a, err := mail.ParseAddress(t)
		if err != nil {
			return fmt.Errorf("notify.email.to[%d]: %w", i, err)
		}
		e.rcpts = append(e.rcpts, a.Address)
	}
	e.Security = strings.ToLower(e.Security)
	if e.Security == "" {
		if e.Port == 465 {
			e.Security = "ssl"
		} else {
			e.Security = "starttls"
		}
	}
	defPort := map[string]int{"ssl": 465, "starttls": 587, "none": 25}
	if _, ok := defPort[e.Security]; !ok {
		return fmt.Errorf("notify.email.security must be ssl, starttls or none, got %q", e.Security)
	}
	if e.Port == 0 {
		e.Port = defPort[e.Security]
	}
	return nil
}

// sendAll sends the message (with retries) and returns the number of failures (0 or 1).
func (e *EmailConfig) sendAll(ctx context.Context, title, htmlBody string) (failed int) {
	label := "email " + strings.Join(e.rcpts, ",")
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		if err = e.send(ctx, title, htmlBody); err == nil || ctx.Err() != nil {
			break
		}
		select {
		case <-time.After(time.Duration(attempt) * retryDelay):
		case <-ctx.Done():
		}
	}
	if err != nil {
		slog.Warn("notification failed", "target", label, "error", err)
		return 1
	}
	slog.Info("notification sent", "target", label)
	return 0
}

func (e *EmailConfig) send(ctx context.Context, title, htmlBody string) error {
	addr := net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
	conn, err := (&net.Dialer{Timeout: 20 * time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(60 * time.Second))
	tlsCfg := &tls.Config{ServerName: e.Host}
	if e.Security == "ssl" {
		conn = tls.Client(conn, tlsCfg)
	}
	c, err := smtp.NewClient(conn, e.Host)
	if err != nil {
		return err
	}
	defer c.Close()
	if e.Security == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("server does not support STARTTLS (use security: ssl or none)")
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return err
		}
	}
	if e.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", e.Username, e.Password, e.Host)); err != nil {
			return err
		}
	}
	if err := c.Mail(e.fromAddr); err != nil {
		return err
	}
	for _, r := range e.rcpts {
		if err := c.Rcpt(r); err != nil {
			return fmt.Errorf("recipient %s: %w", r, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(e.message(title, htmlBody)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func (e *EmailConfig) message(title, htmlBody string) []byte {
	var b strings.Builder
	h := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	h("From", e.From)
	h("To", strings.Join(e.To, ", "))
	h("Subject", mime.BEncoding.Encode("UTF-8", title))
	h("Date", time.Now().Format(time.RFC1123Z))
	h("MIME-Version", "1.0")
	h("Content-Type", `text/html; charset="UTF-8"`)
	h("Content-Transfer-Encoding", "base64")
	b.WriteString("\r\n")
	enc := base64.StdEncoding.EncodeToString([]byte(htmlBody))
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc + "\r\n")
	return []byte(b.String())
}
