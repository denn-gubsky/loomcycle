package kms

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Ref is what a CredentialDef stores for an external key: where it lives and
// which stored credentials reach it. Nothing in a Ref is secret — secrets are
// named by "$cred:<name>" and resolved through Options.Secrets at call time.
type Ref struct {
	Kind Kind `json:"kind"`

	// vault: Address is the Vault server URL (https://vault.example:8200);
	// Mount is the Transit mount path (default "transit"); Key the Transit key
	// name; Namespace the optional Vault Enterprise namespace; TokenCred the
	// "$cred:" holding the Vault token.
	Address   string `json:"address,omitempty"`
	Mount     string `json:"mount,omitempty"`
	Key       string `json:"key,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	TokenCred string `json:"token_cred,omitempty"`

	// aws_kms: Region (us-east-1), KeyID (key id, key ARN, alias/<name> or
	// alias ARN) and the "$cred:" references for the access key pair and the
	// optional session token.
	Region              string `json:"region,omitempty"`
	KeyID               string `json:"key_id,omitempty"`
	AccessKeyIDCred     string `json:"access_key_id_cred,omitempty"`
	SecretAccessKeyCred string `json:"secret_access_key_cred,omitempty"`
	SessionTokenCred    string `json:"session_token_cred,omitempty"`

	// gcp_kms: Resource is the CryptoKey name
	// (projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k>), optionally
	// pinned to one version with /cryptoKeyVersions/<n>; ServiceAccountCred is
	// the "$cred:" holding a service-account key JSON.
	Resource           string `json:"resource,omitempty"`
	ServiceAccountCred string `json:"service_account_cred,omitempty"`
}

const defaultVaultMount = "transit"

var (
	// credRefRe mirrors CredentialDef's name rule (credNameRe in tools/builtin).
	credRefRe = regexp.MustCompile(`^\$cred:[A-Za-z0-9_-]{1,128}$`)
	// vaultPathRe is a Vault mount or namespace: path segments, no "..".
	vaultPathRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}(/[A-Za-z0-9_.-]{1,128}){0,7}$`)
	vaultKeyRe  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	// awsRegionRe covers every partition's shape (us-east-1, us-gov-west-1,
	// cn-north-1, us-isob-east-1). The region becomes part of the endpoint
	// host, so it is held to this strictly.
	awsRegionRe = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]{1,2}$`)
	// awsKeyIDRe admits a key id, alias/<name>, or a key/alias ARN.
	awsKeyIDRe = regexp.MustCompile(`^[A-Za-z0-9:/_-]{1,512}$`)
	// gcpResourceRe is a CryptoKey, optionally pinned to a version.
	gcpResourceRe = regexp.MustCompile(`^projects/[a-z0-9.:-]{1,100}/locations/[a-z0-9-]{1,64}/keyRings/[A-Za-z0-9_-]{1,63}/cryptoKeys/[A-Za-z0-9_-]{1,63}(/cryptoKeyVersions/[1-9][0-9]{0,9})?$`)
)

// Validate checks the Ref's shape: the fields its kind needs are present and
// well-formed, credential fields are "$cred:<name>" references, and no field
// of another kind is set. allowHTTP admits a plain http:// Vault address. It
// makes no network call.
func (r Ref) Validate(allowHTTP bool) error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	cred := func(field, v string, required bool) {
		switch {
		case v == "" && required:
			bad("%s is required", field)
		case v != "" && !credRefRe.MatchString(v):
			bad("%s must be a $cred:<name> reference", field)
		}
	}
	notSet := func(fields map[string]string) {
		for name, v := range fields {
			if v != "" {
				bad("%s does not apply to kind %s", name, r.Kind)
			}
		}
	}
	vaultFields := map[string]string{"address": r.Address, "mount": r.Mount, "key": r.Key, "namespace": r.Namespace, "token_cred": r.TokenCred}
	awsFields := map[string]string{"region": r.Region, "key_id": r.KeyID, "access_key_id_cred": r.AccessKeyIDCred, "secret_access_key_cred": r.SecretAccessKeyCred, "session_token_cred": r.SessionTokenCred}
	gcpFields := map[string]string{"resource": r.Resource, "service_account_cred": r.ServiceAccountCred}

	switch r.Kind {
	case KindVault:
		if err := checkVaultAddress(r.Address, allowHTTP); err != nil {
			errs = append(errs, err)
		}
		if r.Mount != "" && !validVaultPath(r.Mount) {
			bad("mount must be a Vault path (letters, digits, _ . -, separated by /)")
		}
		if r.Namespace != "" && !validVaultPath(r.Namespace) {
			bad("namespace must be a Vault path (letters, digits, _ . -, separated by /)")
		}
		if !vaultKeyRe.MatchString(r.Key) || r.Key == "." || r.Key == ".." {
			bad("key must be a Transit key name (letters, digits, _ . -)")
		}
		cred("token_cred", r.TokenCred, true)
		notSet(awsFields)
		notSet(gcpFields)
	case KindAWS:
		if !awsRegionRe.MatchString(r.Region) {
			bad("region must be an AWS region such as us-east-1")
		}
		if !awsKeyIDRe.MatchString(r.KeyID) {
			bad("key_id must be a key id, key ARN, alias/<name> or alias ARN")
		} else if strings.HasPrefix(r.KeyID, "arn:") {
			// arn:<partition>:kms:<region>:<account>:key/<id> — the call goes to
			// Region's endpoint, so an ARN naming another region can't work.
			parts := strings.SplitN(r.KeyID, ":", 6)
			if len(parts) != 6 || parts[2] != "kms" || parts[3] != r.Region {
				bad("key_id ARN must be a kms ARN in region %q", r.Region)
			}
		}
		cred("access_key_id_cred", r.AccessKeyIDCred, true)
		cred("secret_access_key_cred", r.SecretAccessKeyCred, true)
		cred("session_token_cred", r.SessionTokenCred, false)
		notSet(vaultFields)
		notSet(gcpFields)
	case KindGCP:
		if !gcpResourceRe.MatchString(r.Resource) {
			bad("resource must be projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k>[/cryptoKeyVersions/<n>]")
		}
		cred("service_account_cred", r.ServiceAccountCred, true)
		notSet(vaultFields)
		notSet(awsFields)
	default:
		return fmt.Errorf("kms: unknown kind %q (valid: %s, %s, %s)", r.Kind, KindVault, KindAWS, KindGCP)
	}
	if len(errs) > 0 {
		return fmt.Errorf("kms: invalid %s ref: %w", r.Kind, errors.Join(errs...))
	}
	return nil
}

func validVaultPath(p string) bool {
	if !vaultPathRe.MatchString(p) {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// checkVaultAddress: https (http only when allowed), a host, and nothing that
// could smuggle credentials or reshape the request — no userinfo, query or
// fragment. A path prefix (a Vault behind a reverse proxy) is allowed.
func checkVaultAddress(addr string, allowHTTP bool) error {
	if addr == "" {
		return errors.New("address is required")
	}
	u, err := url.Parse(addr)
	if err != nil {
		return errors.New("address is not a valid URL")
	}
	switch {
	case u.Scheme == "http" && !allowHTTP:
		return errors.New("address must use https")
	case u.Scheme != "https" && u.Scheme != "http":
		return errors.New("address must be an https URL")
	case u.Host == "" || u.Opaque != "":
		return errors.New("address must name a host")
	case u.User != nil:
		return errors.New("address must not contain userinfo")
	case u.RawQuery != "" || u.ForceQuery:
		return errors.New("address must not contain a query")
	case u.Fragment != "" || strings.Contains(addr, "#"):
		return errors.New("address must not contain a fragment")
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == ".." || seg == "." {
			return errors.New("address path must not contain dot segments")
		}
	}
	return nil
}
