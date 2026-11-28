// Package haconfig keeps Home Assistant's configuration in a GitHub
// repository: it deploys the repository's commits to Home Assistant, and
// carries the UI's edits back as pull requests.
package haconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
)

// SVIDSource fetches JWT-SVIDs, like the Workload API's workloadapi.Client.
type SVIDSource interface {
	FetchJWTSVID(ctx context.Context, params jwtsvid.Params) (*jwtsvid.SVID, error)
}

// OctoSTS mints GitHub tokens from octo-sts with the workload's JWT-SVIDs,
// and reuses each for most of its hour.
type OctoSTS struct {
	// URL is octo-sts's, like https://octo-sts.dev.
	URL string
	// Audience is the JWT-SVIDs', which the trust policy checks; octo-sts
	// defaults it to its domain, like octo-sts.dev.
	Audience string
	// Scope is the repository, owner/name, holding the trust policy.
	Scope string
	// Identity names the trust policy, .github/chainguard/<Identity>.sts.yaml.
	Identity string
	SVIDs    SVIDSource
	HTTP     *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// tokenReuse is how long it reuses a token: GitHub's installation tokens
// last an hour, and octo-sts doesn't say when one expires.
const tokenReuse = 45 * time.Minute

// Token returns a GitHub token, minting one when the last is too old.
func (o *OctoSTS) Token(ctx context.Context) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.token != "" && time.Now().Before(o.expires) {
		return o.token, nil
	}
	token, err := o.mint(ctx)
	if err != nil {
		return "", err
	}
	o.token, o.expires = token, time.Now().Add(tokenReuse)
	return token, nil
}

func (o *OctoSTS) mint(ctx context.Context) (string, error) {
	svid, err := o.SVIDs.FetchJWTSVID(ctx, jwtsvid.Params{Audience: o.Audience})
	if err != nil {
		return "", fmt.Errorf("fetching a JWT-SVID for %s: %w", o.Audience, err)
	}
	u := o.URL + "/sts/exchange?" + url.Values{"scope": {o.Scope}, "identity": {o.Identity}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+svid.Marshal())
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("octo-sts exchange for %s as %s: %s: %s", o.Scope, o.Identity, resp.Status, b)
	}
	var r struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return "", fmt.Errorf("octo-sts exchange: %w", err)
	}
	if r.Token == "" {
		return "", fmt.Errorf("octo-sts exchange for %s as %s: no token", o.Scope, o.Identity)
	}
	return r.Token, nil
}
