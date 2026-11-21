package smtp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/xnqOS/go-starter-kit/kit/email"
)

type smtpResult struct {
	message *mail.Message
	body    string
	err     error
	sent    bool
}

func testCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func mockSMTP(t *testing.T, implicit, offerTLS bool) (string, *tls.Config, <-chan smtpResult) {
	t.Helper()
	cert, pool := testCertificate(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	result := make(chan smtpResult, 1)
	go func() {
		out := smtpResult{}
		defer func() { result <- out }()
		conn, err := listener.Accept()
		if err != nil {
			out.err = err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		secure := implicit
		serverTLS := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		if implicit {
			conn = tls.Server(conn, serverTLS)
		}
		reader := bufio.NewReader(conn)
		write := func(text string) error { _, err := io.WriteString(conn, text); return err }
		if out.err = write("220 localhost SMTP\r\n"); out.err != nil {
			return
		}
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if err != io.EOF {
					out.err = err
				}
				return
			}
			line = strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(line, "EHLO "):
				response := "250-localhost\r\n"
				if !secure && offerTLS {
					response += "250-STARTTLS\r\n"
				}
				out.err = write(response + "250 AUTH PLAIN\r\n")
			case line == "STARTTLS":
				if out.err = write("220 start TLS\r\n"); out.err != nil {
					return
				}
				tlsConn := tls.Server(conn, serverTLS)
				if out.err = tlsConn.Handshake(); out.err != nil {
					return
				}
				conn = tlsConn
				reader = bufio.NewReader(conn)
				secure = true
			case strings.HasPrefix(line, "AUTH PLAIN "):
				decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "AUTH PLAIN "))
				if err != nil || !secure || string(decoded) != "\x00test-user\x00test-pass" {
					out.err = errors.New("invalid or insecure AUTH")
					return
				}
				out.err = write("235 authenticated\r\n")
			case strings.HasPrefix(line, "MAIL FROM:") || strings.HasPrefix(line, "RCPT TO:"):
				if !secure {
					out.err = errors.New("message sent without TLS")
					return
				}
				out.err = write("250 ok\r\n")
			case line == "DATA":
				if out.err = write("354 send data\r\n"); out.err != nil {
					return
				}
				data, err := textproto.NewReader(reader).ReadDotBytes()
				if err != nil {
					out.err = err
					return
				}
				out.message, err = mail.ReadMessage(bytes.NewReader(data))
				if err != nil {
					out.err = err
					return
				}
				body, err := io.ReadAll(quotedprintable.NewReader(out.message.Body))
				if err != nil {
					out.err = err
					return
				}
				out.body = string(body)
				out.sent = true
				out.err = write("250 queued\r\n")
			case line == "QUIT":
				out.err = write("221 goodbye\r\n")
				return
			default:
				out.err = fmt.Errorf("unexpected SMTP command")
				return
			}
			if out.err != nil {
				return
			}
		}
	}()
	return listener.Addr().String(), &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}, result
}

func TestSMTPDeliveryRequiresVerifiedTLS(t *testing.T) {
	for _, test := range []struct {
		name, mode                     string
		offerTLS, untrusted, wantError bool
	}{
		{"STARTTLS", "starttls", true, false, false}, {"implicit TLS", "tls", false, false, false},
		{"no downgrade", "starttls", false, false, true}, {"untrusted certificate", "starttls", true, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			address, tlsConfig, result := mockSMTP(t, test.mode == "tls", test.offerTLS)
			if test.untrusted {
				tlsConfig = &tls.Config{ServerName: "127.0.0.1", RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS12}
			}
			sender := &Provider{config: Config{Address: address, Username: "test-user", Password: "test-pass", TLSMode: test.mode}, tlsConfig: tlsConfig}
			_, err := sender.Send(context.Background(), email.Outbound{From: mail.Address{Name: "Shop", Address: "sender@example.com"}, To: mail.Address{Address: "buyer@example.com"}, Subject: "Email verification", Text: "Hello 世界\nToken: a-secret-token\n.line beginning with dot"})
			if (err != nil) != test.wantError {
				t.Fatalf("delivery err=%v", err)
			}
			select {
			case received := <-result:
				if !test.wantError && (received.err != nil || !received.sent || received.message.Header.Get("To") != "<buyer@example.com>" || !strings.Contains(received.body, "Hello 世界") || !strings.Contains(received.body, "Token: a-secret-token")) {
					t.Fatalf("incorrect SMTP delivery: %+v", received)
				}
				if test.wantError && received.sent {
					t.Fatal("sent a message over an untrusted connection")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("SMTP did not finish")
			}
		})
	}
}

func TestSMTPRejectsHeaderInjectionAndHonorsCancellation(t *testing.T) {
	sender := &Provider{config: Config{Address: "127.0.0.1:1"}}
	from := mail.Address{Address: "sender@example.com"}
	for _, m := range []email.Outbound{
		{From: from, To: mail.Address{Address: "buyer@example.com\r\nBcc:other@example.com"}, Subject: "hello"},
		{From: from, To: mail.Address{Address: "buyer@example.com"}, Subject: "hello\r\nBcc:other@example.com"},
		{From: from, To: mail.Address{Address: "buyer@example.com"}, ReplyTo: &mail.Address{Name: "x\nBcc: other@example.com", Address: "r@example.com"}, Subject: "hello"},
		{From: from, To: mail.Address{Address: "buyer@example.com"}, Subject: "hello", Headers: map[string]string{"List-Unsubscribe": "<x>\r\nBcc: other@example.com"}},
	} {
		if _, err := sender.Send(context.Background(), m); err == nil || !strings.Contains(err.Error(), "headers") {
			t.Fatalf("unsafe headers accepted: %v", err)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		_, _ = conn.Read(make([]byte, 1))
	}()
	sender.config.Address = listener.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := sender.Send(ctx, email.Outbound{From: from, To: mail.Address{Address: "buyer@example.com"}, Subject: "hello"}); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("SMTP ignored context: %v", err)
	}
	<-done
}

func TestTextOnlyMessageIsUnchanged(t *testing.T) {
	got, err := buildMessage(email.Outbound{
		From: mail.Address{Name: "Example App", Address: "no-reply@example.com"}, To: mail.Address{Address: "buyer@example.com"},
		Subject: "Verify your email", Text: "Token: abc\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "From: \"Example App\" <no-reply@example.com>\r\nTo: <buyer@example.com>\r\nSubject: Verify your email\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nToken: abc\r\n"
	if string(got) != want {
		t.Fatalf("text-only message changed:\n got %q\nwant %q", got, want)
	}
}

func TestHTMLMessageIsMultipartAlternative(t *testing.T) {
	raw, err := buildMessage(email.Outbound{
		From: mail.Address{Address: "no-reply@example.com"}, To: mail.Address{Address: "buyer@example.com"},
		ReplyTo: &mail.Address{Name: "茶小铺", Address: "owner@example.com"}, Subject: "Hello",
		Text: "plain 世界", HTML: "<p>html 世界</p>", Headers: map[string]string{"List-Unsubscribe": "<https://example.com/u>"},
	})
	if err != nil {
		t.Fatal(err)
	}
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if replyTo, err := message.Header.AddressList("Reply-To"); err != nil || replyTo[0].Address != "owner@example.com" || replyTo[0].Name != "茶小铺" {
		t.Fatalf("Reply-To = %v, %v", replyTo, err)
	}
	if message.Header.Get("List-Unsubscribe") != "<https://example.com/u>" {
		t.Fatal("extra header missing")
	}
	mediaType, params, err := mime.ParseMediaType(message.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/alternative" {
		t.Fatalf("content type %q: %v", mediaType, err)
	}
	reader := multipart.NewReader(message.Body, params["boundary"])
	var bodies []string
	for {
		part, err := reader.NextPart() // decodes quoted-printable
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(part)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, part.Header.Get("Content-Type")+"|"+string(body))
	}
	if len(bodies) != 2 || bodies[0] != "text/plain; charset=UTF-8|plain 世界" || bodies[1] != "text/html; charset=UTF-8|<p>html 世界</p>" {
		t.Fatalf("parts = %q", bodies)
	}
	if _, err := buildMessage(email.Outbound{Headers: map[string]string{"bcc": "x@example.com"}}); err == nil {
		t.Fatal("extra headers may override core headers")
	}
}

func TestClassifiesSMTPFailures(t *testing.T) {
	for _, test := range []struct {
		err      error
		dataSent bool
		want     email.Class
		code     string
	}{
		{&textproto.Error{Code: 550, Msg: "no such user"}, false, email.Permanent, "smtp_550"},
		{&textproto.Error{Code: 554, Msg: "rejected"}, true, email.Permanent, "smtp_554"},
		{&textproto.Error{Code: 421, Msg: "try later"}, false, email.Retryable, "smtp_421"},
		{&textproto.Error{Code: 451, Msg: "try later"}, true, email.Retryable, "smtp_451"},
		{errors.New("connection refused"), false, email.Retryable, "network"},
		{errors.New("connection reset"), true, email.Ambiguous, "network"},
		{context.DeadlineExceeded, false, email.Retryable, "timeout"},
		{context.DeadlineExceeded, true, email.Ambiguous, "timeout"},
	} {
		err := classify(test.err, test.dataSent)
		if email.Classify(err) != test.want || email.Code(err) != test.code || !errors.Is(err, test.err) {
			t.Errorf("%v (data sent %v): %s %q", test.err, test.dataSent, email.Classify(err), email.Code(err))
		}
	}
	// Refused before connecting: nothing was sent.
	_, err := New(Config{Address: "127.0.0.1:1"}).Send(context.Background(), email.Outbound{From: mail.Address{Address: "a@example.com"}, To: mail.Address{Address: "b@example.com"}, Subject: "s"})
	if email.Classify(err) != email.Retryable {
		t.Fatalf("refused connection: %v", err)
	}
}
