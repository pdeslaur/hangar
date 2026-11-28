// Package haconfig keeps Home Assistant's configuration in a GitHub
// repository: it deploys the repository's commits to Home Assistant, and
// carries the UI's edits back as pull requests.
package haconfig

import (
	"context"
	"fmt"
	"time"

	"chainguard.dev/sdk/sts"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"golang.org/x/oauth2"
)

// SVIDSource fetches JWT-SVIDs, like the Workload API's workloadapi.Client.
type SVIDSource interface {
	FetchJWTSVID(ctx context.Context, params jwtsvid.Params) (*jwtsvid.SVID, error)
}

// OctoSTS returns GitHub tokens that octo-sts, at url, mints for the
// workload's JWT-SVIDs of audience, per the trust policy
// .github/chainguard/<identity>.sts.yaml of the repository scope,
// owner/name. Each token is reused until 10 minutes before its hour ends.
func OctoSTS(ctx context.Context, svids SVIDSource, url, audience, scope, identity string) oauth2.TokenSource {
	exchanger := sts.New(url, audience, sts.WithScope(scope), sts.WithIdentity(identity))
	tokens := sts.NewContextTokenSource(ctx, svidTokens{ctx, svids, audience}, exchanger)
	return oauth2.ReuseTokenSourceWithExpiry(nil, tokens, 10*time.Minute)
}

// svidTokens are the workload's JWT-SVIDs, as tokens.
type svidTokens struct {
	ctx      context.Context
	svids    SVIDSource
	audience string
}

func (s svidTokens) Token() (*oauth2.Token, error) {
	svid, err := s.svids.FetchJWTSVID(s.ctx, jwtsvid.Params{Audience: s.audience})
	if err != nil {
		return nil, fmt.Errorf("fetching a JWT-SVID for %s: %w", s.audience, err)
	}
	return &oauth2.Token{AccessToken: svid.Marshal(), Expiry: svid.Expiry}, nil
}
