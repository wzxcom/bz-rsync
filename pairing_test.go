package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPairCodeEnrollmentAndRevocation(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen is required for enrollment")
	}
	r := testReceiver(t)
	hostKey, _, err := generatePairKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.stateDir, "ssh_host_ed25519_key.pub"), []byte(hostKey), 0600); err != nil {
		t.Fatal(err)
	}
	issue := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/pair-code", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.issuePairCode(w, req)
		return w
	}
	w := issue(`{"host":"example.com"}`)
	if w.Code != 200 {
		t.Fatalf("issue failed: %d %s", w.Code, w.Body.String())
	}
	var issued struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(issued.Code, "BZ1-"))
	if err != nil {
		t.Fatal(err)
	}
	var code pairCode
	if err := json.Unmarshal(raw, &code); err != nil {
		t.Fatal(err)
	}
	if code.HostKey != hostKey || code.Version != 1 || code.Host != "example.com" {
		t.Fatal("code did not pin the advertised SSH host")
	}
	data, err := os.ReadFile(filepath.Join(r.stateDir, "pairing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), code.Token) || strings.Contains(string(data), "PRIVATE KEY") {
		t.Fatal("secret was stored on receiver")
	}
	offer, err := readPairOffer(r.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Fields(offer.PublicKey)
	if line := authorizedKeyLine(r.stateDir, "bzsender", parts[0], parts[1]); !strings.Contains(line, "bz-rsync enroll ") {
		t.Fatal("bootstrap key does not require enrollment command")
	}
	regular, _, err := generatePairKey()
	if err != nil {
		t.Fatal(err)
	}
	request := command{PublicKey: regular, Token: code.Token, KeyID: identityDigest(offer.PublicKey)}
	if _, err := r.enroll(request); err != nil {
		t.Fatal(err)
	}
	if _, err := r.enroll(request); err == nil {
		t.Fatal("pair code could be reused")
	}
	parts = strings.Fields(regular)
	if line := authorizedKeyLine(r.stateDir, "bzsender", parts[0], parts[1]); !strings.Contains(line, "bz-rsync ssh ") {
		t.Fatal("long-lived key does not require normal sync command")
	}
	if w := issue(`{"host":"example.com"}`); w.Code != 409 {
		t.Fatal("existing key replacement was allowed without confirmation")
	}
	if w := issue(`{"host":"example.com","replace":true}`); w.Code != 200 {
		t.Fatalf("explicit rotation offer failed: %s", w.Body.String())
	}
	offer, _ = readPairOffer(r.stateDir)
	offer.ExpiresAt = time.Now().Add(-time.Minute)
	data, _ = json.Marshal(offer)
	if err := writeAtomic(r.stateDir, "pairing.json", data); err != nil {
		t.Fatal(err)
	}
	parts = strings.Fields(offer.PublicKey)
	if line := authorizedKeyLine(r.stateDir, "bzsender", parts[0], parts[1]); line != "" {
		t.Fatal("expired key is still authorized")
	}
	if stored, err := readPairedKey(r.stateDir); err != nil || stored != regular {
		t.Fatal("rotation offer displaced the working sender")
	}
}
