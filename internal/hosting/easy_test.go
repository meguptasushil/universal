package hosting

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestConnectCode(t *testing.T) {
	code := EncodeConnectCode("https://203-0-113-7.sslip.io", "tok_ABC-123")
	if !strings.HasPrefix(code, "uni1.") {
		t.Fatalf("code %q", code)
	}
	// pasted with quotes, spaces and a line break in the middle
	messy := "  \"" + code[:10] + "\n " + code[10:] + "\"  "
	s, tok, err := DecodeConnectCode(messy)
	if err != nil || s != "https://203-0-113-7.sslip.io" || tok != "tok_ABC-123" {
		t.Fatalf("decode: %q %q %v", s, tok, err)
	}
	for _, bad := range []string{"", "hello", "uni1.!!!", "uni1." + "e30", EncodeConnectCode("ftp://x", "t")} {
		if _, _, err := DecodeConnectCode(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestIPDomainAndToken(t *testing.T) {
	if d := IPDomain(net.ParseIP("203.0.113.7")); d != "203-0-113-7.sslip.io" {
		t.Fatalf("domain %q", d)
	}
	path := filepath.Join(t.TempDir(), "state", "relay-token")
	tok1, created, err := LoadOrCreateToken(path)
	if err != nil || !created || len(tok1) < 20 {
		t.Fatalf("create: %q %v %v", tok1, created, err)
	}
	tok2, created, _ := LoadOrCreateToken(path)
	if tok2 != tok1 || created {
		t.Fatal("token must persist across restarts")
	}
	if st, _ := os.Stat(path); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %v", st.Mode())
	}
}
