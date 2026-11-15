// Command spiffe-oidc-publisher publishes a SPIFFE trust domain's JWT keys as
// an OIDC issuer's static documents, a discovery document and a JWKS, to an
// S3-compatible bucket served on the issuer's URL. It watches the Workload
// API, writing the documents whenever the keys change, and again every
// REFRESH, so relying parties like GCP's STS verify JWT-SVIDs with no service
// of the trust domain reachable from the internet.
package main

import (
	"bytes"
	"context"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/chainguard-dev/clog"
	"github.com/pdeslaur/hangar/internal/oidcpublish"
	"github.com/pdeslaur/hangar/internal/systemd"
	"github.com/sethvargo/go-envconfig"
	"github.com/spiffe/go-spiffe/v2/bundle/jwtbundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

type config struct {
	// TrustDomain is the trust domain whose JWT keys are published.
	TrustDomain string `env:"TRUST_DOMAIN,required"`
	// Issuer is the issuer's URL, which the bucket is served on.
	Issuer string `env:"ISSUER,required"`
	// Endpoint is the bucket's S3 endpoint, like R2's
	// https://<account>.r2.cloudflarestorage.com. The credentials come from
	// AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY.
	Endpoint string `env:"ENDPOINT,required"`
	Region   string `env:"REGION,default=auto"`
	Bucket   string `env:"BUCKET,required"`
	// Refresh is how often the documents are written again, changed or not.
	Refresh time.Duration `env:"REFRESH,default=1h"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg := envconfig.MustProcess(ctx, &config{})
	if err := run(ctx, cfg); err != nil {
		clog.ErrorContext(ctx, "publishing", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg *config) error {
	td, err := spiffeid.TrustDomainFromString(cfg.TrustDomain)
	if err != nil {
		return err
	}
	store, err := newBucket(ctx, cfg)
	if err != nil {
		return err
	}
	p := &oidcpublish.Publisher{Issuer: strings.TrimSuffix(cfg.Issuer, "/"), Store: store}

	// The Workload API's address comes from SPIFFE_ENDPOINT_SOCKET.
	client, err := workloadapi.New(ctx)
	if err != nil {
		return err
	}
	defer client.Close()

	go func() {
		ticker := time.NewTicker(cfg.Refresh)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := p.Refresh(ctx); err != nil {
					clog.ErrorContext(ctx, "refreshing", "error", err)
				}
			}
		}
	}()

	clog.InfoContext(ctx, "watching", "trust_domain", td.String(), "issuer", p.Issuer, "bucket", cfg.Bucket)
	err = client.WatchJWTBundles(ctx, &watcher{ctx: ctx, td: td, publisher: p})
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// watcher publishes each bundle the Workload API sends. A failed write is
// tried again with the next bundle, or at the next refresh.
type watcher struct {
	ctx       context.Context
	td        spiffeid.TrustDomain
	publisher *oidcpublish.Publisher
	ready     sync.Once
}

func (w *watcher) OnJWTBundlesUpdate(set *jwtbundle.Set) {
	bundle, ok := set.Get(w.td)
	if !ok {
		clog.ErrorContext(w.ctx, "no bundle for the trust domain", "trust_domain", w.td.String())
		return
	}
	if err := w.publisher.Update(w.ctx, bundle); err != nil {
		clog.ErrorContext(w.ctx, "publishing", "error", err)
		return
	}
	clog.InfoContext(w.ctx, "published", "keys", len(bundle.JWTAuthorities()))
	// Ready once the documents are in the bucket.
	w.ready.Do(func() {
		if err := systemd.Ready(); err != nil {
			clog.ErrorContext(w.ctx, "notifying systemd", "error", err)
		}
	})
}

func (w *watcher) OnJWTBundlesWatchError(err error) {
	clog.WarnContext(w.ctx, "watching the Workload API", "error", err)
}

// bucket is an S3 bucket holding the issuer's documents.
type bucket struct {
	client *s3.Client
	name   string
}

func newBucket(ctx context.Context, cfg *config) (*bucket, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = &cfg.Endpoint
		// Checksums only where the API requires them: R2 rejects some of
		// the SDK's defaults.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	})
	return &bucket{client: client, name: cfg.Bucket}, nil
}

func (b *bucket) Put(ctx context.Context, key, contentType string, body []byte) error {
	_, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      &b.name,
		Key:         &key,
		Body:        bytes.NewReader(body),
		ContentType: &contentType,
	})
	return err
}
