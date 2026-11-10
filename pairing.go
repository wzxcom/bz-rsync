package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const pairingLifetime = 10 * time.Minute

type pairOffer struct {
	PublicKey string    `json:"public_key"`
	TokenHash string    `json:"token_hash"`
	Previous  string    `json:"previous"`
	ExpiresAt time.Time `json:"expires_at"`
}

type pairCode struct {
	Version      int    `json:"v"`
	Host         string `json:"host"`
	Port         int    `json:"port"`
	HostKey      string `json:"host_key"`
	BootstrapKey string `json:"bootstrap_key"`
	Token        string `json:"token"`
}

type pairView struct {
	Active    bool       `json:"active"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Host      string     `json:"host"`
	Port      string     `json:"port"`
}

var pairingHostname = regexp.MustCompile("^[A-Za-z0-9][A-Za-z0-9.-]{0,252}$")

func pairingHost() string {
	host := strings.Trim(env("BZ_PAIR_HOST", os.Getenv("BZ_PUBLIC_SSH_BIND")), "[]")
	if ip := net.ParseIP(host); ip != nil && (ip.IsUnspecified() || ip.IsLoopback()) {
		return ""
	}
	return host
}

func validPairHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsUnspecified()
	}
	return pairingHostname.MatchString(host)
}

func keyMaterial(input string) (string, error) {
	input = strings.TrimSpace(input)
	input = strings.TrimPrefix(input, "restrict ")
	key, err := validPublicKey(input)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.TrimPrefix(key, "restrict ")), nil
}

func identityDigest(value string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

func readPairedKey(stateDir string) (string, error) {
	key, err := os.ReadFile(filepath.Join(stateDir, "authorized_keys"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return keyMaterial(string(key))
}

func readPairOffer(stateDir string) (pairOffer, error) {
	var offer pairOffer
	data, err := os.ReadFile(filepath.Join(stateDir, "pairing.json"))
	if err != nil {
		return offer, err
	}
	err = json.Unmarshal(data, &offer)
	return offer, err
}

func currentPairView(stateDir string) pairView {
	view := pairView{Host: pairingHost(), Port: env("BZ_PUBLIC_SSH_PORT", "2222")}
	if offer, err := readPairOffer(stateDir); err == nil && time.Now().Before(offer.ExpiresAt) {
		view.Active, view.ExpiresAt = true, &offer.ExpiresAt
	}
	return view
}

func generatePairKey() (public, private string, err error) {
	dir, err := os.MkdirTemp("", ".bz-pair-key-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "id_ed25519")
	if err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "bz-rsync-enrollment", "-f", file).Run(); err != nil {
		return "", "", err
	}
	pub, err := os.ReadFile(file + ".pub")
	if err != nil {
		return "", "", err
	}
	public, err = keyMaterial(string(pub))
	if err != nil {
		return "", "", err
	}
	data, err := os.ReadFile(file)
	return public, string(data), err
}

func (r *receiver) pairingBusy() bool {
	if r.manual != nil && (r.manual.Status == "queued" || r.manual.Status == "running") {
		return true
	}
	for _, job := range r.runs {
		if job.Status == "transferring" || job.Status == "verifying" {
			return true
		}
	}
	return false
}

func (r *receiver) issuePairCode(w http.ResponseWriter, req *http.Request) {
	if !isJSON(req) {
		jsonReply(w, 415, result{Error: "仅支持 JSON 请求"})
		return
	}
	var input struct {
		Host    string `json:"host"`
		Replace bool   `json:"replace"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 2048)).Decode(&input); err != nil {
		jsonReply(w, 400, result{Error: "配对请求格式无效"})
		return
	}
	host := strings.Trim(strings.TrimSpace(input.Host), "[]")
	if !validPairHost(host) {
		jsonReply(w, 400, result{Error: "请填写宝塔可访问的 NAS 主机名或 IP"})
		return
	}
	port, err := strconv.Atoi(env("BZ_PUBLIC_SSH_PORT", "2222"))
	if err != nil || port < 1 || port > 65535 {
		jsonReply(w, 500, result{Error: "SSH 接收端口配置无效"})
		return
	}
	hostPub, err := os.ReadFile(filepath.Join(r.stateDir, "ssh_host_ed25519_key.pub"))
	if err != nil {
		jsonReply(w, 503, result{Error: "SSH 主机公钥尚未就绪"})
		return
	}
	hostKey, err := keyMaterial(string(hostPub))
	if err != nil {
		jsonReply(w, 500, result{Error: "SSH 主机公钥无效"})
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	oldKey, err := readPairedKey(r.stateDir)
	if err != nil {
		jsonReply(w, 500, result{Error: "无法读取当前配对"})
		return
	}
	if oldKey != "" && !input.Replace {
		jsonReply(w, 409, result{Error: "已有发送端，生成替换配对码需要明确确认"})
		return
	}
	if oldKey != "" && r.pairingBusy() {
		jsonReply(w, 409, result{Error: "有同步正在处理，请完成后再配对"})
		return
	}
	bootstrapPublic, bootstrapPrivate, err := generatePairKey()
	if err != nil {
		jsonReply(w, 500, result{Error: "无法生成临时配对密钥"})
		return
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		jsonReply(w, 500, result{Error: "无法生成配对码"})
		return
	}
	token := base64.RawURLEncoding.EncodeToString(secret[:])
	offer := pairOffer{PublicKey: bootstrapPublic, TokenHash: identityDigest(token),
		Previous: identityDigest(oldKey), ExpiresAt: time.Now().Add(pairingLifetime)}
	data, err := json.Marshal(offer)
	if err == nil {
		err = writeAtomic(r.stateDir, "pairing.json", data)
	}
	if err != nil {
		jsonReply(w, 500, result{Error: "无法保存临时配对请求"})
		return
	}
	code := pairCode{Version: 1, Host: host, Port: port, HostKey: hostKey,
		BootstrapKey: bootstrapPrivate, Token: token}
	payload, err := json.Marshal(code)
	if err != nil {
		jsonReply(w, 500, result{Error: "无法生成配对码"})
		return
	}
	jsonReply(w, 200, struct {
		OK        bool      `json:"ok"`
		Code      string    `json:"code"`
		ExpiresAt time.Time `json:"expires_at"`
	}{true, "BZ1-" + base64.RawURLEncoding.EncodeToString(payload), offer.ExpiresAt})
}

func (r *receiver) revokePairCode(w http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := os.Remove(filepath.Join(r.stateDir, "pairing.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		jsonReply(w, 500, result{Error: "无法撤销配对码"})
		return
	}
	jsonReply(w, 200, result{OK: true})
}

func (r *receiver) enroll(c command) (result, error) {
	newKey, err := keyMaterial(c.PublicKey)
	if err != nil {
		return result{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	offer, err := readPairOffer(r.stateDir)
	if err != nil || !time.Now().Before(offer.ExpiresAt) ||
		subtle.ConstantTimeCompare([]byte(c.KeyID), []byte(identityDigest(offer.PublicKey))) != 1 ||
		subtle.ConstantTimeCompare([]byte(identityDigest(c.Token)), []byte(offer.TokenHash)) != 1 {
		return result{}, fmt.Errorf("配对码已失效或不匹配")
	}
	oldKey, err := readPairedKey(r.stateDir)
	if err != nil || offer.Previous != identityDigest(oldKey) {
		return result{}, fmt.Errorf("当前配对状态已变化，请重新生成配对码")
	}
	if oldKey != newKey && r.pairingBusy() {
		return result{}, fmt.Errorf("有同步正在处理，请完成后再配对")
	}
	// Consume first. If writing fails, the existing paired key stays valid.
	if err := os.Remove(filepath.Join(r.stateDir, "pairing.json")); err != nil {
		return result{}, err
	}
	if oldKey != newKey {
		if err := writeAtomic(r.stateDir, "authorized_keys", []byte("restrict "+newKey+"\n")); err != nil {
			return result{}, err
		}
	}
	return result{OK: true}, nil
}

func authorizedKeyLine(stateDir, user, kind, encoded string) string {
	if user != "bzsender" || kind != "ssh-ed25519" || !validEd25519(encoded) {
		return ""
	}
	requested := kind + " " + encoded
	paired, _ := readPairedKey(stateDir)
	if requested == paired {
		return fmt.Sprintf("restrict,command=\"/usr/local/bin/bz-rsync ssh %s\" %s", identityDigest(paired), paired)
	}
	offer, err := readPairOffer(stateDir)
	if err == nil && requested == offer.PublicKey && time.Now().Before(offer.ExpiresAt) {
		return fmt.Sprintf("restrict,command=\"/usr/local/bin/bz-rsync enroll %s\" %s", identityDigest(requested), requested)
	}
	return ""
}

func enrollmentCommand(keyID string) error {
	if os.Getenv("SSH_ORIGINAL_COMMAND") != "bz-enroll" {
		return fmt.Errorf("临时密钥仅用于配对")
	}
	var data command
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 4096)).Decode(&data); err != nil {
		return err
	}
	data.KeyID = keyID
	reply, err := controlCall("bz-enroll", data)
	if err != nil {
		reply.Error = err.Error()
	}
	if encodeErr := json.NewEncoder(os.Stdout).Encode(reply); encodeErr != nil {
		return encodeErr
	}
	return err
}
