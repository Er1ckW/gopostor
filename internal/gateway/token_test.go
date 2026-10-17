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
