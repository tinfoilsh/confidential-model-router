package cachesalt

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Known-answer values hardcoded in source so the construction stays pinned
// even if testdata/vectors.json is ever wrongly regenerated from this
// package. Recomputable without either implementation:
//
//	printf 'tinfoil/cache-salt/v1\x00\x00\x00\x09org_12345' | \
//	    openssl dgst -sha256 -binary | basenc --base64url | tr -d '='
const (
	// victimSalt = Derive("org_12345", "").
	victimSalt = "_1Jm3ACF8algKU89HHA1hZ57regn5axC7TwCDAkVZNY"
	// attackerSalt = Derive("org_1", "2345") — same concatenated bytes as
	// the victim's inputs; must never collide with victimSalt.
	attackerSalt = "pzER3wZKUaTw_ibSyQ5JOh0XepQ0MbQq0Nnp9Ff9VIo"
)

// vectors pins the exact construction. testdata/vectors.json comes from an
// independent implementation of the same construction; regenerating it from
// this package would defeat the cross-check. The known-answer constants
// above are the backstop if that ever happens.
type vectors struct {
	Derive []struct {
		Note            string `json:"note"`
		TenantID        string `json:"tenant_id"`
		UserCacheSecret string `json:"user_cache_secret"`
		Salt            string `json:"salt"`
		Mode            Mode   `json:"mode"`
	} `json:"derive"`
	Sum []struct {
		Tag       string   `json:"tag"`
		Fields    []string `json:"fields"`
		SHA256Hex string   `json:"sha256_hex"`
	} `json:"sum"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatalf("reading vectors: %v", err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parsing vectors: %v", err)
	}
	// Guard against a truncated or gutted file: require the full suite,
	// and specifically the adversarial boundary-shift pair.
	if len(v.Derive) < 14 || len(v.Sum) < 8 {
		t.Fatalf("vectors file truncated: %d derive / %d sum vectors", len(v.Derive), len(v.Sum))
	}
	salts := make(map[string]bool, len(v.Derive))
	for _, d := range v.Derive {
		salts[d.Salt] = true
	}
	if !salts[victimSalt] || !salts[attackerSalt] {
		t.Fatal("vectors file no longer contains the boundary-shift pair")
	}
	return v
}

func TestDeriveVectors(t *testing.T) {
	seen := make(map[string]string)
	for _, tc := range loadVectors(t).Derive {
		salt, mode := Derive(tc.TenantID, tc.UserCacheSecret)
		if salt != tc.Salt {
			t.Errorf("%s: Derive(%q, %q) = %q, want %q", tc.Note, tc.TenantID, tc.UserCacheSecret, salt, tc.Salt)
		}
		if mode != tc.Mode {
			t.Errorf("%s: mode = %q, want %q", tc.Note, mode, tc.Mode)
		}
		// Every vector must occupy a distinct namespace — the file
		// includes the adversarial pairs precisely so this check bites.
		if prev, dup := seen[salt]; dup {
			t.Errorf("salt collision between vectors %q and %q", prev, tc.Note)
		}
		seen[salt] = tc.Note
	}
}

func TestSumVectors(t *testing.T) {
	seen := make(map[string]string)
	for _, tc := range loadVectors(t).Sum {
		sum := Sum(tc.Tag, tc.Fields...)
		got := hex.EncodeToString(sum[:])
		if got != tc.SHA256Hex {
			t.Errorf("Sum(%q, %q) = %s, want %s", tc.Tag, tc.Fields, got, tc.SHA256Hex)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("digest collision between sum vectors %q and %q", prev, tc.Fields)
		}
		seen[got] = strings.Join(tc.Fields, ",")

		// Cross-link the two vector sets: the raw digest for the mode-1
		// derive inputs must encode to the pinned known-answer salt.
		if tc.Tag == SaltDomainTag && len(tc.Fields) == 1 && tc.Fields[0] == "org_12345" {
			if enc := base64.RawURLEncoding.EncodeToString(sum[:]); enc != victimSalt {
				t.Errorf("sum vector for org_12345 encodes to %q, want known-answer %q", enc, victimSalt)
			}
		}
	}
}

// TestBoundaryShift covers the attack the framing exists to stop: without
// length prefixes, ("org_1", "2345") and ("org_12345") hash identical
// concatenated bytes, letting a tenant craft a secret that joins another
// tenant's namespace.
func TestBoundaryShift(t *testing.T) {
	attacker, _ := Derive("org_1", "2345")
	victim, _ := Derive("org_12345", "")
	if attacker == victim {
		t.Fatal("boundary-shift collision: mode 2 secret reached another tenant's mode 1 namespace")
	}
	if victim != victimSalt {
		t.Errorf("Derive(\"org_12345\", \"\") = %q, want known-answer %q", victim, victimSalt)
	}
	if attacker != attackerSalt {
		t.Errorf("Derive(\"org_1\", \"2345\") = %q, want known-answer %q", attacker, attackerSalt)
	}
}

// TestMultiByteLengths pins fields long enough to exercise the second and
// third bytes of the uint32 length prefix. Without these, a truncated
// length (e.g. len&0xff) breaks framing only for fields >= 256 bytes and
// passes every short-field test.
func TestMultiByteLengths(t *testing.T) {
	salt, mode := Derive("tenant-a", strings.Repeat("s", 300)) // len 0x012c
	if want := "Hxi7oZrj2eO4WokluncuHuGGqeR4_Ofy7N8hTRgKLws"; salt != want {
		t.Errorf("300-byte secret: salt = %q, want %q", salt, want)
	}
	if mode != ModeUser {
		t.Errorf("300-byte secret: mode = %q, want ModeUser", mode)
	}

	sum := Sum("tinfoil/test/v1", strings.Repeat("t", 70000)) // len 0x011170
	if got, want := hex.EncodeToString(sum[:]), "c6aa1913f099860cde4c3852fb010b8e6b65e1221e60701569239652f190a8f6"; got != want {
		t.Errorf("70000-byte field: digest = %s, want %s", got, want)
	}
}

func TestFramingIsPrefixFree(t *testing.T) {
	tag := "tinfoil/test/v1"
	distinct := map[[32]byte]string{}
	for name, sum := range map[string][32]byte{
		`("a","bc")`:            Sum(tag, "a", "bc"),
		`("ab","c")`:            Sum(tag, "ab", "c"),
		`("abc")`:               Sum(tag, "abc"),
		`("abc","")`:            Sum(tag, "abc", ""),
		`("","abc")`:            Sum(tag, "", "abc"),
		`("a","b")`:             Sum(tag, "a", "b"),
		`("b","a") order swap`:  Sum(tag, "b", "a"),
		`no fields`:             Sum(tag),
		`("")`:                  Sum(tag, ""),
		`lp-shaped field bytes`: Sum(tag, "\x00\x00\x00\x01a\x00\x00\x00\x02bc"),
	} {
		if prev, dup := distinct[sum]; dup {
			t.Errorf("field splits %s and %s collide", prev, name)
		}
		distinct[sum] = name
	}
}

func TestDomainTagSeparation(t *testing.T) {
	if Sum("tinfoil/cache-salt/v1", "x") == Sum("tinfoil/routing-key/v1", "x") {
		t.Error("distinct domain tags produced the same hash")
	}
}

func TestDeriveModes(t *testing.T) {
	if salt, mode := Derive("", "ignored"); salt != "" || mode != ModeNone {
		t.Errorf("empty tenant: got (%q, %q), want empty salt and ModeNone", salt, mode)
	}

	tenantSalt, mode := Derive("tenant-a", "")
	if mode != ModeTenant {
		t.Errorf("empty secret: mode = %q, want ModeTenant", mode)
	}

	userSalt, mode := Derive("tenant-a", "s1")
	if mode != ModeUser {
		t.Errorf("with secret: mode = %q, want ModeUser", mode)
	}
	if userSalt == tenantSalt {
		t.Error("mode 2 salt equals the tenant's mode 1 salt")
	}

	otherUser, _ := Derive("tenant-a", "s2")
	if otherUser == userSalt {
		t.Error("different secrets in one tenant share a salt")
	}
	otherTenant, _ := Derive("tenant-b", "s1")
	if otherTenant == userSalt {
		t.Error("same secret across tenants shares a salt")
	}

	// A secret equal to another tenant's full id must not reach that
	// tenant's mode-1 namespace.
	crossSecret, _ := Derive("tenant-a", "tenant-b")
	victimTenant, _ := Derive("tenant-b", "")
	if crossSecret == victimTenant {
		t.Error("secret equal to another tenant's id joined its namespace")
	}

	// Argument order matters: Derive(a, b) and Derive(b, a) are different
	// namespaces.
	ab, _ := Derive("a", "b")
	ba, _ := Derive("b", "a")
	if ab == ba {
		t.Error("swapped arguments produced the same salt")
	}

	if again, _ := Derive("tenant-a", "s1"); again != userSalt {
		t.Error("Derive is not deterministic")
	}
}

// TestSaltFormat pins the output encoding: 43 chars of unpadded URL-safe
// base64 decoding to the full 32-byte digest. vLLM only requires a
// non-empty string, but changing the encoding re-keys every namespace.
func TestSaltFormat(t *testing.T) {
	salt, _ := Derive("tenant-a", "s1")
	if len(salt) != 43 {
		t.Errorf("salt length = %d, want 43", len(salt))
	}
	decoded, err := base64.RawURLEncoding.DecodeString(salt)
	if err != nil {
		t.Fatalf("salt is not unpadded URL-safe base64: %v", err)
	}
	if len(decoded) != 32 {
		t.Errorf("decoded salt = %d bytes, want 32", len(decoded))
	}
}

func TestBinaryFieldsAreSafe(t *testing.T) {
	// Go strings are byte strings; NULs, invalid UTF-8, and lp()-shaped
	// bytes inside a field must frame like any other bytes.
	a, _ := Derive("tenant-a", "a\x00\x00\x00\x01b")
	b, _ := Derive("tenant-a", "a")
	c, _ := Derive("tenant-a", string([]byte{0xff, 0xfe}))
	if a == b || a == c || b == c {
		t.Error("binary secrets collided")
	}
}
