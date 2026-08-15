package secrets

import (
	"context"
	"errors"
	"testing"

	"github.com/namesarnav/synapse/internal/auth"
	"github.com/namesarnav/synapse/internal/testutil"
)

func newStore(t *testing.T) (*Store, string, string) {
	t.Helper()
	db := testutil.NewDB(t)
	st, err := New(db, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	a := &auth.Store{DB: db}
	mk := func(email string) string {
		_, ws, err := a.Register(context.Background(), email, "correct-horse-battery", "T", "ws")
		if err != nil {
			t.Fatal(err)
		}
		return ws.ID
	}
	return st, mk("a@example.com"), mk("b@example.com")
}

func TestSetGetLoadDelete(t *testing.T) {
	st, ws, other := newStore(t)
	ctx := context.Background()
	if err := st.Set(ctx, ws, "API_KEY", "s3cr3t-value", ""); err != nil {
		t.Fatal(err)
	}
	if v, err := st.Get(ctx, ws, "API_KEY"); err != nil || v != "s3cr3t-value" {
		t.Fatalf("get: %q %v", v, err)
	}
	if _, err := st.Get(ctx, other, "API_KEY"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-workspace get: %v", err)
	}
	// Ciphertext at rest must not contain the plaintext.
	var ct []byte
	if err := st.DB.Pool.QueryRow(ctx, `SELECT ciphertext FROM secrets WHERE workspace_id=$1`, ws).Scan(&ct); err != nil {
		t.Fatal(err)
	}
	if string(ct) == "s3cr3t-value" || len(ct) <= len("s3cr3t-value") {
		t.Fatal("secret stored unencrypted")
	}
	// Overwrite is visible after the cache is invalidated.
	if _, err := st.Load(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if err := st.Set(ctx, ws, "API_KEY", "rotated-value", ""); err != nil {
		t.Fatal(err)
	}
	m, err := st.Load(ctx, ws)
	if err != nil || m["API_KEY"] != "rotated-value" {
		t.Fatalf("load: %v %v", m, err)
	}
	list, _ := st.List(ctx, ws)
	if len(list) != 1 || list[0].Name != "API_KEY" {
		t.Fatalf("list = %+v", list)
	}
	if err := st.Delete(ctx, ws, "API_KEY"); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, ws, "API_KEY"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestTamperedRowFailsToDecrypt(t *testing.T) {
	st, ws, _ := newStore(t)
	ctx := context.Background()
	_ = st.Set(ctx, ws, "A_SECRET", "value-one", "")
	_ = st.Set(ctx, ws, "B_SECRET", "value-two", "")
	// Swap ciphertexts between names: AAD binding must reject it.
	if _, err := st.DB.Pool.Exec(ctx, `UPDATE secrets SET ciphertext = (SELECT ciphertext FROM secrets WHERE name='B_SECRET'),
		nonce = (SELECT nonce FROM secrets WHERE name='B_SECRET') WHERE name='A_SECRET'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(ctx, ws, "A_SECRET"); err == nil {
		t.Fatal("swapped ciphertext decrypted")
	}
}

func TestValidation(t *testing.T) {
	st, ws, _ := newStore(t)
	ctx := context.Background()
	for _, n := range []string{"", "1abc", "has space", "a-b"} {
		if err := st.Set(ctx, ws, n, "value", ""); !errors.Is(err, ErrInvalidName) {
			t.Errorf("name %q: %v", n, err)
		}
	}
	if err := st.Set(ctx, ws, "OK", "", ""); !errors.Is(err, ErrEmptyValue) {
		t.Fatalf("empty: %v", err)
	}
	if _, err := New(st.DB, []byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestRedact(t *testing.T) {
	r := Redactor(map[string]string{"A": "hunter22", "B": "hunter22-extended", "C": "ab"})
	in := map[string]any{
		"msg":      "token hunter22-extended and hunter22 and ab",
		"list":     []any{"x hunter22", 5, true},
		"hunter22": map[string]any{"nested": "hunter22"},
	}
	out := Redact(r, in).(map[string]any)
	if out["msg"] != "token *** and *** and ab" {
		t.Fatalf("msg = %v", out["msg"])
	}
	if out["list"].([]any)[0] != "x ***" || out["list"].([]any)[1] != 5 {
		t.Fatalf("list = %v", out["list"])
	}
	if _, ok := out["***"]; !ok {
		t.Fatalf("key not redacted: %v", out)
	}
	if Redactor(map[string]string{"x": "ab"}) != nil {
		t.Fatal("short values must not create a redactor")
	}
	if Redact(nil, "plain") != "plain" {
		t.Fatal("nil redactor must be identity")
	}
}
