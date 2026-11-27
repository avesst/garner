package oci

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/avesst/garner/internal/auth"
	"github.com/avesst/garner/internal/blob"
	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/id"
	"github.com/avesst/garner/internal/ingest"
	"github.com/avesst/garner/internal/store"
)

// Registry serves /v2/: pulls from the current snapshot and the blob
// store, and pushes into ingest (section 19). A reverse proxy may serve
// the pulls from public/ itself and send only the rest here.
type Registry struct {
	Store    *store.Store
	Blobs    *blob.Store
	Ingester *ingest.Ingester
	Config   func() *config.Config
	// Root is storage.path.
	Root string
	Log  *slog.Logger
	// Now is the clock for staging; tests replace it.
	Now func() time.Time
}

// Prefix is the path the registry serves.
const Prefix = "/v2/"

// Error codes of the distribution spec.
const (
	codeBlobUnknown         = "BLOB_UNKNOWN"
	codeBlobUploadInvalid   = "BLOB_UPLOAD_INVALID"
	codeBlobUploadUnknown   = "BLOB_UPLOAD_UNKNOWN"
	codeDigestInvalid       = "DIGEST_INVALID"
	codeManifestBlobUnknown = "MANIFEST_BLOB_UNKNOWN"
	codeManifestInvalid     = "MANIFEST_INVALID"
	codeManifestUnknown     = "MANIFEST_UNKNOWN"
	codeNameInvalid         = "NAME_INVALID"
	codeNameUnknown         = "NAME_UNKNOWN"
	codeSizeInvalid         = "SIZE_INVALID"
	codeUnauthorized        = "UNAUTHORIZED"
	codeDenied              = "DENIED"
	codeUnsupported         = "UNSUPPORTED"
)

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string][]apiError{"errors": {{Code: code, Message: msg}}})
}

func (g *Registry) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// route is a parsed /v2/ path.
type route struct {
	name string
	kind string // "manifests", "blobs", "uploads", "tags"
	ref  string // tag or digest, digest, or upload ID
}

func parseRoute(p string) (route, bool) {
	rest := strings.TrimPrefix(p, Prefix)
	if i := strings.Index(rest, "/blobs/uploads"); i > 0 {
		tail := rest[i+len("/blobs/uploads"):]
		if tail != "" && tail[0] != '/' {
			return route{}, false
		}
		return route{name: rest[:i], kind: "uploads", ref: strings.TrimPrefix(tail, "/")}, true
	}
	for _, k := range []string{"manifests", "blobs"} {
		if i := strings.LastIndex(rest, "/"+k+"/"); i > 0 {
			return route{name: rest[:i], kind: k, ref: rest[i+len(k)+2:]}, true
		}
	}
	if name, ok := strings.CutSuffix(rest, "/tags/list"); ok && name != "" {
		return route{name: name, kind: "tags"}, true
	}
	return route{}, false
}

func (g *Registry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	if r.URL.Path == Prefix || r.URL.Path == strings.TrimSuffix(Prefix, "/") {
		g.ping(w, r)
		return
	}
	if r.URL.Path == tokenPath {
		g.token(w, r)
		return
	}
	rt, ok := parseRoute(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, codeUnsupported, "no such registry endpoint")
		return
	}
	if err := ValidName(rt.name); err != nil {
		writeError(w, http.StatusNotFound, codeNameInvalid, err.Error())
		return
	}
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	switch {
	case rt.kind == "manifests" && read:
		g.getManifest(w, r, rt)
	case rt.kind == "manifests" && r.Method == http.MethodPut:
		g.putManifest(w, r, rt)
	case rt.kind == "manifests" && r.Method == http.MethodDelete:
		g.deleteManifest(w, r, rt)
	case rt.kind == "blobs" && read:
		g.getBlob(w, r, rt)
	case rt.kind == "tags" && r.Method == http.MethodGet:
		g.listTags(w, r, rt)
	case rt.kind == "uploads" && rt.ref == "" && r.Method == http.MethodPost:
		g.startUpload(w, r, rt)
	case rt.kind == "uploads" && rt.ref != "":
		g.upload(w, r, rt)
	default:
		writeError(w, http.StatusMethodNotAllowed, codeUnsupported, r.Method+" is not supported here")
	}
}

// Registry auth follows the token flow of the distribution spec: a 401
// names the token endpoint, which gives anonymous clients a token that
// only pulls (pulls check nothing) and checks the Basic credentials of
// the others. A Basic challenge would stop anonymous pulls with Docker's
// classic image store, which refuses a Basic challenge it has no
// credentials for.
const (
	tokenPath      = Prefix + "token"
	tokenService   = "garner"
	anonymousToken = "anonymous"
	tokenLifetime  = time.Hour
)

// realm is the token endpoint at the registry's host, which clients put
// in front of image names.
func (g *Registry) realm() string {
	return registryURL(g.Config(), g.Config().Server.PublicURL) + tokenPath
}

// challenge asks for a token, for pushing to repo when it is not "".
func (g *Registry) challenge(w http.ResponseWriter, repo string) {
	v := fmt.Sprintf(`Bearer realm=%q,service=%q`, g.realm(), tokenService)
	if repo != "" {
		v += fmt.Sprintf(`,scope="repository:%s:pull,push"`, repo)
	}
	w.Header().Set("WWW-Authenticate", v)
}

// basicFromBearer returns r with a token from the token endpoint turned
// back into the Basic credentials it carries, and an anonymous or
// malformed token dropped.
func basicFromBearer(r *http.Request) *http.Request {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return r
	}
	r = r.Clone(r.Context())
	r.Header.Del("Authorization")
	if raw, err := base64.RawURLEncoding.DecodeString(tok); err == nil && tok != anonymousToken {
		r.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString(raw))
	}
	return r
}

// token is the token endpoint. The token carries the checked Basic
// credentials, so any node can check it again without shared state.
func (g *Registry) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, codeUnsupported, r.Method+" is not supported here")
		return
	}
	tok := anonymousToken
	if name, secret, ok := r.BasicAuth(); ok {
		if g.authenticate(w, r, "", "") == nil {
			return
		}
		tok = base64.RawURLEncoding.EncodeToString([]byte(name + ":" + secret))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{
		"token": tok, "access_token": tok,
		"expires_in": int(tokenLifetime.Seconds()), "issued_at": g.now().UTC().Format(time.RFC3339),
	})
}

// authenticate checks the request's credentials, Basic or a token from
// the token endpoint, and scope. On failure it writes the response, with
// a challenge for pushing to repo, and returns nil.
func (g *Registry) authenticate(w http.ResponseWriter, r *http.Request, scope, repo string) *store.Token {
	cfg := g.Config()
	pol := auth.Policy{TrustedProxies: cfg.TrustedProxyPrefixes(), AllowInsecureAuth: cfg.Server.AllowInsecureAuth}
	t, err := auth.Authenticate(r.Context(), &g.Store.Queries, pol, basicFromBearer(r))
	switch {
	case errors.Is(err, auth.ErrNoCredentials), errors.Is(err, auth.ErrBadToken):
		g.challenge(w, repo)
		writeError(w, http.StatusUnauthorized, codeUnauthorized, err.Error())
		return nil
	case errors.Is(err, auth.ErrInsecure):
		writeError(w, http.StatusForbidden, codeDenied, err.Error())
		return nil
	case err != nil:
		g.internal(w, err)
		return nil
	}
	if scope != "" && !auth.HasScope(t, scope) {
		writeError(w, http.StatusForbidden, codeDenied, fmt.Sprintf("token %q lacks the %s scope", t.Name, scope))
		return nil
	}
	return t
}

func (g *Registry) internal(w http.ResponseWriter, err error) {
	g.Log.Error("registry request failed", "err", err)
	writeError(w, http.StatusInternalServerError, codeUnsupported, "internal error; see the server log")
}

// ping answers the version check clients start with. Without credentials
// it challenges, so that docker login gets a token and with it checks the
// credentials; an anonymous token passes, as pulls need nothing more.
func (g *Registry) ping(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, codeUnsupported, r.Method+" is not supported here")
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+anonymousToken && g.authenticate(w, r, "", "") == nil {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, "{}")
}

func (g *Registry) public(name string, parts ...string) string {
	return filepath.Join(append([]string{g.Root, "public", "v2", filepath.FromSlash(name)}, parts...)...)
}

// getManifest serves a manifest of the current snapshot by tag or digest.
func (g *Registry) getManifest(w http.ResponseWriter, r *http.Request, rt route) {
	if !ValidTag(rt.ref) {
		if _, err := ParseDigest(rt.ref); err != nil {
			writeError(w, http.StatusNotFound, codeManifestUnknown, "invalid tag or digest")
			return
		}
	}
	for _, k := range kinds {
		body, err := os.ReadFile(g.public(rt.name, "manifests", rt.ref+"."+k.suffix))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			g.internal(w, err)
			return
		}
		digest := Digest(body)
		w.Header().Set("Content-Type", k.mediaType)
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("ETag", `"`+digest+`"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if strings.HasPrefix(rt.ref, "sha256:") {
			w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		if r.Method == http.MethodGet {
			w.Write(body)
		}
		return
	}
	writeError(w, http.StatusNotFound, codeManifestUnknown, fmt.Sprintf("%s:%s is not published", rt.name, rt.ref))
}

// getBlob serves a published image blob from the pool. A HEAD also finds
// a blob a push staged, so a client does not send a layer garner has;
// other stored blobs, such as those of held packages, are not served.
func (g *Registry) getBlob(w http.ResponseWriter, r *http.Request, rt route) {
	h, err := ParseDigest(rt.ref)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeDigestInvalid, err.Error())
		return
	}
	setHeaders := func() {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Docker-Content-Digest", rt.ref)
		w.Header().Set("ETag", `"`+rt.ref+`"`)
	}
	f, err := os.Open(filepath.Join(g.Root, "pool", Format{}.Name(), Format{}.BlobPoolPath(h)))
	if errors.Is(err, fs.ErrNotExist) {
		if r.Method == http.MethodHead {
			staged, err := g.Store.Staged(r.Context(), h, g.now())
			if err != nil {
				g.internal(w, err)
				return
			}
			size, serr := g.Store.BlobSize(r.Context(), h)
			if staged && serr == nil {
				if _, err := os.Stat(g.Blobs.PathOf(h)); err == nil {
					// The client skips the upload now, so keep the blob
					// for the rest of its push.
					if err := g.Store.StageBlob(r.Context(), h, g.now().Add(g.expiry())); err != nil {
						g.internal(w, err)
						return
					}
					setHeaders()
					w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
					return
				}
			}
		}
		writeError(w, http.StatusNotFound, codeBlobUnknown, rt.ref+" is not published")
		return
	}
	if err != nil {
		g.internal(w, err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		g.internal(w, err)
		return
	}
	setHeaders()
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

// listTags serves the current snapshot's tag list, with the spec's n and
// last pagination.
func (g *Registry) listTags(w http.ResponseWriter, r *http.Request, rt route) {
	body, err := os.ReadFile(g.public(rt.name, "tags", "list"))
	if errors.Is(err, fs.ErrNotExist) {
		writeError(w, http.StatusNotFound, codeNameUnknown, rt.name+" is not published")
		return
	}
	if err != nil {
		g.internal(w, err)
		return
	}
	var list TagList
	if err := json.Unmarshal(body, &list); err != nil {
		g.internal(w, err)
		return
	}
	q := r.URL.Query()
	if last := q.Get("last"); last != "" {
		i := 0
		for i < len(list.Tags) && list.Tags[i] <= last {
			i++
		}
		list.Tags = list.Tags[i:]
	}
	if ns := q.Get("n"); ns != "" {
		n, err := strconv.Atoi(ns)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, codeUnsupported, "n must be a non-negative number")
			return
		}
		if n < len(list.Tags) {
			list.Tags = list.Tags[:n]
			if n > 0 {
				next := url.Values{"n": {ns}, "last": {list.Tags[n-1]}}
				w.Header().Set("Link", fmt.Sprintf(`<%s%s/tags/list?%s>; rel="next"`, Prefix, rt.name, next.Encode()))
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	json.NewEncoder(w).Encode(list)
}

// mayPush checks the token and that its owner may deliver the name. On
// failure it writes the response and returns nil.
func (g *Registry) mayPush(w http.ResponseWriter, r *http.Request, name string) *store.Token {
	t := g.authenticate(w, r, auth.ScopeUpload, name)
	if t == nil {
		return nil
	}
	why, err := g.Ingester.MayDeliver(r.Context(), Format{}, name, t.Owner)
	if err != nil {
		g.internal(w, err)
		return nil
	}
	if why != "" {
		writeError(w, http.StatusForbidden, codeDenied, why)
		return nil
	}
	return t
}

func (g *Registry) uploadPath(id string) string { return filepath.Join(g.Root, "uploads", id) }

func uploadLocation(name, id string) string { return Prefix + name + "/blobs/uploads/" + id }

// startUpload opens a blob upload session, or finishes one in a single
// request (digest), or reuses a stored blob (mount).
func (g *Registry) startUpload(w http.ResponseWriter, r *http.Request, rt route) {
	if g.mayPush(w, r, rt.name) == nil {
		return
	}
	q := r.URL.Query()
	if m := q.Get("mount"); m != "" {
		if h, err := ParseDigest(m); err == nil {
			ok, err := g.stageStored(r.Context(), h)
			if err != nil {
				g.internal(w, err)
				return
			}
			if ok {
				w.Header().Set("Location", Prefix+rt.name+"/blobs/"+m)
				w.Header().Set("Docker-Content-Digest", m)
				w.WriteHeader(http.StatusCreated)
				return
			}
		}
	}
	u := &store.Upload{ID: id.New(), Format: Format{}.Name(), Name: rt.name}
	if err := os.MkdirAll(filepath.Dir(g.uploadPath(u.ID)), 0o755); err != nil {
		g.internal(w, err)
		return
	}
	f, err := os.OpenFile(g.uploadPath(u.ID), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		g.internal(w, err)
		return
	}
	f.Close()
	if err := g.Store.InsertUpload(r.Context(), u); err != nil {
		os.Remove(g.uploadPath(u.ID))
		g.internal(w, err)
		return
	}
	if d := q.Get("digest"); d != "" {
		if !g.appendChunk(w, r, u) {
			return
		}
		g.finishUpload(w, r, u, d)
		return
	}
	w.Header().Set("Location", uploadLocation(rt.name, u.ID))
	w.Header().Set("Range", "0-0")
	w.Header().Set("Docker-Upload-UUID", u.ID)
	w.WriteHeader(http.StatusAccepted)
}

// available reports whether a push may use a blob it did not send: one
// published as part of an image, or staged by a push or sync. Other stored
// blobs, such as a held package's, stay out of reach.
func (g *Registry) available(ctx context.Context, sha string) (bool, error) {
	if _, err := os.Stat(filepath.Join(g.Root, "pool", Format{}.Name(), Format{}.BlobPoolPath(sha))); err == nil {
		return true, nil
	}
	return g.Store.Staged(ctx, sha, g.now())
}

// stageStored stages a stored blob for a push that mounts it. It reports
// false when the blob is not available to pushes.
func (g *Registry) stageStored(ctx context.Context, sha string) (bool, error) {
	if ok, err := g.available(ctx, sha); err != nil || !ok {
		return false, err
	}
	f, err := g.Blobs.Open(sha)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false, err
	}
	return true, g.Store.Tx(ctx, func(q *store.Queries) error {
		if err := q.AddBlob(ctx, sha, fi.Size()); err != nil {
			return err
		}
		if err := g.Blobs.Ensure(sha, f, fi.Size()); err != nil {
			return err
		}
		return q.StageBlob(ctx, sha, g.now().Add(g.expiry()))
	})
}

func (g *Registry) expiry() time.Duration {
	if c := g.Config().OCI; c != nil {
		return c.UploadExpiry
	}
	return 24 * time.Hour
}

// upload handles the requests on an open session.
func (g *Registry) upload(w http.ResponseWriter, r *http.Request, rt route) {
	if g.mayPush(w, r, rt.name) == nil {
		return
	}
	u, err := g.Store.GetUpload(r.Context(), rt.ref)
	if errors.Is(err, store.ErrNotFound) || err == nil && (u.Name != rt.name || u.Format != (Format{}).Name()) {
		writeError(w, http.StatusNotFound, codeBlobUploadUnknown, "no such upload")
		return
	}
	if err != nil {
		g.internal(w, err)
		return
	}
	switch r.Method {
	case http.MethodGet:
		g.progress(w, u, http.StatusNoContent)
	case http.MethodPatch:
		if g.appendChunk(w, r, u) {
			g.progress(w, u, http.StatusAccepted)
		}
	case http.MethodPut:
		d := r.URL.Query().Get("digest")
		if d == "" {
			writeError(w, http.StatusBadRequest, codeDigestInvalid, "the digest parameter is required")
			return
		}
		if g.appendChunk(w, r, u) {
			g.finishUpload(w, r, u, d)
		}
	case http.MethodDelete:
		g.dropUpload(r.Context(), u.ID)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, codeUnsupported, r.Method+" is not supported here")
	}
}

func (g *Registry) progress(w http.ResponseWriter, u *store.Upload, status int) {
	w.Header().Set("Location", uploadLocation(u.Name, u.ID))
	end := u.Size - 1
	if end < 0 {
		end = 0
	}
	w.Header().Set("Range", fmt.Sprintf("0-%d", end))
	w.Header().Set("Docker-Upload-UUID", u.ID)
	w.WriteHeader(status)
}

func (g *Registry) dropUpload(ctx context.Context, id string) {
	if err := g.Store.DeleteUpload(ctx, id); err != nil {
		g.Log.Error("cannot remove upload session", "upload", id, "err", err)
	}
	os.Remove(g.uploadPath(id))
}

// uploadIdleTimeout cuts off a chunk that stops arriving.
var uploadIdleTimeout = time.Minute

// appendChunk writes the request body at the session's end. A
// Content-Range must start there. On failure it writes the response and
// returns false; on success u.Size is the new size.
func (g *Registry) appendChunk(w http.ResponseWriter, r *http.Request, u *store.Upload) bool {
	if cr := r.Header.Get("Content-Range"); cr != "" {
		start, _, ok := strings.Cut(strings.TrimPrefix(cr, "bytes "), "-")
		n, err := strconv.ParseInt(start, 10, 64)
		if !ok || err != nil || n != u.Size {
			w.Header().Set("Location", uploadLocation(u.Name, u.ID))
			w.Header().Set("Range", fmt.Sprintf("0-%d", max(u.Size-1, 0)))
			writeError(w, http.StatusRequestedRangeNotSatisfiable, codeBlobUploadInvalid,
				fmt.Sprintf("chunk must start at %d", u.Size))
			return false
		}
	}
	limit := int64(g.Config().Storage.MaxPackageSize) - u.Size
	f, err := os.OpenFile(g.uploadPath(u.ID), os.O_WRONLY, 0)
	if errors.Is(err, fs.ErrNotExist) {
		writeError(w, http.StatusNotFound, codeBlobUploadUnknown, "the upload's data is gone")
		return false
	}
	if err != nil {
		g.internal(w, err)
		return false
	}
	defer f.Close()
	body := &deadlineReader{r: r.Body, rc: http.NewResponseController(w)}
	n, err := io.Copy(io.NewOffsetWriter(f, u.Size), io.LimitReader(body, limit+1))
	if err == nil && n > limit {
		writeError(w, http.StatusRequestEntityTooLarge, codeSizeInvalid,
			fmt.Sprintf("blob exceeds storage.max_package_size (%d bytes)", g.Config().Storage.MaxPackageSize))
		g.dropUpload(r.Context(), u.ID)
		return false
	}
	if err != nil {
		// The client went away or stalled; the session stays at its old
		// size, so it can resume.
		writeError(w, http.StatusBadRequest, codeBlobUploadInvalid, "reading the chunk failed: "+err.Error())
		return false
	}
	if n == 0 {
		return true
	}
	ok, err := g.Store.GrowUpload(r.Context(), u.ID, u.Size, u.Size+n)
	if err != nil {
		g.internal(w, err)
		return false
	}
	if !ok {
		writeError(w, http.StatusRequestedRangeNotSatisfiable, codeBlobUploadInvalid,
			"another request changed the upload; resume from its current size")
		return false
	}
	u.Size += n
	return true
}

// deadlineReader moves the connection's read deadline with the data and
// clears it at the end of the body: the deadline covers the whole
// connection, and net/http cancels the request's context when it passes,
// so it must not cut off the work that follows, such as finishing a blob.
type deadlineReader struct {
	r  io.Reader
	rc *http.ResponseController
}

func (d *deadlineReader) Read(p []byte) (int, error) {
	d.rc.SetReadDeadline(time.Now().Add(uploadIdleTimeout))
	n, err := d.r.Read(p)
	if err == io.EOF {
		d.rc.SetReadDeadline(time.Time{})
	}
	return n, err
}

// finishUpload checks the session's data against digest and stores it as
// a staged blob.
func (g *Registry) finishUpload(w http.ResponseWriter, r *http.Request, u *store.Upload, digest string) {
	ctx := r.Context()
	want, err := ParseDigest(digest)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeDigestInvalid, err.Error())
		return
	}
	// The recorded size is what the session holds; a failed chunk may
	// have left bytes past it. The data is copied into a new file, so a
	// request still writing to the session can't change the stored blob:
	// at worst the digest does not match.
	src, err := os.Open(g.uploadPath(u.ID))
	if errors.Is(err, fs.ErrNotExist) {
		writeError(w, http.StatusNotFound, codeBlobUploadUnknown, "the upload's data is gone")
		return
	}
	if err != nil {
		g.internal(w, err)
		return
	}
	b, f, err := g.Blobs.PutOpen(io.LimitReader(src, u.Size))
	src.Close()
	if err != nil {
		g.internal(w, err)
		return
	}
	defer f.Close()
	if b.Size != u.Size {
		g.dropUpload(ctx, u.ID)
		writeError(w, http.StatusBadRequest, codeBlobUploadInvalid,
			fmt.Sprintf("the upload holds %d bytes, recorded %d", b.Size, u.Size))
		return
	}
	if b.SHA256 != want {
		// The data is stored as an unreferenced blob, which GC collects.
		g.dropUpload(ctx, u.ID)
		writeError(w, http.StatusBadRequest, codeDigestInvalid,
			fmt.Sprintf("the upload has digest sha256:%s, not %s", b.SHA256, digest))
		return
	}
	err = g.Store.Tx(ctx, func(q *store.Queries) error {
		if err := q.AddBlob(ctx, b.SHA256, b.Size); err != nil {
			return err
		}
		if err := g.Blobs.Ensure(b.SHA256, f, b.Size); err != nil {
			return err
		}
		if err := q.StageBlob(ctx, b.SHA256, g.now().Add(g.expiry())); err != nil {
			return err
		}
		return q.DeleteUpload(ctx, u.ID)
	})
	if err != nil {
		g.internal(w, err)
		return
	}
	os.Remove(g.uploadPath(u.ID))
	w.Header().Set("Location", Prefix+u.Name+"/blobs/"+digest)
	w.Header().Set("Docker-Content-Digest", digest)
	w.WriteHeader(http.StatusCreated)
}

// fetch reads a stored manifest for Walk.
func (g *Registry) fetch(sha string) ([]byte, error) {
	f, err := g.Blobs.Open(sha)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrMissing
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, MaxManifestSize))
}

// putManifest stores a manifest. By tag it records the image and moves
// the tag; by digest it only stores the manifest, for an index pushed
// after it.
func (g *Registry) putManifest(w http.ResponseWriter, r *http.Request, rt route) {
	t := g.mayPush(w, r, rt.name)
	if t == nil {
		return
	}
	byDigest := strings.HasPrefix(rt.ref, "sha256:")
	if !byDigest && !ValidTag(rt.ref) {
		writeError(w, http.StatusBadRequest, codeManifestInvalid, "invalid tag "+strconv.Quote(rt.ref))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxManifestSize+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, codeManifestInvalid, "reading the manifest failed: "+err.Error())
		return
	}
	if len(body) > MaxManifestSize {
		writeError(w, http.StatusRequestEntityTooLarge, codeSizeInvalid, "manifests are limited to 4 MiB")
		return
	}
	tree, err := Walk(body, r.Header.Get("Content-Type"), g.fetch)
	switch {
	case errors.Is(err, ErrMissing):
		writeError(w, http.StatusBadRequest, codeManifestBlobUnknown, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, codeManifestInvalid, err.Error())
		return
	}
	if byDigest && rt.ref != tree.Digest {
		writeError(w, http.StatusBadRequest, codeDigestInvalid,
			fmt.Sprintf("the manifest has digest %s, not %s", tree.Digest, rt.ref))
		return
	}
	ctx := r.Context()
	for _, sha := range tree.Blobs {
		ok, err := g.available(ctx, sha)
		if err != nil {
			g.internal(w, err)
			return
		}
		if !ok {
			writeError(w, http.StatusBadRequest, codeManifestBlobUnknown, "blob sha256:"+sha+" has not been pushed")
			return
		}
	}
	if byDigest {
		if err := g.stageContent(ctx, body); err != nil {
			g.internal(w, err)
			return
		}
	} else {
		res, err := g.Ingester.Record(ctx, delivery(tree, rt.name, []string{rt.ref}, ingest.Request{
			Owner: t.Owner, Origin: "upload", OriginRef: "token:" + t.Name, VerifyStatus: "n/a",
		}))
		var ie *ingest.Error
		switch {
		case errors.As(err, &ie) && ie.Kind == ingest.Conflict:
			writeError(w, http.StatusForbidden, codeDenied, ie.Msg)
			return
		case errors.As(err, &ie):
			writeError(w, http.StatusBadRequest, codeManifestBlobUnknown, ie.Msg)
			return
		case err != nil:
			g.internal(w, err)
			return
		}
		g.Log.Info("image pushed", "name", rt.name, "tag", rt.ref, "digest", tree.Digest,
			"token", t.Name, "existing", res.Existing)
	}
	w.Header().Set("Location", Prefix+rt.name+"/manifests/"+tree.Digest)
	w.Header().Set("Docker-Content-Digest", tree.Digest)
	w.WriteHeader(http.StatusCreated)
}

// delivery is the ingest delivery of a manifest tree.
func delivery(tree *Tree, name string, tags []string, req ingest.Request) ingest.Delivery {
	md, _ := json.Marshal(tree.Meta)
	return ingest.Delivery{
		Format:  Format{},
		Parsed:  &format.Parsed{Name: name, Version: tree.Digest, Metadata: md},
		Content: tree.Content,
		Blobs:   tree.Blobs,
		Tags:    tags,
		Request: req,
	}
}

// stageContent stores and stages a manifest pushed by digest.
func (g *Registry) stageContent(ctx context.Context, body []byte) error {
	b, f, err := g.Blobs.PutOpen(strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	defer f.Close()
	return g.Store.Tx(ctx, func(q *store.Queries) error {
		if err := q.AddBlob(ctx, b.SHA256, b.Size); err != nil {
			return err
		}
		if err := g.Blobs.Ensure(b.SHA256, f, b.Size); err != nil {
			return err
		}
		return q.StageBlob(ctx, b.SHA256, g.now().Add(g.expiry()))
	})
}

// deleteManifest removes a tag, or by digest the image from future
// snapshots with its tags. Both need admin, like garner remove.
func (g *Registry) deleteManifest(w http.ResponseWriter, r *http.Request, rt route) {
	t := g.authenticate(w, r, auth.ScopeAdmin, rt.name)
	if t == nil {
		return
	}
	ctx := r.Context()
	found := false
	err := g.Store.Tx(ctx, func(q *store.Queries) error {
		if !strings.HasPrefix(rt.ref, "sha256:") {
			var err error
			found, err = q.DeleteTag(ctx, Format{}.Name(), rt.name, rt.ref)
			return err
		}
		pkgs, err := q.Placed(ctx, store.PackageFilter{Format: Format{}.Name(), Name: rt.name, Version: rt.ref})
		if err != nil {
			return err
		}
		for _, p := range pkgs {
			if p.State == store.Removed {
				continue
			}
			found = true
			if _, err := q.SetState(ctx, p.ID, p.Distribution, store.Removed); err != nil {
				return err
			}
			if err := q.DeleteTagsOf(ctx, p.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		g.internal(w, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, codeManifestUnknown, fmt.Sprintf("%s:%s is not stored", rt.name, rt.ref))
		return
	}
	g.Log.Info("image deleted", "name", rt.name, "ref", rt.ref, "token", t.Name)
	if g.Ingester.Publish != nil {
		g.Ingester.Publish()
	}
	w.WriteHeader(http.StatusAccepted)
}

// CollectUploads removes upload sessions idle for longer than
// oci.upload_expiry and upload files without a session. GC calls it.
func (g *Registry) CollectUploads(ctx context.Context) error {
	sessions, err := g.Store.Uploads(ctx)
	if err != nil {
		return err
	}
	cutoff := g.now().Add(-g.expiry())
	for id, at := range sessions {
		// A chunk being written moves the file's time before the row's.
		if fi, err := os.Stat(g.uploadPath(id)); err == nil && fi.ModTime().After(at) {
			at = fi.ModTime()
		}
		if at.Before(cutoff) {
			g.Log.Info("removing an abandoned upload", "upload", id)
			g.dropUpload(ctx, id)
		}
	}
	entries, err := os.ReadDir(filepath.Join(g.Root, "uploads"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if _, ok := sessions[e.Name()]; ok {
			continue
		}
		// A session row is inserted after its file; give that a moment.
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(g.Root, "uploads", e.Name()))
		}
	}
	return nil
}
