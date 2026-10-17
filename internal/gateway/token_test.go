package gateway

import (
	"encoding/base64"
	"testing"
)

// TestCreateTokenWireFormat locks the existing payload independently of the Go token types.
func TestCreateTokenWireFormat(t *testing.T) {
	encoded := createToken("player", 50663625)
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"Content":{"Puid":"player","ClientVersion":50663625,"ExpiresAt":"2012-12-21T00:00:00"},"Hash":"impostor_was_here"}`
	if string(data) != want {
		t.Fatalf("token payload = %s; want %s", data, want)
	}
}

func FuzzParseTokenVersion(f *testing.F) {
	f.Add("")
	f.Add("Bearer !!!")
	f.Add("Bearer " + createToken("player", 50595550))
	f.Fuzz(func(t *testing.T, header string) {
		version, _, err := parseTokenVersion(header)
		if err == nil && (version < 1 || version > maxVersion) {
			t.Fatalf("accepted invalid version %d", version)
		}
	})
}

func BenchmarkParseTokenVersion(b *testing.B) {
	header := "Bearer " + createToken("player", 50595550)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, _, err := parseTokenVersion(header); err != nil {
			b.Fatal(err)
		}
	}
}
