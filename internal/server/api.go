package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/avesst/garner/internal/auth"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/ingest"
	"github.com/avesst/garner/internal/store"
)

// PackageJSON is a package as the API returns it.
type PackageJSON struct {
	ID           string `json:"id"`
	Format       string `json:"format"`
	Name         string `json:"name"`
	Version      string `json:"version"`
	Arch         string `json:"arch"`
	Distribution string `json:"distribution"`
	State        string `json:"state"`
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size,omitempty"`
	Origin       string `json:"origin"`
	OriginRef    string `json:"origin_ref"`
	VerifyStatus string `json:"verify_status"`
	Pinned       bool   `json:"pinned,omitempty"`
	CreatedAt    string `json:"created_at"`
}

// FullVersion formats epoch, version and release in the usual way.
func FullVersion(p *format.Package) string {
	v := p.Version
	if p.Release != "" {
		v += "-" + p.Release
	}
	if p.Epoch != 0 {
		v = fmt.Sprintf("%d:%s", p.Epoch, v)
	}
	return v
}

func packageJSON(p *format.Package) PackageJSON {
	return PackageJSON{
		ID: p.ID, Format: p.Format, Name: p.Name, Version: FullVersion(p), Arch: p.Arch,
		Distribution: p.Distribution, State: p.State, SHA256: p.ServedSHA256(), Size: p.ServedSize,
		Origin: p.Origin, OriginRef: p.OriginRef, VerifyStatus: p.VerifyStatus, Pinned: p.Pinned,
		CreatedAt: p.CreatedAt,
	}
}

// UploadResponse is the body of a successful upload.
type UploadResponse struct {
	Package  PackageJSON `json:"package"`
	Existing bool        `json:"existing"`
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	cfg := s.Config()
	t := tokenFrom(r)
	targets := map[string]string{}
	configured := map[string][]string{}
	for _, fm := range s.Formats.All() {
		key := fm.DistributionKey()
		targets[key] = r.URL.Query().Get(key)
		configured[key] = fm.Distributions(cfg)
	}
	// A slow upload may take longer than ReadTimeout; a stalled one is cut
	// off after uploadIdleTimeout without data.
	rc := http.NewResponseController(w)
	progress := &deadlineReader{r: r.Body, extend: func() {
		rc.SetReadDeadline(time.Now().Add(uploadIdleTimeout)) // unsupported in tests' recorders
	}}
	body := http.MaxBytesReader(w, progress, int64(cfg.Storage.MaxPackageSize))
	counter := &countingReader{r: body}
	res, err := s.Ingester.Ingest(r.Context(), counter, ingest.Request{
		Targets: targets, Owner: t.Owner, Origin: "upload",
		OriginRef: "token:" + t.Name, VerifyStatus: "n/a",
	})
	var tooBig *http.MaxBytesError
	var ie *ingest.Error
	switch {
	case errors.As(err, &tooBig):
		s.uploads.Inc("too_large")
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("upload exceeds storage.max_package_size (%d bytes)", tooBig.Limit))
	case errors.As(err, &ie) && ie.Kind == ingest.Conflict:
		s.uploads.Inc("conflict")
		writeError(w, http.StatusConflict, ie.Msg)
	case errors.As(err, &ie):
		s.uploads.Inc("invalid")
		body := errorBody{Error: ie.Msg}
		if ie.Kind == ingest.NotConfigured {
			body.Configured = configured
		}
		writeJSON(w, http.StatusBadRequest, body)
	case err != nil:
		s.uploads.Inc("error")
		s.internal(w, err)
	default:
		s.uploads.Inc("ok")
		s.uploadBytes.Add(float64(counter.n))
		pj := packageJSON(res.Package)
		pj.Distribution, pj.State = res.Distribution, res.State
		writeJSON(w, http.StatusOK, UploadResponse{Package: pj, Existing: res.Existing})
	}
}

// deadlineReader calls extend before every read, so the read deadline
// moves with the data.
type deadlineReader struct {
	r      io.ReadCloser
	extend func()
}

func (d *deadlineReader) Read(p []byte) (int, error) {
	d.extend()
	return d.r.Read(p)
}

func (d *deadlineReader) Close() error { return d.r.Close() }

type countingReader struct {
	r interface{ Read([]byte) (int, error) }
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// Status is the body of GET /api/v1/status.
type Status struct {
	Version         string         `json:"version"`
	ConfigHash      string         `json:"config_hash"`
	ConfigCommit    string         `json:"config_commit,omitempty"`
	CurrentSnapshot string         `json:"current_snapshot,omitempty"`
	PublishedAt     string         `json:"published_at,omitempty"`
	Sources         []SourceStatus `json:"sources"`
	OpenConflicts   int            `json:"open_conflicts"`
	HeldPackages    int            `json:"held_packages"`
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg := s.Config()
	st := Status{Version: s.Version, ConfigHash: cfg.Hash, Sources: []SourceStatus{}}
	if s.ConfigCommit != nil {
		st.ConfigCommit = s.ConfigCommit()
	}
	if cur, err := s.Store.CurrentSnapshot(ctx); err == nil {
		st.CurrentSnapshot, st.PublishedAt = cur.ID, cur.CreatedAt
	}
	if s.Syncer != nil {
		if src := s.Syncer.Status(ctx); src != nil {
			st.Sources = src
		}
	}
	cs, err := s.Store.Conflicts(ctx, false)
	if err != nil {
		s.internal(w, err)
		return
	}
	st.OpenConflicts = len(cs)
	held, err := s.Store.Placed(ctx, store.PackageFilter{States: []string{store.Held}})
	if err != nil {
		s.internal(w, err)
		return
	}
	st.HeldPackages = len(held)
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) listPackages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.PackageFilter{Format: q.Get("format"), Name: q.Get("name"), Arch: q.Get("arch"),
		Distribution: q.Get("distribution")}
	if st := q.Get("state"); st != "" {
		f.States = []string{st}
	} else if q.Get("all") == "" {
		f.States = []string{store.Active, store.Held}
	}
	pkgs, err := s.Store.Placed(r.Context(), f)
	if err != nil {
		s.internal(w, err)
		return
	}
	out := make([]PackageJSON, 0, len(pkgs))
	for _, p := range pkgs {
		out = append(out, packageJSON(p))
	}
	writeJSON(w, http.StatusOK, out)
}

// removePackage marks placements removed. version, arch and distribution
// narrow the selection; version matches the full version string.
func (s *Server) removePackage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	pkgs, err := s.Store.Placed(ctx, store.PackageFilter{Name: r.PathValue("name"), Format: q.Get("format"),
		Arch: q.Get("arch"), Distribution: q.Get("distribution"), States: []string{store.Active, store.Held}})
	if err != nil {
		s.internal(w, err)
		return
	}
	var removed []PackageJSON
	err = s.Store.Tx(ctx, func(tx *store.Queries) error {
		for _, p := range pkgs {
			if v := q.Get("version"); v != "" && v != FullVersion(p) && v != p.Version {
				continue
			}
			if _, err := tx.SetState(ctx, p.ID, p.Distribution, store.Removed); err != nil {
				return err
			}
			pj := packageJSON(p)
			pj.State = store.Removed
			removed = append(removed, pj)
		}
		return nil
	})
	if err != nil {
		s.internal(w, err)
		return
	}
	if len(removed) == 0 {
		writeError(w, http.StatusNotFound, "no matching package")
		return
	}
	s.Log.Info("removed packages", "name", r.PathValue("name"), "count", len(removed), "by", tokenFrom(r).Name)
	s.Scheduler.Request()
	writeJSON(w, http.StatusOK, removed)
}

// errApproveConflict and errApproveNeedsAdmin roll back a refused approve.
var (
	errApproveConflict   = errors.New("approve: content conflict")
	errApproveNeedsAdmin = errors.New("approve: needs admin")
)

func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pkgs, err := s.Store.Placed(ctx, store.PackageFilter{ID: r.PathValue("id"), States: []string{store.Held}})
	if err != nil {
		s.internal(w, err)
		return
	}
	if len(pkgs) == 0 {
		writeError(w, http.StatusNotFound, "no held package with that id")
		return
	}
	var conflict string
	admin := auth.HasScope(tokenFrom(r), auth.ScopeAdmin)
	err = s.Store.Tx(ctx, func(q *store.Queries) error {
		for _, p := range pkgs {
			// Approving makes the package decide its version, so it must
			// pass the content check of section 7 like a new delivery.
			ident := store.Identity{Format: p.Format, Name: p.Name, Epoch: p.Epoch,
				Version: p.Version, Release: p.Release, Arch: p.Arch}
			variants, err := q.PlacedVariants(ctx, ident, p.Distribution)
			if err != nil {
				return err
			}
			// A hold whose deliverer does not own the name publishes it
			// under another owner's name: an admin decision (section 8).
			for _, v := range variants {
				if v.ID == p.ID && v.OwnerConflict && !admin {
					return errApproveNeedsAdmin
				}
			}
			for _, v := range variants {
				if v.ID != p.ID && v.BlobSHA256 != p.BlobSHA256 && v.DecidesVersion() {
					conflict = fmt.Sprintf("%s %s is already in %s with different content (sha256 %s)",
						p.Name, p.Version, p.Distribution, v.BlobSHA256)
					return errApproveConflict
				}
			}
			if _, err := q.SetState(ctx, p.ID, p.Distribution, store.Active); err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, errApproveNeedsAdmin) {
		writeError(w, http.StatusForbidden, fmt.Sprintf(
			"%s is held because its deliverer does not own the name; approving it needs the %s scope too",
			pkgs[0].Name, auth.ScopeAdmin))
		return
	}
	if errors.Is(err, errApproveConflict) {
		writeError(w, http.StatusConflict, conflict)
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	s.Log.Info("approved package", "id", pkgs[0].ID, "name", pkgs[0].Name, "by", tokenFrom(r).Name)
	s.Scheduler.Request()
	out := make([]PackageJSON, 0, len(pkgs))
	for _, p := range pkgs {
		pj := packageJSON(p)
		pj.State = store.Active
		out = append(out, pj)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listOwners(w http.ResponseWriter, r *http.Request) {
	owners, err := s.Store.Owners(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	if owners == nil {
		owners = []store.NameOwner{}
	}
	writeJSON(w, http.StatusOK, owners)
}

// SetOwnerRequest is the body of PUT /api/v1/owners/{name}.
type SetOwnerRequest struct {
	Owner     string `json:"owner"`
	Namespace string `json:"namespace,omitempty"`
}

func (s *Server) setOwner(w http.ResponseWriter, r *http.Request) {
	var req SetOwnerRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil || req.Owner == "" {
		writeError(w, http.StatusBadRequest, `body must be {"owner": "..."}`)
		return
	}
	if req.Namespace == "" {
		req.Namespace = "os"
	}
	name := r.PathValue("name")
	if err := s.Store.SetOwner(r.Context(), req.Namespace, name, req.Owner, store.OwnerManual); err != nil {
		s.internal(w, err)
		return
	}
	s.Log.Info("owner set", "name", name, "owner", req.Owner, "by", tokenFrom(r).Name)
	o, err := s.Store.Owner(r.Context(), req.Namespace, name)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (s *Server) sync(w http.ResponseWriter, r *http.Request) {
	if s.Syncer == nil {
		writeError(w, http.StatusNotFound, "no sources configured")
		return
	}
	if err := s.Syncer.Trigger(r.PathValue("name")); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "sync started"})
}

func (s *Server) listConflicts(w http.ResponseWriter, r *http.Request) {
	cs, err := s.Store.Conflicts(r.Context(), r.URL.Query().Get("all") != "")
	if err != nil {
		s.internal(w, err)
		return
	}
	if cs == nil {
		cs = []store.Conflict{}
	}
	writeJSON(w, http.StatusOK, cs)
}

func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request) {
	snaps, err := s.Store.Snapshots(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	if snaps == nil {
		snaps = []store.Snapshot{}
	}
	writeJSON(w, http.StatusOK, snaps)
}

func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	snap, err := s.Publisher.Rollback(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) publishNow(w http.ResponseWriter, r *http.Request) {
	snap, err := s.Publisher.Publish(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) reload(w http.ResponseWriter, r *http.Request) {
	if err := s.Reload(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"config_hash": s.Config().Hash})
}

func (s *Server) gc(w http.ResponseWriter, r *http.Request) {
	res, err := s.Publisher.GC(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	ts, err := s.Store.Tokens(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	if ts == nil {
		ts = []*store.Token{}
	}
	writeJSON(w, http.StatusOK, ts)
}

// CreateTokenRequest is the body of POST /api/v1/tokens.
type CreateTokenRequest struct {
	Name   string   `json:"name"`
	Owner  string   `json:"owner"`
	Scopes []string `json:"scopes"`
}

// CreateTokenResponse carries the secret, shown once.
type CreateTokenResponse struct {
	Token  *store.Token `json:"token"`
	Secret string       `json:"secret"`
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var req CreateTokenRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	secret, t, err := auth.CreateToken(r.Context(), &s.Store.Queries, req.Name, req.Owner, req.Scopes)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.Log.Info("token created", "name", t.Name, "owner", t.Owner, "scopes", t.Scopes, "by", tokenFrom(r).Name)
	writeJSON(w, http.StatusCreated, CreateTokenResponse{Token: t, Secret: secret})
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	err := s.Store.RevokeToken(r.Context(), r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	s.Log.Info("token revoked", "name", r.PathValue("name"), "by", tokenFrom(r).Name)
	writeJSON(w, http.StatusOK, map[string]string{"revoked": r.PathValue("name")})
}
