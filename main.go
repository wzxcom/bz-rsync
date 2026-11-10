package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var remoteRsync = regexp.MustCompile("^rsync --server .+ \\. ([a-f0-9]{32})/?$")

//go:embed VERSION
var versionFile string

func releaseVersion() string {
	return "v" + strings.TrimSpace(versionFile)
}

func main() {
	var err error
	switch {
	case len(os.Args) == 3 && os.Args[1] == "ssh":
		key, readErr := readPairedKey(env("BZ_STATE", "/state"))
		if readErr != nil || key == "" || identityDigest(key) != os.Args[2] {
			err = fmt.Errorf("发送端配对已失效")
		} else {
			err = sshCommand()
		}
	case len(os.Args) == 3 && os.Args[1] == "enroll":
		err = enrollmentCommand(os.Args[2])
	case len(os.Args) == 5 && os.Args[1] == "authorized-keys":
		if line := authorizedKeyLine(env("BZ_STATE", "/state"), os.Args[2], os.Args[3], os.Args[4]); line != "" {
			fmt.Println(line)
		}
	case len(os.Args) == 2 && os.Args[1] == "healthcheck":
		err = healthcheck()
	case len(os.Args) == 1:
		err = serve()
	default:
		err = fmt.Errorf("用法：bz-rsync [healthcheck]")
	}
	if err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func socketPath() string {
	return env("BZ_SOCKET", "/run/bz-rsync/control.sock")
}

func serve() error {
	stateDir := env("BZ_STATE", "/state")
	layout := env("BZ_LAYOUT", "dated")
	if layout != "dated" && layout != "split" {
		return fmt.Errorf("BZ_LAYOUT 必须是 dated 或 split")
	}
	r, err := openReceiverLayout(stateDir, env("BZ_BACKUPS", "/backups"), layout == "split")
	if err != nil {
		return err
	}
	login, err := openAuth(stateDir)
	if err != nil {
		return err
	}
	socket := socketPath()
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(socket, 0600); err != nil {
		return err
	}
	control := &http.Server{Handler: http.HandlerFunc(r.control), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := control.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	go r.retentionWorker()
	go r.telegramWorker()
	r.scheduleCleanup()
	handler := r.web(login, env("BZ_UI", "/app/index.html"))
	server := &http.Server{
		Addr: env("BZ_WEB_ADDR", ":8080"), Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute,
	}
	webListener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}
	defer webListener.Close()
	sshEnabled := os.Getenv("BZ_EXPECT_SSH") == "1"
	if sshEnabled {
		deadline := time.Now().Add(5 * time.Second)
		for {
			err = probeSSH()
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("SSH 接收服务未就绪：%w", err)
			}
			time.Sleep(150 * time.Millisecond)
		}
	}
	printStartup(webListener, sshEnabled)
	telegramContext, stopTelegram := context.WithCancel(context.Background())
	defer stopTelegram()
	go r.telegramCommandWorker(telegramContext)
	return server.Serve(webListener)
}

func probeSSH() error {
	connection, err := net.DialTimeout("tcp", "127.0.0.1:2222", time.Second)
	if err != nil {
		return err
	}
	return connection.Close()
}

func printStartup(listener net.Listener, sshEnabled bool) {
	port := env("BZ_PUBLIC_WEB_PORT", fmt.Sprint(listener.Addr().(*net.TCPAddr).Port))
	host := env("BZ_PUBLIC_WEB_BIND", "127.0.0.1")
	if host == "0.0.0.0" {
		host = "<NAS IP>"
	}
	fmt.Println("----------------------------------------")
	fmt.Println("bz-rsync 接收端启动成功")
	fmt.Printf("应用版本  %s\n", releaseVersion())
	fmt.Printf("管理页面地址  http://%s\n", net.JoinHostPort(host, port))
	fmt.Printf("管理页面端口  %s\n", port)
	if sshEnabled {
		fmt.Printf("同步监听端口  %s (SSH)\n", env("BZ_PUBLIC_SSH_PORT", "2222"))
		fmt.Println("接收端同步服务已开启")
	}
	fmt.Println("发送端由宝塔服务器独立运行")
	fmt.Println("----------------------------------------")
}

func healthcheck() error {
	client := http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://127.0.0.1:8080/healthz")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("管理页面健康检查返回 %d", response.StatusCode)
	}
	return probeSSH()
}

func jsonReply(w http.ResponseWriter, status int, response any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Print(err)
	}
}

func (r *receiver) control(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var data command
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&data); err != nil {
		jsonReply(w, 400, result{Error: "命令数据无效"})
		return
	}
	action := strings.TrimPrefix(req.URL.Path, "/")
	response, err := r.execute(action, data)
	if err != nil {
		jsonReply(w, 409, result{Error: err.Error()})
		return
	}
	jsonReply(w, 200, response)
}

func (r *receiver) web(login *auth, ui string) http.Handler {
	uiHandler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.ServeFile(w, req, ui)
	})
	mux := http.NewServeMux()
	mux.Handle("GET /{$}", uiHandler)
	mux.HandleFunc("GET /downloads/sender.zip", senderDownload)
	mux.HandleFunc("POST /api/login", login.login)
	mux.HandleFunc("POST /api/logout", login.logout)
	mux.HandleFunc("POST /api/password", login.change)
	mux.HandleFunc("GET /api/state", r.state)
	mux.HandleFunc("POST /api/key", r.addKey)
	mux.HandleFunc("DELETE /api/key", r.deleteKey)
	mux.HandleFunc("POST /api/retention", r.setRetention)
	mux.HandleFunc("POST /api/paths", r.setPaths)
	mux.HandleFunc("POST /api/sources", r.setSources)
	mux.HandleFunc("POST /api/manual-sync", r.requestManual)
	mux.HandleFunc("POST /api/run-action", r.runAction)
	mux.HandleFunc("GET /api/telegram", r.getTelegram)
	mux.HandleFunc("POST /api/telegram", r.setTelegram)
	mux.HandleFunc("POST /api/telegram/test", r.testTelegram)
	mux.HandleFunc("POST /api/pair-code", r.issuePairCode)
	mux.HandleFunc("DELETE /api/pair-code", r.revokePairCode)
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		if req.URL.Path == "/healthz" && req.Method == http.MethodGet {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		public := (req.URL.Path == "/" && req.Method == http.MethodGet) ||
			(req.URL.Path == "/api/login" && req.Method == http.MethodPost)
		if !public && !login.sessionValid(req) {
			jsonReply(w, 401, result{Error: "请先登录"})
			return
		}
		mux.ServeHTTP(w, req)
	})
}

func senderDownload(w http.ResponseWriter, req *http.Request) {
	name := "bz-rsync-sender-" + releaseVersion() + ".zip"
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Content-Type", "application/zip")
	http.ServeFile(w, req, env("BZ_SENDER_DOWNLOAD", "/app/downloads/sender.zip"))
}

func (r *receiver) state(w http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	_, keyErr := os.Stat(filepath.Join(r.stateDir, "authorized_keys"))
	if keyErr != nil && !errors.Is(keyErr, os.ErrNotExist) {
		r.mu.Unlock()
		jsonReply(w, 500, result{Error: "无法读取发送端配对状态"})
		return
	}
	response := struct {
		Runs         []run                  `json:"runs"`
		Paired       bool                   `json:"paired"`
		Days         int                    `json:"retention_days"`
		LastCleanup  *time.Time             `json:"last_cleanup,omitempty"`
		CleanupError string                 `json:"cleanup_error,omitempty"`
		WebPort      string                 `json:"web_port"`
		SSHPort      string                 `json:"ssh_port"`
		Split        bool                   `json:"split_layout"`
		Paths        pathView               `json:"paths"`
		Sources      sourceSettings         `json:"sources"`
		Version      string                 `json:"version"`
		Manual       *manualSync            `json:"manual_sync"`
		PairCode     pairView               `json:"pair_code"`
		Scheduled    *scheduledConfirmation `json:"scheduled_confirmation,omitempty"`
	}{
		r.runs, keyErr == nil, r.retention.Days, r.retention.LastCleanup, r.retention.CleanupError,
		env("BZ_PUBLIC_WEB_PORT", "8080"), env("BZ_PUBLIC_SSH_PORT", "2222"), r.split, r.pathView(), r.sources, releaseVersion(), r.manual, currentPairView(r.stateDir),
		r.scheduled,
	}
	data, err := json.Marshal(response)
	r.mu.Unlock()
	if err != nil {
		jsonReply(w, 500, result{Error: "无法读取同步状态"})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(data)
}

func validEd25519(encoded string) bool {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	return err == nil && len(raw) == 51 &&
		bytes.Equal(raw[:19], []byte("\x00\x00\x00\x0bssh-ed25519\x00\x00\x00\x20"))
}

func (r *receiver) addKey(w http.ResponseWriter, req *http.Request) {
	var input struct {
		Key string `json:"key"`
	}
	if !isJSON(req) {
		jsonReply(w, 415, result{Error: "仅支持 JSON 请求"})
		return
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4096)).Decode(&input); err != nil {
		jsonReply(w, 400, result{Error: "公钥请求无效"})
		return
	}
	key, err := validPublicKey(input.Key)
	if err != nil {
		jsonReply(w, 400, result{Error: err.Error()})
		return
	}
	r.mu.Lock()
	err = writeAtomic(r.stateDir, "authorized_keys", []byte(key))
	r.mu.Unlock()
	if err != nil {
		log.Print(err)
		jsonReply(w, 500, result{Error: "无法保存发送端公钥"})
		return
	}
	jsonReply(w, 200, result{OK: true})
}

func isJSON(req *http.Request) bool {
	return strings.TrimSpace(strings.SplitN(req.Header.Get("Content-Type"), ";", 2)[0]) == "application/json"
}

func (r *receiver) deleteKey(w http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	err := os.Remove(filepath.Join(r.stateDir, "authorized_keys"))
	r.mu.Unlock()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Print(err)
		jsonReply(w, 500, result{Error: "无法撤销公钥"})
		return
	}
	jsonReply(w, 200, result{OK: true})
}

func controlCall(action string, data command) (result, error) {
	body, err := json.Marshal(data)
	if err != nil {
		return result{}, err
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	client := &http.Client{Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath())
		},
	}}
	if action != "bz-finish" && action != "bz-start" {
		client.Timeout = 15 * time.Second
	}
	response, err := client.Post("http://local/"+action, "application/json", bytes.NewReader(body))
	if err != nil {
		return result{}, err
	}
	defer response.Body.Close()
	var out result
	if err := json.NewDecoder(io.LimitReader(response.Body, 32<<10)).Decode(&out); err != nil {
		return result{}, err
	}
	if response.StatusCode != 200 {
		return out, errors.New(out.Error)
	}
	return out, nil
}

func sshCommand() error {
	original := os.Getenv("SSH_ORIGINAL_COMMAND")
	switch original {
	case "bz-ping", "bz-config", "bz-config-result", "bz-manual-claim", "bz-manual-result",
		"bz-start", "bz-progress", "bz-finish", "bz-fail", "bz-run-control":
		var data command
		if original != "bz-ping" && original != "bz-config" && original != "bz-manual-claim" {
			if err := json.NewDecoder(io.LimitReader(os.Stdin, 16<<10)).Decode(&data); err != nil {
				return err
			}
		}
		reply, err := controlCall(original, data)
		if err != nil {
			reply.Error = err.Error()
		}
		if encodeErr := json.NewEncoder(os.Stdout).Encode(reply); encodeErr != nil {
			return encodeErr
		}
		return err
	}
	if len(original) > 4096 {
		return fmt.Errorf("传输命令过长")
	}
	match := remoteRsync.FindStringSubmatchIndex(original)
	if match == nil {
		return fmt.Errorf("仅允许 bz-rsync 命令和批次上传")
	}
	id := original[match[2]:match[3]]
	response, err := controlCall("claim", command{RunID: id})
	if err != nil {
		return err
	}
	child := exec.Command("/usr/bin/rrsync", "-wo", "-no-del", "-munge", response.Directory)
	child.Env = append(os.Environ(), "SSH_ORIGINAL_COMMAND="+original[:match[2]]+"./")
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = child.Run()
	release := command{RunID: id}
	if err != nil {
		release.Error = err.Error()
	}
	_, releaseErr := controlCall("release", release)
	return errors.Join(err, releaseErr)
}
