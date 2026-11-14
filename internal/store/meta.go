package store

import (
	"context"
	"database/sql"
	"strings"
)

// Owner sources.
const (
	OwnerFirst  = "first"
	OwnerConfig = "config"
	OwnerManual = "manual"
)

// NameOwner is one row of name_owners.
type NameOwner struct {
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	Owner      string `json:"owner"`
	Source     string `json:"source"`
	AssignedAt string `json:"assigned_at"`
}

// Owner returns the owner of a name, or ErrNotFound.
func (q *Queries) Owner(ctx context.Context, namespace, name string) (*NameOwner, error) {
	var o NameOwner
	err := q.q.QueryRowContext(ctx, `SELECT namespace, name, owner, source, assigned_at FROM name_owners
		WHERE repository_id = ? AND namespace = ? AND name = ?`, RepositoryID, namespace, name).
		Scan(&o.Namespace, &o.Name, &o.Owner, &o.Source, &o.AssignedAt)
	if err != nil {
		return nil, wrapNotFound(err, "owner")
	}
	return &o, nil
}

// SetOwner assigns or transfers a name.
func (q *Queries) SetOwner(ctx context.Context, namespace, name, owner, source string) error {
	_, err := q.q.ExecContext(ctx, `INSERT INTO name_owners (repository_id, namespace, name, owner, source, assigned_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (repository_id, namespace, name) DO UPDATE
		SET owner = excluded.owner, source = excluded.source, assigned_at = excluded.assigned_at`,
		RepositoryID, namespace, name, owner, source, Now())
	return err
}

// Owners lists every owned name.
func (q *Queries) Owners(ctx context.Context) ([]NameOwner, error) {
	rows, err := q.q.QueryContext(ctx, `SELECT namespace, name, owner, source, assigned_at FROM name_owners
		WHERE repository_id = ? ORDER BY namespace, name`, RepositoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NameOwner
	for rows.Next() {
		var o NameOwner
		if err := rows.Scan(&o.Namespace, &o.Name, &o.Owner, &o.Source, &o.AssignedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// Conflict is one recorded conflict.
type Conflict struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	PackageRef string `json:"package"`
	Detail     string `json:"detail"`
	CreatedAt  string `json:"created_at"`
	ResolvedAt string `json:"resolved_at,omitempty"`
}

// AddConflict records a conflict unless an identical open one exists.
func (q *Queries) AddConflict(ctx context.Context, id, kind, ref, detail string) error {
	var n int
	if err := q.q.QueryRowContext(ctx, `SELECT count(*) FROM conflicts
		WHERE kind = ? AND package_ref = ? AND detail = ? AND resolved_at IS NULL`,
		kind, ref, detail).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := q.q.ExecContext(ctx, `INSERT INTO conflicts (id, kind, package_ref, detail, created_at)
		VALUES (?, ?, ?, ?, ?)`, id, kind, ref, detail, Now())
	return err
}

// ResolveConflicts marks open conflicts about a package reference resolved.
func (q *Queries) ResolveConflicts(ctx context.Context, ref string) error {
	_, err := q.q.ExecContext(ctx, `UPDATE conflicts SET resolved_at = ?
		WHERE package_ref = ? AND resolved_at IS NULL`, Now(), ref)
	return err
}

// Conflicts lists conflicts, only open ones unless all is set.
func (q *Queries) Conflicts(ctx context.Context, all bool) ([]Conflict, error) {
	query := `SELECT id, kind, package_ref, detail, created_at, resolved_at FROM conflicts`
	if !all {
		query += ` WHERE resolved_at IS NULL`
	}
	rows, err := q.q.QueryContext(ctx, query+` ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Conflict
	for rows.Next() {
		var c Conflict
		var resolved sql.NullString
		if err := rows.Scan(&c.ID, &c.Kind, &c.PackageRef, &c.Detail, &c.CreatedAt, &resolved); err != nil {
			return nil, err
		}
		c.ResolvedAt = nullString(resolved)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Token is a stored API token; the secret itself is never stored.
type Token struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Owner      string   `json:"owner"`
	Scopes     []string `json:"scopes"`
	CreatedAt  string   `json:"created_at"`
	LastUsedAt string   `json:"last_used_at,omitempty"`
	RevokedAt  string   `json:"revoked_at,omitempty"`
}

// InsertToken stores a token with the sha256 hex of its secret.
func (q *Queries) InsertToken(ctx context.Context, t *Token, hash string) error {
	_, err := q.q.ExecContext(ctx, `INSERT INTO tokens (id, name, owner, scopes, hash, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, t.ID, t.Name, t.Owner, strings.Join(t.Scopes, " "), hash, t.CreatedAt)
	return err
}

const tokenColumns = `id, name, owner, scopes, created_at, last_used_at, revoked_at`

func scanToken(s scanner) (*Token, error) {
	var t Token
	var scopes string
	var used, revoked sql.NullString
	if err := s.Scan(&t.ID, &t.Name, &t.Owner, &scopes, &t.CreatedAt, &used, &revoked); err != nil {
		return nil, err
	}
	t.Scopes = strings.Fields(scopes)
	t.LastUsedAt, t.RevokedAt = nullString(used), nullString(revoked)
	return &t, nil
}

// TokenByHash returns the unrevoked token whose secret hashes to hash.
func (q *Queries) TokenByHash(ctx context.Context, hash string) (*Token, error) {
	t, err := scanToken(q.q.QueryRowContext(ctx, `SELECT `+tokenColumns+` FROM tokens
		WHERE hash = ? AND revoked_at IS NULL`, hash))
	return t, wrapNotFound(err, "token")
}

// TouchToken records that a token was used.
func (q *Queries) TouchToken(ctx context.Context, id string) error {
	_, err := q.q.ExecContext(ctx, `UPDATE tokens SET last_used_at = ? WHERE id = ?`, Now(), id)
	return err
}

// Tokens lists all tokens, revoked ones included.
func (q *Queries) Tokens(ctx context.Context) ([]*Token, error) {
	rows, err := q.q.QueryContext(ctx, `SELECT `+tokenColumns+` FROM tokens ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Token
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken revokes a token by name; ErrNotFound if no active token has
// that name.
func (q *Queries) RevokeToken(ctx context.Context, name string) error {
	res, err := q.q.ExecContext(ctx, `UPDATE tokens SET revoked_at = ? WHERE name = ? AND revoked_at IS NULL`,
		Now(), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return wrapNotFound(sql.ErrNoRows, "token "+name)
	}
	return nil
}
