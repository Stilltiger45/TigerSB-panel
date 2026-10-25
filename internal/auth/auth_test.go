package auth

import (
	"strings"
	"testing"
	"time"
)

func TestHashAndVerify(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$") {
		t.Fatalf("unexpected hash format: %s", h)
	}
	ok, err := VerifyPassword(h, "correct horse battery")
	if err != nil || !ok {
		t.Fatalf("right password rejected: ok=%v err=%v", ok, err)
	}
	ok, err = VerifyPassword(h, "wrong password")
	if err != nil || ok {
		t.Fatalf("wrong password accepted: ok=%v err=%v", ok, err)
	}
	h2, _ := HashPassword("correct horse battery")
	if h == h2 {
		t.Fatal("two hashes of the same password must differ (salt)")
	}
}

func TestVerifyRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "plain", "$argon2i$v=19$m=1,t=1,p=1$a$b", "$argon2id$v=19$m=x$a$b"} {
		if _, err := VerifyPassword(bad, "x"); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestTokenHash(t *testing.T) {
	tok, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if HashToken(tok) != hash {
		t.Fatal("HashToken must match the hash returned by NewToken")
	}
	tok2, _, _ := NewToken()
	if tok == tok2 {
		t.Fatal("tokens must be random")
	}
}

func TestLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewLimiter(3, time.Minute, 10*time.Minute)
	l.now = func() time.Time { return now }

	for i := 0; i < 2; i++ {
		l.Fail("k")
	}
	if ok, _ := l.Allowed("k"); !ok {
		t.Fatal("blocked before reaching the limit")
	}
	l.Fail("k")
	if ok, wait := l.Allowed("k"); ok || wait != 10*time.Minute {
		t.Fatalf("expected 10m lockout, got ok=%v wait=%v", ok, wait)
	}
	if ok, _ := l.Allowed("other"); !ok {
		t.Fatal("other keys must not be blocked")
	}
	now = now.Add(11 * time.Minute)
	if ok, _ := l.Allowed("k"); !ok {
		t.Fatal("lockout must expire")
	}
	l.Fail("k")
	l.Reset("k")
	l.Fail("k")
	l.Fail("k")
	if ok, _ := l.Allowed("k"); !ok {
		t.Fatal("Reset must clear earlier failures")
	}
}
