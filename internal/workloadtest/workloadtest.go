// Package workloadtest serves a fake SPIFFE Workload API for tests, handing
// out JWT-SVIDs signed with a key of its own.
package workloadtest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/spiffe/go-spiffe/v2/proto/spiffe/workload"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// API is a fake Workload API, serving JWT-SVIDs for one SPIFFE ID.
type API struct {
	workload.UnimplementedSpiffeWorkloadAPIServer

	// Addr is where it listens, like SPIFFE_ENDPOINT_SOCKET names it.
	Addr string

	id     string
	signer jose.Signer

	mu        sync.Mutex
	unready   int
	audiences []string
}

// Serve serves a fake Workload API handing out JWT-SVIDs for id until the
// test ends.
func Serve(t *testing.T, id string) *API {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	// Not t.TempDir: its paths can be longer than a socket's may be.
	dir, err := os.MkdirTemp("", "workload")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "api.sock")
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	api := &API{Addr: "unix://" + socket, id: id, signer: signer}
	srv := grpc.NewServer()
	workload.RegisterSpiffeWorkloadAPIServer(srv, api)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	return api
}

// Unready makes its next n fetches fail, as an agent that hasn't attested
// the workload yet does.
func (a *API) Unready(n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.unready = n
}

// Audiences are those of the JWT-SVIDs it handed out, in order.
func (a *API) Audiences() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.audiences...)
}

func (a *API) FetchJWTSVID(_ context.Context, req *workload.JWTSVIDRequest) (*workload.JWTSVIDResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.unready > 0 {
		a.unready--
		return nil, status.Error(codes.PermissionDenied, "no identity issued")
	}
	now := time.Now()
	token, err := jwt.Signed(a.signer).Claims(jwt.Claims{
		Subject:  a.id,
		Audience: req.Audience,
		IssuedAt: jwt.NewNumericDate(now),
		Expiry:   jwt.NewNumericDate(now.Add(5 * time.Minute)),
	}).Serialize()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	a.audiences = append(a.audiences, req.Audience...)
	return &workload.JWTSVIDResponse{Svids: []*workload.JWTSVID{{SpiffeId: a.id, Svid: token}}}, nil
}
