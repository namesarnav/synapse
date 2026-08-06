package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/namesarnav/synapse/internal/persistence"
)

type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
	RoleViewer Role = "viewer"
)

var roleRank = map[Role]int{RoleViewer: 1, RoleMember: 2, RoleAdmin: 3, RoleOwner: 4}

// AtLeast reports whether r grants at least the privileges of min.
func (r Role) AtLeast(min Role) bool { return roleRank[r] >= roleRank[min] && roleRank[r] > 0 }

type User struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	CreatedAt   time.Time `json:"created_at"`
}

type Workspace struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Role      Role      `json:"role,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type Session struct {
	ID        string
	UserID    string
	ExpiresAt time.Time
}

var (
	ErrEmailTaken         = errors.New("email already registered")
	ErrInvalidCredentials = errors.New("invalid email or password")
)

const SessionTTL = 7 * 24 * time.Hour

// Store persists users, workspaces and sessions.
type Store struct{ DB *persistence.DB }

func NormalizeEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// Register creates a user and a personal workspace, returning both.
func (s *Store) Register(ctx context.Context, email, password, displayName, workspaceName string) (User, Workspace, error) {
	if err := ValidatePassword(password); err != nil {
		return User{}, Workspace{}, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, Workspace{}, err
	}
	var u User
	var w Workspace
	err = s.DB.InTx(ctx, func(tx *persistence.Tx) error {
		err := tx.QueryRow(ctx,
			`INSERT INTO users (email, password_hash, display_name) VALUES ($1,$2,$3)
			 RETURNING id::text, email, display_name, created_at`,
			NormalizeEmail(email), hash, displayName).Scan(&u.ID, &u.Email, &u.DisplayName, &u.CreatedAt)
		if persistence.IsUniqueViolation(err, "") {
			return ErrEmailTaken
		}
		if err != nil {
			return err
		}
		if workspaceName == "" {
			workspaceName = "Personal"
		}
		w, err = createWorkspace(ctx, tx, u.ID, workspaceName)
		return err
	})
	return u, w, err
}

func createWorkspace(ctx context.Context, q persistence.Querier, userID, name string) (Workspace, error) {
	var w Workspace
	if err := q.QueryRow(ctx, `INSERT INTO workspaces (name) VALUES ($1) RETURNING id::text, name, created_at`, name).
		Scan(&w.ID, &w.Name, &w.CreatedAt); err != nil {
		return w, err
	}
	w.Role = RoleOwner
	_, err := q.Exec(ctx, `INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1,$2,'owner')`, w.ID, userID)
	return w, err
}

// CreateWorkspace creates a workspace owned by userID.
func (s *Store) CreateWorkspace(ctx context.Context, userID, name string) (Workspace, error) {
	var w Workspace
	err := s.DB.InTx(ctx, func(tx *persistence.Tx) (err error) {
		w, err = createWorkspace(ctx, tx, userID, name)
		return
	})
	return w, err
}

// Authenticate verifies credentials. It spends comparable time whether or not
// the account exists to avoid leaking account existence through timing.
func (s *Store) Authenticate(ctx context.Context, email, password string) (User, error) {
	var u User
	var hash string
	err := s.DB.Pool.QueryRow(ctx,
		`SELECT id::text, email, display_name, created_at, password_hash FROM users WHERE lower(email)=$1`,
		NormalizeEmail(email)).Scan(&u.ID, &u.Email, &u.DisplayName, &u.CreatedAt, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		_, _ = VerifyPassword(password, dummyHash)
		return User{}, ErrInvalidCredentials
	}
	if err != nil {
		return User{}, err
	}
	ok, err := VerifyPassword(password, hash)
	if err != nil || !ok {
		return User{}, ErrInvalidCredentials
	}
	return u, nil
}

var dummyHash, _ = HashPassword("dummy-password-for-timing")

func hashToken(tok string) []byte {
	h := sha256.Sum256([]byte(tok))
	return h[:]
}

// CreateSession issues a new opaque session token. Only its hash is stored.
func (s *Store) CreateSession(ctx context.Context, userID, userAgent string) (token string, sess Session, err error) {
	var raw [32]byte
	if _, err = rand.Read(raw[:]); err != nil {
		return
	}
	token = base64.RawURLEncoding.EncodeToString(raw[:])
	sess = Session{UserID: userID, ExpiresAt: time.Now().Add(SessionTTL)}
	if len(userAgent) > 300 {
		userAgent = userAgent[:300]
	}
	err = s.DB.Pool.QueryRow(ctx,
		`INSERT INTO sessions (user_id, token_hash, user_agent, expires_at) VALUES ($1,$2,$3,$4) RETURNING id::text`,
		userID, hashToken(token), userAgent, sess.ExpiresAt).Scan(&sess.ID)
	return
}

// Lookup resolves a token to its session and user; revoked and expired
// sessions are not found.
func (s *Store) Lookup(ctx context.Context, token string) (User, Session, error) {
	var u User
	var sess Session
	err := s.DB.Pool.QueryRow(ctx,
		`SELECT s.id::text, s.user_id::text, s.expires_at, u.email, u.display_name, u.created_at
		   FROM sessions s JOIN users u ON u.id = s.user_id
		  WHERE s.token_hash=$1 AND s.revoked_at IS NULL AND s.expires_at > now()`,
		hashToken(token)).Scan(&sess.ID, &sess.UserID, &sess.ExpiresAt, &u.Email, &u.DisplayName, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, Session{}, persistence.ErrNotFound
	}
	u.ID = sess.UserID
	return u, sess, err
}

// Revoke invalidates a session token.
func (s *Store) Revoke(ctx context.Context, token string) error {
	_, err := s.DB.Pool.Exec(ctx, `UPDATE sessions SET revoked_at=now() WHERE token_hash=$1 AND revoked_at IS NULL`, hashToken(token))
	return err
}

// Rotate revokes the old token and issues a fresh one for the same user.
func (s *Store) Rotate(ctx context.Context, oldToken, userAgent string) (string, Session, error) {
	u, _, err := s.Lookup(ctx, oldToken)
	if err != nil {
		return "", Session{}, err
	}
	if err := s.Revoke(ctx, oldToken); err != nil {
		return "", Session{}, err
	}
	return s.CreateSession(ctx, u.ID, userAgent)
}

// PurgeExpired deletes sessions that expired or were revoked long ago.
func (s *Store) PurgeExpired(ctx context.Context) (int64, error) {
	tag, err := s.DB.Pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now() - interval '1 day'`)
	return tag.RowsAffected(), err
}

// Workspaces lists the workspaces a user belongs to.
func (s *Store) Workspaces(ctx context.Context, userID string) ([]Workspace, error) {
	rows, err := s.DB.Pool.Query(ctx,
		`SELECT w.id::text, w.name, m.role, w.created_at FROM workspace_members m
		   JOIN workspaces w ON w.id = m.workspace_id WHERE m.user_id=$1 ORDER BY w.created_at, w.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Workspace{}
	for rows.Next() {
		var w Workspace
		if err := rows.Scan(&w.ID, &w.Name, &w.Role, &w.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// RoleIn returns the user's role in a workspace, or ErrNotFound if not a member.
func (s *Store) RoleIn(ctx context.Context, userID, workspaceID string) (Role, error) {
	var r Role
	err := s.DB.Pool.QueryRow(ctx, `SELECT role FROM workspace_members WHERE user_id=$1 AND workspace_id=$2`, userID, workspaceID).Scan(&r)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", persistence.ErrNotFound
	}
	return r, err
}

// AddMember grants a role to an existing user identified by email.
func (s *Store) AddMember(ctx context.Context, workspaceID, email string, role Role) error {
	if roleRank[role] == 0 {
		return errors.New("invalid role")
	}
	tag, err := s.DB.Pool.Exec(ctx,
		`INSERT INTO workspace_members (workspace_id, user_id, role)
		 SELECT $1, id, $3 FROM users WHERE lower(email)=$2
		 ON CONFLICT (workspace_id, user_id) DO UPDATE SET role=EXCLUDED.role`, workspaceID, NormalizeEmail(email), string(role))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	return nil
}
