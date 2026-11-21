// Package smtp delivers mail over SMTP with mandatory, verified TLS.
package smtp

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"net/textproto"
	"sort"
	"time"

	"github.com/xnqOS/go-starter-kit/kit/email"
)

// Config is the SMTP connection. The sender address comes from each message.
type Config struct {
	Address  string // host:port
	Username string
	Password string `json:"-"`
	TLSMode  string // "starttls" (default) or "tls"
}

type Provider struct {
	config Config
	// Tests use a private CA. Production always verifies the configured host.
	tlsConfig *tls.Config
}

func New(c Config) *Provider { return &Provider{config: c} }

func (*Provider) Name() string { return "smtp" }

func (p *Provider) Send(ctx context.Context, m email.Outbound) (result email.Result, err error) {
	if m.From.Address == "" {
		return result, email.Failed(email.Permanent, "invalid_sender", errors.New("invalid sender address"))
	}
	if m.To.Address == "" || m.ValidateHeaders() != nil {
		return result, email.Failed(email.Permanent, "invalid_headers", errors.New("invalid email headers"))
	}
	message, err := buildMessage(m)
	if err != nil {
		return result, email.Failed(email.Permanent, "invalid_headers", err)
	}
	ctx, cancel := context.WithTimeout(ctx, email.SendTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	// dataSent is set once message bytes started going to the server; a
	// failure after that may mean the server kept the message.
	dataSent := false
	defer func() {
		if err != nil {
			if ctx.Err() != nil {
				err = ctx.Err()
			} else {
				// The socket deadline can fire before the context's timer
				// callback runs. Keep the timeout result independent of that race.
				var timeout net.Error
				if errors.As(err, &timeout) && timeout.Timeout() && !time.Now().Before(deadline) {
					err = context.DeadlineExceeded
				}
			}
			err = classify(err, dataSent)
		}
	}()
	host, _, err := net.SplitHostPort(p.config.Address)
	if err != nil {
		return result, errors.New("invalid SMTP address")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", p.config.Address)
	if err != nil {
		return result, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := conn.SetDeadline(deadline); err != nil {
		return result, err
	}
	tlsConfig := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	if p.tlsConfig != nil {
		tlsConfig = p.tlsConfig.Clone()
	}
	var transport net.Conn = conn
	if p.config.TLSMode == "tls" {
		secure := tls.Client(conn, tlsConfig)
		if err := secure.HandshakeContext(ctx); err != nil {
			return result, err
		}
		transport = secure
	}
	client, err := smtp.NewClient(transport, host)
	if err != nil {
		return result, err
	}
	defer client.Close()
	if p.config.TLSMode != "tls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return result, errors.New("SMTP server must support STARTTLS")
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			return result, err
		}
	}
	if p.config.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", p.config.Username, p.config.Password, host)); err != nil {
			return result, err
		}
	}
	if err := client.Mail(m.From.Address); err != nil {
		return result, err
	}
	if err := client.Rcpt(m.To.Address); err != nil {
		return result, err
	}
	w, err := client.Data()
	if err != nil {
		return result, err
	}
	dataSent = true
	if _, err := w.Write(message); err != nil {
		return result, err
	}
	if err := w.Close(); err != nil {
		return result, err
	}
	// The server accepted the message with its reply to DATA; a failed QUIT
	// does not undo that.
	_ = client.Quit()
	return result, nil
}

// classify maps SMTP failures: reply codes 4xx are Retryable and 5xx
// Permanent; anything else is Retryable before the message was sent and
// Ambiguous after.
func classify(err error, dataSent bool) error {
	var reply *textproto.Error
	if errors.As(err, &reply) {
		code := fmt.Sprintf("smtp_%d", reply.Code)
		if reply.Code >= 500 {
			return email.Failed(email.Permanent, code, err)
		}
		return email.Failed(email.Retryable, code, err)
	}
	code := "network"
	if errors.Is(err, context.DeadlineExceeded) {
		code = "timeout"
	} else if errors.Is(err, context.Canceled) {
		code = "canceled"
	}
	if dataSent {
		return email.Failed(email.Ambiguous, code, err)
	}
	return email.Failed(email.Retryable, code, err)
}

// buildMessage writes the RFC 5322 message. A text-only message without extra
// headers is byte-for-byte what the SMTP sender produced before adapters.
func buildMessage(m email.Outbound) ([]byte, error) {
	var message bytes.Buffer
	fmt.Fprintf(&message, "From: %s\r\nTo: %s\r\n", m.From.String(), m.To.String())
	if m.ReplyTo != nil {
		fmt.Fprintf(&message, "Reply-To: %s\r\n", m.ReplyTo.String())
	}
	fmt.Fprintf(&message, "Subject: %s\r\n", mime.QEncoding.Encode("UTF-8", m.Subject))
	names := make([]string, 0, len(m.Headers))
	for name := range m.Headers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		switch textproto.CanonicalMIMEHeaderKey(name) {
		case "From", "To", "Cc", "Bcc", "Reply-To", "Subject", "Mime-Version", "Content-Type", "Content-Transfer-Encoding":
			return nil, fmt.Errorf("header %s is set by the mailer", name)
		}
		fmt.Fprintf(&message, "%s: %s\r\n", name, mime.QEncoding.Encode("UTF-8", m.Headers[name]))
	}
	message.WriteString("MIME-Version: 1.0\r\n")
	if m.HTML == "" {
		message.WriteString("Content-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
		if err := writeQuotedPrintable(&message, m.Text); err != nil {
			return nil, err
		}
		return message.Bytes(), nil
	}
	parts := multipart.NewWriter(&message)
	fmt.Fprintf(&message, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", parts.Boundary())
	for _, part := range []struct{ contentType, body string }{{"text/plain", m.Text}, {"text/html", m.HTML}} {
		w, err := parts.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {part.contentType + "; charset=UTF-8"},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return nil, err
		}
		if err := writeQuotedPrintable(w, part.body); err != nil {
			return nil, err
		}
	}
	if err := parts.Close(); err != nil {
		return nil, err
	}
	return message.Bytes(), nil
}

func writeQuotedPrintable(w io.Writer, body string) error {
	encoded := quotedprintable.NewWriter(w)
	if _, err := encoded.Write([]byte(body)); err != nil {
		return err
	}
	return encoded.Close()
}
