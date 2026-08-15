// Package secrets stores workspace secrets encrypted with AES-256-GCM and
// redacts their values from anything that leaves the system.
package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/namesarnav/synapse/internal/persistence"
)

var (
	ErrNotFound    = errors.New("secret not found")
	ErrInvalidName = errors.New("secret names must match [A-Za-z_][A-Za-z0-9_]{0,63}")
	ErrEmptyValue  = errors.New("secret value must not be empty")
	ErrTooLarge    = errors.New("secret value is too large")
)

const maxValueBytes = 16 << 10

var nameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// Meta describes a secret without its value.
type Meta struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store persists secrets. Values are bound to their workspace and name via the
// GCM additional data, so a row cannot be replayed under another identity.
type Store struct {
	DB   *persistence.DB
	aead cipher.AEAD

	// TTL bounds how long decrypted values are cached per workspace.
	TTL   time.Duration
	Now   func() time.Time
	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	vals    map[string]string
	expires time.Time
}

// New returns a Store using a 32-byte master key.
func New(db *persistence.DB, key []byte) (*Store, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("master key must be 32 bytes, got %d", len(key))
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	return &Store{DB: db, aead: gcm, TTL: 5 * time.Second, Now: time.Now, cache: map[string]cached{}}, nil
}

func aad(ws, name string) []byte { return []byte(ws + "/" + name) }

// ValidName reports whether name is a legal secret name.
func ValidName(name string) bool { return nameRE.MatchString(name) }

// Set creates or replaces a secret.
func (s *Store) Set(ctx context.Context, ws, name, value, userID string) error {
	if !ValidName(name) {
		return ErrInvalidName
	}
	if value == "" {
		return ErrEmptyValue
	}
	if len(value) > maxValueBytes {
		return ErrTooLarge
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	ct := s.aead.Seal(nil, nonce, []byte(value), aad(ws, name))
	var uid any
	if userID != "" {
		uid = userID
	}
	_, err := s.DB.Pool.Exec(ctx, `INSERT INTO secrets (workspace_id, name, ciphertext, nonce, created_by)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (workspace_id, name) DO UPDATE SET ciphertext=EXCLUDED.ciphertext, nonce=EXCLUDED.nonce, updated_at=now()`,
		ws, name, ct, nonce, uid)
	if err != nil {
		return err
	}
	s.invalidate(ws)
	return nil
}

// List returns secret names and timestamps, never values.
func (s *Store) List(ctx context.Context, ws string) ([]Meta, error) {
	rows, err := s.DB.Pool.Query(ctx, `SELECT name, created_at, updated_at FROM secrets WHERE workspace_id=$1 ORDER BY name`, ws)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Meta{}
	for rows.Next() {
		var m Meta
		if err := rows.Scan(&m.Name, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Delete removes a secret.
func (s *Store) Delete(ctx context.Context, ws, name string) error {
	tag, err := s.DB.Pool.Exec(ctx, `DELETE FROM secrets WHERE workspace_id=$1 AND name=$2`, ws, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.invalidate(ws)
	return nil
}

// Get returns one decrypted secret.
func (s *Store) Get(ctx context.Context, ws, name string) (string, error) {
	var ct, nonce []byte
	err := s.DB.Pool.QueryRow(ctx, `SELECT ciphertext, nonce FROM secrets WHERE workspace_id=$1 AND name=$2`, ws, name).Scan(&ct, &nonce)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	pt, err := s.aead.Open(nil, nonce, ct, aad(ws, name))
	if err != nil {
		return "", fmt.Errorf("decrypt secret %q: %w", name, err)
	}
	return string(pt), nil
}

// Load returns every decrypted secret of a workspace; it satisfies
// runtime.SecretProvider. Results are cached briefly.
func (s *Store) Load(ctx context.Context, ws string) (map[string]string, error) {
	s.mu.Lock()
	if c, ok := s.cache[ws]; ok && s.Now().Before(c.expires) {
		s.mu.Unlock()
		return c.vals, nil
	}
	s.mu.Unlock()
	rows, err := s.DB.Pool.Query(ctx, `SELECT name, ciphertext, nonce FROM secrets WHERE workspace_id=$1`, ws)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	vals := map[string]string{}
	for rows.Next() {
		var name string
		var ct, nonce []byte
		if err := rows.Scan(&name, &ct, &nonce); err != nil {
			return nil, err
		}
		pt, err := s.aead.Open(nil, nonce, ct, aad(ws, name))
		if err != nil {
			return nil, fmt.Errorf("decrypt secret %q: %w", name, err)
		}
		vals[name] = string(pt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cache[ws] = cached{vals: vals, expires: s.Now().Add(s.TTL)}
	s.mu.Unlock()
	return vals, nil
}

func (s *Store) invalidate(ws string) {
	s.mu.Lock()
	delete(s.cache, ws)
	s.mu.Unlock()
}

// Mask is what redacted values are replaced with.
const Mask = "***"

// minRedact skips very short values, which would shred unrelated text.
const minRedact = 4

// Redactor builds a replacer for a set of secret values (longest first so
// overlapping values mask fully).
func Redactor(vals map[string]string) *strings.Replacer {
	list := make([]string, 0, len(vals))
	for _, v := range vals {
		if len(v) >= minRedact {
			list = append(list, v)
		}
	}
	if len(list) == 0 {
		return nil
	}
	sort.Slice(list, func(i, j int) bool { return len(list[i]) > len(list[j]) })
	pairs := make([]string, 0, 2*len(list))
	for _, v := range list {
		pairs = append(pairs, v, Mask)
	}
	return strings.NewReplacer(pairs...)
}

// Redact walks a JSON-like value and masks secret values in every string.
func Redact(r *strings.Replacer, v any) any {
	if r == nil {
		return v
	}
	switch t := v.(type) {
	case string:
		return r.Replace(t)
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = Redact(r, t[i])
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[r.Replace(k)] = Redact(r, e)
		}
		return out
	default:
		return v
	}
}

// RedactFor loads the workspace's secrets and redacts v; it matches the
// worker's Redact hook. If secrets cannot be loaded the value is fully masked.
func (s *Store) RedactFor(workspaceID string, v any) any {
	vals, err := s.Load(context.Background(), workspaceID)
	if err != nil {
		return Mask
	}
	return Redact(Redactor(vals), v)
}
