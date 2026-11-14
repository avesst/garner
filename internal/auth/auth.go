// Package auth creates API tokens and authenticates requests (design
// section 11).
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"github.com/avesst/garner/internal/id"
	"github.com/avesst/garner/internal/store"
)

// Scopes.
const (
	ScopeUpload  = "upload"
	ScopePublish = "publish"
	ScopeAdmin   = "admin"
)

// AllScopes lists every scope.
var AllScopes = []string{ScopeUpload, ScopePublish, ScopeAdmin}

var reLabel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// secretPrefix makes garner tokens recognizable to secret scanners.
const secretPrefix = "garner_"

// Hash is how a token secret is stored.
func Hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// CreateToken stores a new token and returns its secret, which is shown
// once and never stored.
func CreateToken(ctx context.Context, q *store.Queries, name, owner string, scopes []string) (string, *store.Token, error) {
	if !reLabel.MatchString(name) {
		return "", nil, fmt.Errorf("token name %q: use letters, digits, '.', '_' and '-'", name)
	}
	if !reLabel.MatchString(owner) || strings.HasPrefix(owner, "source") {
		return "", nil, fmt.Errorf("owner %q: use letters, digits, '.', '_' and '-', not starting with \"source\"", owner)
	}
	if len(scopes) == 0 {
		return "", nil, errors.New("at least one scope is required")
	}
	for _, s := range scopes {
		if !slices.Contains(AllScopes, s) {
			return "", nil, fmt.Errorf("unknown scope %q; scopes are %s", s, strings.Join(AllScopes, ", "))
		}
	}
	var raw [32]byte
	rand.Read(raw[:])
	secret := secretPrefix + base64.RawURLEncoding.EncodeToString(raw[:])
	t := &store.Token{ID: id.New(), Name: name, Owner: owner, Scopes: scopes, CreatedAt: store.Now()}
	if err := q.InsertToken(ctx, t, Hash(secret)); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return "", nil, fmt.Errorf("a token named %q already exists", name)
		}
		return "", nil, err
	}
	return secret, t, nil
}

// Policy decides whether a request may carry credentials.
type Policy struct {
	TrustedProxies    []netip.Prefix
	AllowInsecureAuth bool
}

// Errors returned by Authenticate.
var (
	ErrNoCredentials = errors.New("authentication required")
	ErrBadToken      = errors.New("invalid token")
	ErrInsecure      = errors.New("credentials are only accepted over HTTPS through a trusted proxy, or from loopback")
)

// Authenticate checks the request's Basic credentials (token name and
// secret) and the transport rules of section 11.
func Authenticate(ctx context.Context, q *store.Queries, pol Policy, r *http.Request) (*store.Token, error) {
	name, secret, ok := r.BasicAuth()
	if !ok {
		return nil, ErrNoCredentials
	}
	if !pol.AllowInsecureAuth && !secureTransport(pol, r) {
		return nil, ErrInsecure
	}
	t, err := q.TokenByHash(ctx, Hash(secret))
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrBadToken
	}
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare([]byte(t.Name), []byte(name)) != 1 {
		return nil, ErrBadToken
	}
	q.TouchToken(ctx, t.ID)
	return t, nil
}

// HasScope reports whether a token carries a scope. Scopes do not imply
// one another.
func HasScope(t *store.Token, scope string) bool { return slices.Contains(t.Scopes, scope) }

// RemoteAddr returns the peer address of a request.
func RemoteAddr(r *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// FromTrustedProxy reports whether the peer is a configured proxy, whose
// X-Forwarded-* headers may be honoured.
func FromTrustedProxy(pol Policy, r *http.Request) bool {
	a, ok := RemoteAddr(r)
	if !ok {
		return false
	}
	for _, p := range pol.TrustedProxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func secureTransport(pol Policy, r *http.Request) bool {
	a, ok := RemoteAddr(r)
	if !ok {
		return false
	}
	if a.IsLoopback() {
		return true
	}
	return FromTrustedProxy(pol, r) && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
