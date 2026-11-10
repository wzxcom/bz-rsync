package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const sessionCookie = "bz_rsync_session"
const sessionLifetime = 12 * time.Hour

type credential struct {
	Salt []byte `json:"salt"`
	Hash []byte `json:"hash"`
}

type attempts struct {
	Count int
	Until time.Time
}

type auth struct {
	mu         sync.Mutex
	dir        string
	credential credential
	sessions   map[string]time.Time
	failures   map[string]attempts
}

func openAuth(dir string) (*auth, error) {
	a := &auth{dir: dir, sessions: make(map[string]time.Time), failures: make(map[string]attempts)}
	data, err := os.ReadFile(filepath.Join(dir, "login.json"))
	if errors.Is(err, os.ErrNotExist) {
		err = a.store("password")
		return a, err
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &a.credential); err != nil {
		return nil, err
	}
	if len(a.credential.Hash) != 32 || len(a.credential.Salt) != 16 {
		return nil, errors.New("密码状态文件无效")
	}
	return a, nil
}

func passwordHash(password string, salt []byte) []byte {
	hash, _ := pbkdf2.Key(sha256.New, password, salt, 600000, 32)
	return hash
}

func (a *auth) store(password string) error {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	next := credential{Salt: salt, Hash: passwordHash(password, salt)}
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := writeAtomic(a.dir, "login.json", data); err != nil {
		return err
	}
	a.credential = next
	return nil
}

func (a *auth) matches(password string) bool {
	return len(password) <= 256 &&
		subtle.ConstantTimeCompare(passwordHash(password, a.credential.Salt), a.credential.Hash) == 1
}

func token(req *http.Request) string {
	cookie, err := req.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func (a *auth) sessionValid(req *http.Request) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[token(req)].After(time.Now())
}

func newToken() (string, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(random[:]), nil
}

func setSessionCookie(w http.ResponseWriter, req *http.Request, value string, age int) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: value, Path: "/", MaxAge: age,
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: req.TLS != nil || strings.EqualFold(req.Header.Get("X-Forwarded-Proto"), "https"),
	})
}

func (a *auth) login(w http.ResponseWriter, req *http.Request) {
	var input struct {
		Password string `json:"password"`
	}
	if !readAuthRequest(w, req, &input) {
		return
	}
	ip, _, _ := net.SplitHostPort(req.RemoteAddr)
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, attempt := range a.failures {
		if !attempt.Until.After(now) {
			delete(a.failures, key)
		}
	}
	attempt := a.failures[ip]
	if attempt.Count >= 5 || len(a.failures) >= 128 {
		jsonReply(w, 429, result{Error: "尝试次数过多，请稍后再试"})
		return
	}
	if !a.matches(input.Password) {
		if attempt.Count == 0 {
			attempt.Until = now.Add(time.Minute)
		}
		attempt.Count++
		a.failures[ip] = attempt
		jsonReply(w, 401, result{Error: "密码不正确"})
		return
	}
	delete(a.failures, ip)
	for key, expires := range a.sessions {
		if !expires.After(now) {
			delete(a.sessions, key)
		}
	}
	if len(a.sessions) >= 64 {
		jsonReply(w, 429, result{Error: "活动会话数量已达上限"})
		return
	}
	value, err := newToken()
	if err != nil {
		jsonReply(w, 500, result{Error: "无法创建登录会话"})
		return
	}
	delete(a.sessions, token(req))
	a.sessions[value] = now.Add(sessionLifetime)
	setSessionCookie(w, req, value, int(sessionLifetime.Seconds()))
	jsonReply(w, 200, result{OK: true})
}

func (a *auth) change(w http.ResponseWriter, req *http.Request) {
	var input struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if !readAuthRequest(w, req, &input) {
		return
	}
	if utf8.RuneCountInString(input.New) < 8 || len(input.New) > 256 || input.New == "password" {
		jsonReply(w, 400, result{Error: "请使用至少 8 个字符的新密码，最多 256 字节，且避开默认密码"})
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.sessions[token(req)].After(time.Now()) {
		jsonReply(w, 401, result{Error: "登录已失效"})
		return
	}
	if !a.matches(input.Current) {
		jsonReply(w, 400, result{Error: "当前密码不正确"})
		return
	}
	value, err := newToken()
	if err == nil {
		err = a.store(input.New)
	}
	if err != nil {
		jsonReply(w, 500, result{Error: "无法保存新密码"})
		return
	}
	clear(a.sessions)
	a.sessions[value] = time.Now().Add(sessionLifetime)
	setSessionCookie(w, req, value, int(sessionLifetime.Seconds()))
	jsonReply(w, 200, result{OK: true})
}

func (a *auth) logout(w http.ResponseWriter, req *http.Request) {
	a.mu.Lock()
	delete(a.sessions, token(req))
	a.mu.Unlock()
	setSessionCookie(w, req, "", -1)
	jsonReply(w, 200, result{OK: true})
}

func readAuthRequest(w http.ResponseWriter, req *http.Request, input any) bool {
	if !isJSON(req) {
		jsonReply(w, 415, result{Error: "仅支持 JSON 请求"})
		return false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4096)).Decode(input); err != nil {
		jsonReply(w, 400, result{Error: "请求数据无效"})
		return false
	}
	return true
}
