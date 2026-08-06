package auth

import (
	"strings"
	"testing"
)

func TestPasswordHashVerify(t *testing.T) {
	h, err := HashPassword("correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := VerifyPassword("correct-horse-battery", h); !ok || err != nil {
		t.Fatalf("verify good: %v %v", ok, err)
	}
	if ok, _ := VerifyPassword("wrong", h); ok {
		t.Fatal("wrong password verified")
	}
	h2, _ := HashPassword("correct-horse-battery")
	if h == h2 {
		t.Fatal("salts must differ")
	}
	for _, bad := range []string{"", "plain", "$argon2id$v=19$m=1$x", "$bcrypt$v=19$m=1,t=1,p=1$YQ$YQ"} {
		if _, err := VerifyPassword("x", bad); err == nil {
			t.Errorf("malformed hash %q accepted", bad)
		}
	}
}

func TestPasswordPolicy(t *testing.T) {
	if ValidatePassword("short") == nil {
		t.Fatal("short password accepted")
	}
	if ValidatePassword(strings.Repeat("a", MaxPasswordLen+1)) == nil {
		t.Fatal("overlong password accepted")
	}
	if err := ValidatePassword("long-enough-pass"); err != nil {
		t.Fatal(err)
	}
}

func TestRoleAtLeast(t *testing.T) {
	cases := []struct {
		r, min Role
		want   bool
	}{
		{RoleOwner, RoleAdmin, true}, {RoleAdmin, RoleAdmin, true}, {RoleMember, RoleAdmin, false},
		{RoleViewer, RoleViewer, true}, {RoleViewer, RoleMember, false}, {"bogus", RoleViewer, false}, {"", RoleViewer, false},
	}
	for _, c := range cases {
		if got := c.r.AtLeast(c.min); got != c.want {
			t.Errorf("%q.AtLeast(%q)=%v want %v", c.r, c.min, got, c.want)
		}
	}
}
