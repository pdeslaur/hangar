// Command smtp-relay takes email over SMTP, without authentication, and sends
// it through cloudflare-sts with the workload's JWT-SVID, for an app that
// only speaks SMTP.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/chainguard-dev/clog"
	"github.com/emersion/go-smtp"
	"github.com/pdeslaur/hangar/internal/cloudflarests"
	"github.com/sethvargo/go-envconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

type config struct {
	// Addr is where it listens. It defaults to loopback, so that only its
	// pod reaches it: anything that does sends as the profile.
	Addr string `env:"ADDR,default=127.0.0.1:2525"`
	// BrokerURL is cloudflare-sts's, and the audience of the JWT-SVIDs it
	// sends with.
	BrokerURL string `env:"BROKER_URL,required"`
	// Profile is the broker's profile it sends as.
	Profile string `env:"PROFILE,required"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg := envconfig.MustProcess(ctx, &config{})
	if err := run(ctx, cfg); err != nil {
		clog.ErrorContext(ctx, "serving", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg *config) error {
	// The Workload API's address comes from SPIFFE_ENDPOINT_SOCKET.
	svids, err := workloadapi.New(ctx)
	if err != nil {
		return err
	}
	defer svids.Close()
	broker := &cloudflarests.Client{URL: cfg.BrokerURL, SVIDs: svids, HTTP: cloudflarests.NewHTTPClient()}

	lis, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	srv := newServer(ctx, broker, cfg.Profile)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	clog.InfoContext(ctx, "listening", "addr", cfg.Addr, "profile", cfg.Profile)
	if err := srv.Serve(lis); !errors.Is(err, smtp.ErrServerClosed) {
		return err
	}
	return nil
}

// The broker's limits, which are Cloudflare's.
const (
	maxRecipients   = 50
	maxMessageBytes = 25 << 20
)

func newServer(ctx context.Context, broker *cloudflarests.Client, profile string) *smtp.Server {
	srv := smtp.NewServer(smtp.BackendFunc(func(*smtp.Conn) (smtp.Session, error) {
		return &session{ctx: ctx, broker: broker, profile: profile}, nil
	}))
	srv.Domain = "localhost"
	srv.MaxRecipients = maxRecipients
	srv.MaxMessageBytes = maxMessageBytes
	srv.ReadTimeout = time.Minute
	srv.WriteTimeout = time.Minute
	return srv
}

// session sends each message of an SMTP session through the broker, and
// answers only once the broker has, so that the app's retries keep working:
// it keeps no queue.
type session struct {
	ctx     context.Context
	broker  *cloudflarests.Client
	profile string

	from string
	to   []string
}

func (s *session) Mail(from string, _ *smtp.MailOptions) error {
	s.from = from
	return nil
}

func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	s.to = append(s.to, to)
	return nil
}

func (s *session) Data(r io.Reader) error {
	// Read it whole, so that it's consumed even when no JWT-SVID can be
	// fetched to send it with.
	message, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	ids, err := s.broker.SendEmail(s.ctx, s.profile, s.from, s.to, bytes.NewReader(message))
	if err != nil {
		clog.WarnContext(s.ctx, "sending", "from", s.from, "to", s.to, "error", err)
		return reply(err)
	}
	clog.InfoContext(s.ctx, "sent", "from", s.from, "to", s.to, "message_ids", ids)
	return nil
}

// reply is the SMTP reply for the broker's failure to send: one the client
// retries when retrying can help, or one it gives up on.
func reply(err error) *smtp.SMTPError {
	if cloudflarests.Temporary(err) {
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: fmt.Sprintf("Not sent, try again later: %v", err)}
	}
	return &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 0, 0}, Message: fmt.Sprintf("Not sent: %v", err)}
}

func (s *session) Reset() {
	s.from, s.to = "", nil
}

func (s *session) Logout() error {
	return nil
}
