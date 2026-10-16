package cache

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Issuer signs the runtime tokens GitHub gives jobs, and KeysURL publishes
// its public keys.
const (
	Issuer  = "https://token.actions.githubusercontent.com"
	KeysURL = Issuer + "/.well-known/jwks"
)

// Permission bits of a cache scope in a runtime token.
const (
	Read  = 1
	Write = 2
)

// Scope is a ref whose cache entries a job may read or write, such as
// refs/heads/main.
type Scope struct {
	Name       string `json:"Scope"`
	Permission int    `json:"Permission"`
}

// Claims are what a verified runtime token says about the job that holds it.
type Claims struct {
	// Repo is the repository's id.
	Repo string
	// Scopes come in the token's order: the run's own ref first.
	Scopes []Scope
}

// Keys checks runtime tokens against the issuer's published keys. It reads
// them when it sees an unknown key id or its copy is an hour old, at most
// once a minute, and after a failed read waits twice as long as before, up
// to ten minutes. One call reads at a time; meanwhile the others use the
// keys at hand, or wait when they have none for the token.
type Keys struct {
	URL    string
	Issuer string
	HTTP   *http.Client
	// Log, when set, hears when the keys cannot be read.
	Log *slog.Logger

	mu   sync.Mutex
	keys map[string]*rsa.PublicKey
	// read is when the keys were read, and next the earliest the next read
	// may start.
	read, next time.Time
	failures   int
	// reading is closed when the read in progress ends.
	reading chan struct{}
}

// ErrToken means the token is missing, malformed, expired or not signed by
// the issuer.
var ErrToken = errors.New("invalid runtime token")

// Verify checks an RS256 token's signature, issuer and lifetime and returns
// its repository and cache scopes.
func (k *Keys) Verify(ctx context.Context, token string, now time.Time) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, fmt.Errorf("%w: not a JWT", ErrToken)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodePart(parts[0], &header); err != nil {
		return Claims{}, err
	}
	if header.Alg != "RS256" {
		return Claims{}, fmt.Errorf("%w: algorithm %q", ErrToken, header.Alg)
	}
	key, err := k.key(ctx, header.Kid, now)
	if err != nil {
		return Claims{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, fmt.Errorf("%w: signature: %v", ErrToken, err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return Claims{}, fmt.Errorf("%w: signature does not match", ErrToken)
	}
	var claims struct {
		Iss  string          `json:"iss"`
		Exp  int64           `json:"exp"`
		Nbf  int64           `json:"nbf"`
		Repo json.RawMessage `json:"repository_id"`
		AC   string          `json:"ac"`
		Scp  string          `json:"scp"`
	}
	if err := decodePart(parts[1], &claims); err != nil {
		return Claims{}, err
	}
	const leeway = time.Minute
	switch {
	case claims.Iss != k.Issuer:
		return Claims{}, fmt.Errorf("%w: issuer %q", ErrToken, claims.Iss)
	case claims.Exp == 0 || now.After(time.Unix(claims.Exp, 0).Add(leeway)):
		return Claims{}, fmt.Errorf("%w: expired", ErrToken)
	case now.Add(leeway).Before(time.Unix(claims.Nbf, 0)):
		return Claims{}, fmt.Errorf("%w: not valid yet", ErrToken)
	// The issuer also signs OIDC ID tokens, which are not for the results
	// service.
	case !slices.ContainsFunc(strings.Fields(claims.Scp), func(s string) bool { return strings.Split(s, ":")[0] == "Actions.Results" }):
		return Claims{}, fmt.Errorf("%w: no Actions.Results scope", ErrToken)
	}
	var c Claims
	// GitHub writes the id as a string; a number would mean the same.
	c.Repo = strings.Trim(string(claims.Repo), `"`)
	if c.Repo == "" || strings.Trim(c.Repo, "0123456789") != "" {
		return Claims{}, fmt.Errorf("%w: no repository_id", ErrToken)
	}
	if claims.AC == "" || json.Unmarshal([]byte(claims.AC), &c.Scopes) != nil || len(c.Scopes) == 0 {
		return Claims{}, fmt.Errorf("%w: no cache scopes", ErrToken)
	}
	return c, nil
}

func decodePart(part string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(part)
	if err == nil {
		err = json.Unmarshal(b, v)
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrToken, err)
	}
	return nil
}

func (k *Keys) key(ctx context.Context, kid string, now time.Time) (*rsa.PublicKey, error) {
	k.mu.Lock()
	for {
		key, ok := k.keys[kid]
		due := (!ok || now.Sub(k.read) > time.Hour) && !now.Before(k.next)
		if due && k.reading == nil {
			k.reading = make(chan struct{})
			k.mu.Unlock()
			keys, err := k.fetch(context.WithoutCancel(ctx))
			k.mu.Lock()
			close(k.reading)
			k.reading = nil
			if err != nil {
				k.failures++
				k.next = now.Add(min(time.Minute<<min(k.failures-1, 4), 10*time.Minute))
				if k.Log != nil {
					k.Log.Warn("cache: reading the token issuer's keys", "url", k.URL, "err", err)
				}
			} else {
				k.keys, k.read, k.next, k.failures = keys, now, now.Add(time.Minute), 0
			}
			continue
		}
		if due && !ok {
			reading := k.reading
			k.mu.Unlock()
			select {
			case <-reading:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			k.mu.Lock()
			continue
		}
		var err error
		switch {
		case ok:
		case k.keys == nil || k.failures > 0:
			err = errors.New("the token issuer's keys cannot be read")
		default:
			err = fmt.Errorf("%w: unknown key %q", ErrToken, kid)
		}
		k.mu.Unlock()
		return key, err
	}
}

func (k *Keys) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := k.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", k.URL, resp.Status)
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, fmt.Errorf("%s: %w", k.URL, err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, jwk := range set.Keys {
		n, errN := base64.RawURLEncoding.DecodeString(jwk.N)
		e, errE := base64.RawURLEncoding.DecodeString(jwk.E)
		if jwk.Kty != "RSA" || errN != nil || errE != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		keys[jwk.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%s: no RSA keys", k.URL)
	}
	return keys, nil
}
