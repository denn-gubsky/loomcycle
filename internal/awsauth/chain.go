package awsauth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// refreshWindow is how long before expiry temporary credentials are replaced,
// so a request is never signed with credentials about to lapse in flight.
const refreshWindow = 5 * time.Minute

// ErrNoCredentials wraps every failure to obtain credentials, so a caller can
// tell "AWS is not configured here" from an AWS answer.
var ErrNoCredentials = errors.New("aws credentials unavailable")

// ChainOptions configures one credential chain.
type ChainOptions struct {
	// Region is the signing region; STS calls for role assumption go to it.
	Region string
	// RoleARN, when set, is assumed on top of whatever the chain found.
	RoleARN string
	// ExternalID goes with RoleARN (a value, already read from its env var).
	ExternalID string
	// SessionName is the role session name CloudTrail records. Default
	// "loomcycle".
	SessionName string
}

// CredentialSource yields current credentials and refreshes them before they
// expire. Safe for concurrent use: a burst of calls at expiry causes one
// refresh, not one per call.
type CredentialSource interface {
	Retrieve(ctx context.Context) (Credentials, error)
	// Invalidate drops cached credentials so the next Retrieve fetches fresh
	// ones (after AWS says a token expired early).
	Invalidate()
}

// NewChain builds the AWS SDK's default credential chain: environment keys,
// web identity, the shared config and credentials files (profile from
// AWS_PROFILE), the ECS / EKS Pod Identity container endpoint, and EC2
// instance metadata. Instance metadata is IMDSv2 only: the SDK's fallback to
// IMDSv1, the variant an SSRF can read without a token, is switched off.
//
// Nothing is fetched here; the first Retrieve does that.
func NewChain(ctx context.Context, o ChainOptions) (CredentialSource, error) {
	base, err := config.LoadDefaultConfig(ctx, config.WithRegion(o.Region))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoCredentials, err)
	}
	// The IMDS client is built from the same config, so an endpoint override
	// (AWS_EC2_METADATA_SERVICE_ENDPOINT, the IPv6 mode) still applies; only
	// the v1 fallback is forced off.
	imdsClient := imds.NewFromConfig(base, func(io *imds.Options) { io.EnableFallback = aws.FalseTernary })
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(o.Region),
		config.WithEC2RoleCredentialOptions(func(ro *ec2rolecreds.Options) { ro.Client = imdsClient }),
		config.WithCredentialsCacheOptions(func(co *aws.CredentialsCacheOptions) { co.ExpiryWindow = refreshWindow }),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoCredentials, err)
	}
	provider := cfg.Credentials
	if o.RoleARN != "" {
		name := o.SessionName
		if name == "" {
			name = "loomcycle"
		}
		assume := stscreds.NewAssumeRoleProvider(sts.NewFromConfig(cfg), o.RoleARN, func(ao *stscreds.AssumeRoleOptions) {
			ao.RoleSessionName = name
			if o.ExternalID != "" {
				ao.ExternalID = aws.String(o.ExternalID)
			}
		})
		provider = aws.NewCredentialsCache(assume, func(co *aws.CredentialsCacheOptions) { co.ExpiryWindow = refreshWindow })
	}
	if provider == nil {
		return nil, fmt.Errorf("%w: no credential provider", ErrNoCredentials)
	}
	return &sdkSource{provider: provider}, nil
}

type sdkSource struct {
	provider aws.CredentialsProvider
}

func (s *sdkSource) Retrieve(ctx context.Context) (Credentials, error) {
	c, err := s.provider.Retrieve(ctx)
	if err != nil {
		return Credentials{}, fmt.Errorf("%w: %v", ErrNoCredentials, err)
	}
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return Credentials{}, fmt.Errorf("%w: %s returned no key", ErrNoCredentials, c.Source)
	}
	return Credentials{
		AccessKeyID:     c.AccessKeyID,
		SecretAccessKey: c.SecretAccessKey,
		SessionToken:    c.SessionToken,
		CanExpire:       c.CanExpire,
		Expires:         c.Expires,
		Source:          c.Source,
	}, nil
}

func (s *sdkSource) Invalidate() {
	if c, ok := s.provider.(*aws.CredentialsCache); ok {
		c.Invalidate()
	}
}
