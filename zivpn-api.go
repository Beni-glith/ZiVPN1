package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ConfigFile             = "/etc/zivpn/config.json"
	UserDB                 = "/etc/zivpn/users.json"
	UserIPSessionsFile     = "/etc/zivpn/user_ip_sessions.json"
	MultiLoginNotifiedFile = "/etc/zivpn/multi_login_notified.json"
	BotConfigFile          = "/etc/zivpn/bot-config.json"
	DomainFile             = "/etc/zivpn/domain"
	ApiKeyFile             = "/etc/zivpn/apikey"
	Port                   = "/etc/zivpn/api_port"
)

var AuthToken = "AutoFtBot-agskjgdvsbdreiWG1234512SDKrqw"

type Config struct {
	Listen string `json:"listen"`
	Cert   string `json:"cert"`
	Key    string `json:"key"`
	Obfs   string `json:"obfs"`
	Auth   struct {
		Mode   string   `json:"mode"`
		Type   string   `json:"type"`
		Config []string `json:"config"`
	} `json:"auth"`
}

type UserRequest struct {
	Password string `json:"password"`
	Username string `json:"username"`
	Days     int    `json:"days"`
	IpLimit  int    `json:"ip_limit"`
}

type UserStore struct {
	Password     string `json:"password"`
	Expired      string `json:"expired"`
	Status       string `json:"status"`
	IpLimit      int    `json:"ip_limit"`
	ManualLocked bool   `json:"manual_locked"`
}

type Response struct {
	Success bool        `json:"success"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

var mutex = &sync.Mutex{}
var sessionMutex = &sync.Mutex{}
var notifyMutex = &sync.Mutex{}
var ispCache = &sync.Mutex{}
var cachedIsp = make(map[string]cachedISP)

type cachedISP struct {
	Name      string
	ExpiresAt time.Time
}

func main() {
	port := flag.Int("port", 8080, "Port to run the API server on")
	flag.Parse()

	if keyBytes, err := ioutil.ReadFile(ApiKeyFile); err == nil {
		AuthToken = strings.TrimSpace(string(keyBytes))
	}

	http.HandleFunc("/api/user/create", authMiddleware(createUser))
	http.HandleFunc("/api/user/delete", authMiddleware(deleteUser))
	http.HandleFunc("/api/user/renew", authMiddleware(renewUser))
	http.HandleFunc("/api/user/lock", authMiddleware(lockUser))
	http.HandleFunc("/api/user/unlock", authMiddleware(unlockUser))
	http.HandleFunc("/api/users", authMiddleware(listUsers))
	http.HandleFunc("/api/info", authMiddleware(getSystemInfo))
	http.HandleFunc("/api/cron/expire", authMiddleware(checkExpiration))
	http.HandleFunc("/api/cron/cleanup", authMiddleware(cleanupExpired))
	http.HandleFunc("/auth/hysteria", authHysteria)

	log.Printf("Server started at :%d", *port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", *port), nil))
}

func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-API-Key")
		if token != AuthToken {
			jsonResponse(w, http.StatusUnauthorized, false, "Unauthorized", nil)
			return
		}
		next(w, r)
	}
}

func jsonResponse(w http.ResponseWriter, status int, success bool, message string, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(Response{
		Success: success,
		Message: message,
		Data:    data,
	})
}

func createUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonResponse(w, http.StatusMethodNotAllowed, false, "Method not allowed", nil)
		return
	}

	var req UserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, false, "Invalid request body", nil)
		return
	}

	if req.Password == "" || req.Days <= 0 {
		jsonResponse(w, http.StatusBadRequest, false, "Password dan days harus valid", nil)
		return
	}

	mutex.Lock()
	defer mutex.Unlock()

	ipLimit := req.IpLimit
	if ipLimit <= 0 {
		ipLimit = 1
	}

	config, configErr := loadConfig()
	if configErr == nil && usesPasswordAuth(config) {
		for _, p := range config.Auth.Config {
			if p == req.Password {
				jsonResponse(w, http.StatusConflict, false, "User sudah ada", nil)
				return
			}
		}
	}

	expDate := time.Now().Add(time.Duration(req.Days) * 24 * time.Hour).Format("2006-01-02")

	users, err := loadUsers()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, false, "Gagal membaca database user", nil)
		return
	}

	for _, u := range users {
		if u.Password == req.Password {
			jsonResponse(w, http.StatusConflict, false, "User sudah ada", nil)
			return
		}
	}

	newUser := UserStore{
		Password:     req.Password,
		Expired:      expDate,
		Status:       "active",
		IpLimit:      ipLimit,
		ManualLocked: false,
	}
	users = append(users, newUser)

	if err := saveUsers(users); err != nil {
		jsonResponse(w, http.StatusInternalServerError, false, "Gagal menyimpan database user", nil)
		return
	}

	if configErr == nil && usesPasswordAuth(config) {
		config.Auth.Config = append(config.Auth.Config, req.Password)
		if err := saveConfig(config); err != nil {
			jsonResponse(w, http.StatusInternalServerError, false, "Gagal menyimpan config", nil)
			return
		}
		if err := restartService(); err != nil {
			jsonResponse(w, http.StatusInternalServerError, false, "Gagal merestart service", nil)
			return
		}
	}

	domain := "Tidak diatur"
	if domainBytes, err := ioutil.ReadFile(DomainFile); err == nil {
		domain = strings.TrimSpace(string(domainBytes))
	}

	jsonResponse(w, http.StatusOK, true, "User berhasil dibuat", map[string]interface{}{
		"password": req.Password,
		"expired":  expDate,
		"domain":   domain,
		"ip_limit": ipLimit,
	})
}

func deleteUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonResponse(w, http.StatusMethodNotAllowed, false, "Method not allowed", nil)
		return
	}

	var req UserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, false, "Invalid request body", nil)
		return
	}

	mutex.Lock()
	defer mutex.Unlock()

	foundInConfig := false
	newConfigAuth := []string{}
	config, err := loadConfig()
	if err == nil && usesPasswordAuth(config) {
		for _, p := range config.Auth.Config {
			if p == req.Password {
				foundInConfig = true
			} else {
				newConfigAuth = append(newConfigAuth, p)
			}
		}

		if foundInConfig {
			config.Auth.Config = newConfigAuth
			if err := saveConfig(config); err != nil {
				jsonResponse(w, http.StatusInternalServerError, false, "Gagal menyimpan config", nil)
				return
			}
		}
	}

	users, err := loadUsers()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, false, "Gagal membaca database user", nil)
		return
	}

	foundInDB := false
	newUsers := []UserStore{}
	for _, u := range users {
		if u.Password == req.Password {
			foundInDB = true
			continue
		}
		newUsers = append(newUsers, u)
	}

	if !foundInConfig && !foundInDB {
		jsonResponse(w, http.StatusNotFound, false, "User tidak ditemukan", nil)
		return
	}

	if foundInDB {
		if err := saveUsers(newUsers); err != nil {
			jsonResponse(w, http.StatusInternalServerError, false, "Gagal menyimpan database user", nil)
			return
		}
	}

	if foundInConfig {
		if err := restartService(); err != nil {
			jsonResponse(w, http.StatusInternalServerError, false, "Gagal merestart service", nil)
			return
		}
	}

	jsonResponse(w, http.StatusOK, true, "User berhasil dihapus", nil)
}

func renewUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonResponse(w, http.StatusMethodNotAllowed, false, "Method not allowed", nil)
		return
	}

	var req UserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, false, "Invalid request body", nil)
		return
	}

	mutex.Lock()
	defer mutex.Unlock()

	users, err := loadUsers()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, false, "Gagal membaca database user", nil)
		return
	}

	found := false
	newUsers := []UserStore{}
	var newExpDate string

	for _, u := range users {
		if u.Password == req.Password {
			found = true
			currentExp, err := time.Parse("2006-01-02", u.Expired)
			if err != nil {
				currentExp = time.Now()
			}

			if currentExp.Before(time.Now()) {
				currentExp = time.Now()
			}

			newExp := currentExp.Add(time.Duration(req.Days) * 24 * time.Hour)
			newExpDate = newExp.Format("2006-01-02")

			u.Expired = newExpDate

			newUsers = append(newUsers, u)
		} else {
			newUsers = append(newUsers, u)
		}
	}

	if !found {
		jsonResponse(w, http.StatusNotFound, false, "User tidak ditemukan di database", nil)
		return
	}

	if err := saveUsers(newUsers); err != nil {
		jsonResponse(w, http.StatusInternalServerError, false, "Gagal menyimpan database user", nil)
		return
	}

	config, err := loadConfig()
	if err == nil && usesPasswordAuth(config) {
		if err := restartService(); err != nil {
			jsonResponse(w, http.StatusInternalServerError, false, "Gagal merestart service", nil)
			return
		}
	}

	jsonResponse(w, http.StatusOK, true, "User berhasil diperpanjang", map[string]string{
		"password": req.Password,
		"expired":  newExpDate,
	})
}

func listUsers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonResponse(w, http.StatusMethodNotAllowed, false, "Method not allowed", nil)
		return
	}

	users, err := loadUsers()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, false, "Gagal membaca database user", nil)
		return
	}

	type UserInfo struct {
		Password     string `json:"password"`
		Expired      string `json:"expired"`
		Status       string `json:"status"`
		IpLimit      int    `json:"ip_limit"`
		ManualLocked bool   `json:"manual_locked"`
	}

	userList := []UserInfo{}
	today := time.Now().Format("2006-01-02")

	for _, u := range users {
		status := "Active"
		if u.ManualLocked || u.Status == "locked" {
			status = "Locked"
		} else if u.Expired < today {
			status = "Expired"
		}

		userList = append(userList, UserInfo{
			Password:     u.Password,
			Expired:      u.Expired,
			Status:       status,
			IpLimit:      defaultIpLimit(u.IpLimit),
			ManualLocked: u.ManualLocked,
		})
	}

	jsonResponse(w, http.StatusOK, true, "Daftar user", userList)
}

func lockUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonResponse(w, http.StatusMethodNotAllowed, false, "Method not allowed", nil)
		return
	}

	var req UserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, false, "Invalid request body", nil)
		return
	}

	username := resolveUsername(req)
	if username == "" {
		jsonResponse(w, http.StatusBadRequest, false, "Username harus diisi", nil)
		return
	}

	mutex.Lock()
	defer mutex.Unlock()

	users, err := loadUsers()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, false, "Gagal membaca database user", nil)
		return
	}

	updated := false
	for i := range users {
		if users[i].Password == username {
			users[i].ManualLocked = true
			users[i].Status = "locked"
			updated = true
			break
		}
	}

	if !updated {
		jsonResponse(w, http.StatusNotFound, false, "User tidak ditemukan", nil)
		return
	}

	if err := saveUsers(users); err != nil {
		jsonResponse(w, http.StatusInternalServerError, false, "Gagal menyimpan database user", nil)
		return
	}

	jsonResponse(w, http.StatusOK, true, "User berhasil di-lock", nil)
}

func unlockUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonResponse(w, http.StatusMethodNotAllowed, false, "Method not allowed", nil)
		return
	}

	var req UserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, false, "Invalid request body", nil)
		return
	}

	username := resolveUsername(req)
	if username == "" {
		jsonResponse(w, http.StatusBadRequest, false, "Username harus diisi", nil)
		return
	}

	mutex.Lock()
	defer mutex.Unlock()

	users, err := loadUsers()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, false, "Gagal membaca database user", nil)
		return
	}

	updated := false
	for i := range users {
		if users[i].Password == username {
			users[i].ManualLocked = false
			if users[i].Status == "locked" {
				users[i].Status = "active"
			}
			updated = true
			break
		}
	}

	if !updated {
		jsonResponse(w, http.StatusNotFound, false, "User tidak ditemukan", nil)
		return
	}

	if err := saveUsers(users); err != nil {
		jsonResponse(w, http.StatusInternalServerError, false, "Gagal menyimpan database user", nil)
		return
	}

	jsonResponse(w, http.StatusOK, true, "User berhasil di-unlock", nil)
}

func getSystemInfo(w http.ResponseWriter, r *http.Request) {
	cmd := exec.Command("curl", "-s", "ifconfig.me")
	ipPub, _ := cmd.Output()

	cmd = exec.Command("hostname", "-I")
	ipPriv, _ := cmd.Output()

	domain := "Tidak diatur"
	if domainBytes, err := ioutil.ReadFile(DomainFile); err == nil {
		domain = strings.TrimSpace(string(domainBytes))
	}

	info := map[string]string{
		"domain":     domain,
		"public_ip":  strings.TrimSpace(string(ipPub)),
		"private_ip": strings.Fields(string(ipPriv))[0],
		"port":       "5667",
		"service":    "zivpn",
	}

	jsonResponse(w, http.StatusOK, true, "System Info", info)
}

func authHysteria(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonResponse(w, http.StatusMethodNotAllowed, false, "Method not allowed", nil)
		return
	}

	body, _ := ioutil.ReadAll(r.Body)
	authReq := parseAuthRequest(body, r)
	if authReq.Credential == "" {
		jsonResponse(w, http.StatusForbidden, false, "Credential kosong", nil)
		return
	}

	clientIP := authReq.ClientIP
	if clientIP == "" {
		jsonResponse(w, http.StatusForbidden, false, "IP client tidak ditemukan", nil)
		return
	}

	users, err := loadUsers()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, false, "Gagal membaca database user", nil)
		return
	}

	user, found := findUser(users, authReq.Credential)
	if !found {
		jsonResponse(w, http.StatusForbidden, false, "User tidak ditemukan", nil)
		return
	}

	if isExpired(user.Expired) {
		jsonResponse(w, http.StatusForbidden, false, "User expired", nil)
		return
	}

	if user.ManualLocked {
		jsonResponse(w, http.StatusForbidden, false, "User di-lock", nil)
		return
	}

	ttlSeconds := getEnvInt("ZIVPN_IP_SESSION_TTL_SECONDS", 300)
	now := time.Now().Unix()

	sessionMutex.Lock()
	sessions, err := loadUserSessions()
	if err != nil {
		sessionMutex.Unlock()
		jsonResponse(w, http.StatusInternalServerError, false, "Gagal membaca sesi user", nil)
		return
	}

	userSessions := sessions[authReq.Credential]
	if userSessions == nil {
		userSessions = make(map[string]int64)
	}

	expiryCutoff := now - int64(ttlSeconds)
	for ip, lastSeen := range userSessions {
		if lastSeen < expiryCutoff {
			delete(userSessions, ip)
		}
	}

	userSessions[clientIP] = now
	sessions[authReq.Credential] = userSessions

	if err := saveUserSessions(sessions); err != nil {
		sessionMutex.Unlock()
		jsonResponse(w, http.StatusInternalServerError, false, "Gagal menyimpan sesi user", nil)
		return
	}
	sessionMutex.Unlock()

	activeCount := len(userSessions)
	if activeCount > defaultIpLimit(user.IpLimit) {
		if shouldNotifyMultiLogin(authReq.Credential, now) {
			if err := sendMultiLoginNotification(authReq.Credential, clientIP, defaultIpLimit(user.IpLimit), activeCount); err != nil {
				log.Printf("Multi login notify error: %v", err)
			}
		}
	}

	jsonResponse(w, http.StatusOK, true, "OK", map[string]bool{"ok": true})
}

func checkExpiration(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonResponse(w, http.StatusMethodNotAllowed, false, "Method not allowed", nil)
		return
	}

	users, err := loadUsers()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, false, "Gagal membaca database user", nil)
		return
	}

	today := time.Now().Format("2006-01-02")

	revokedCount := 0
	config, err := loadConfig()
	if err == nil && usesPasswordAuth(config) {
		activeUsers := make(map[string]bool)
		for _, p := range config.Auth.Config {
			activeUsers[p] = true
		}

		for _, u := range users {
			if u.Expired < today && activeUsers[u.Password] {
				log.Printf("User %s expired (Exp: %s). Revoking access.\n", u.Password, u.Expired)
				revokeAccess(u.Password)
				revokedCount++
			}
		}
	}

	jsonResponse(w, http.StatusOK, true, fmt.Sprintf("Expiration check complete. Revoked: %d", revokedCount), nil)
}

// cleanupExpired menghapus semua akun expired dari config.json DAN users.json
func cleanupExpired(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonResponse(w, http.StatusMethodNotAllowed, false, "Method not allowed", nil)
		return
	}

	mutex.Lock()
	defer mutex.Unlock()

	users, err := loadUsers()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, false, "Gagal membaca database user", nil)
		return
	}

	config, err := loadConfig()
	if err != nil && !os.IsNotExist(err) {
		jsonResponse(w, http.StatusInternalServerError, false, "Gagal membaca config", nil)
		return
	}

	today := time.Now().Format("2006-01-02")

	// Collect expired passwords
	expiredPasswords := make(map[string]bool)
	for _, u := range users {
		if u.Expired < today {
			expiredPasswords[u.Password] = true
		}
	}

	if len(expiredPasswords) == 0 {
		jsonResponse(w, http.StatusOK, true, "Tidak ada akun expired", nil)
		return
	}

	// Remove from users.json
	activeUsers := []UserStore{}
	for _, u := range users {
		if !expiredPasswords[u.Password] {
			activeUsers = append(activeUsers, u)
		}
	}

	// Remove from config.json when password auth
	if err == nil && usesPasswordAuth(config) {
		activeConfig := []string{}
		for _, p := range config.Auth.Config {
			if !expiredPasswords[p] {
				activeConfig = append(activeConfig, p)
			}
		}
		config.Auth.Config = activeConfig
	}

	// Save both
	if err := saveUsers(activeUsers); err != nil {
		jsonResponse(w, http.StatusInternalServerError, false, "Gagal menyimpan users.json", nil)
		return
	}

	if err == nil && usesPasswordAuth(config) {
		if err := saveConfig(config); err != nil {
			jsonResponse(w, http.StatusInternalServerError, false, "Gagal menyimpan config.json", nil)
			return
		}
	}

	// Restart service
	if err == nil && usesPasswordAuth(config) {
		restartService()
	}

	deletedCount := len(expiredPasswords)
	deletedList := []string{}
	for p := range expiredPasswords {
		deletedList = append(deletedList, p)
	}

	jsonResponse(w, http.StatusOK, true, fmt.Sprintf("Berhasil menghapus %d akun expired", deletedCount), map[string]interface{}{
		"deleted_count": deletedCount,
		"deleted_users": deletedList,
	})
}

func revokeAccess(password string) {
	mutex.Lock()
	defer mutex.Unlock()

	config, err := loadConfig()
	if err == nil && usesPasswordAuth(config) {
		newConfigAuth := []string{}
		changed := false
		for _, p := range config.Auth.Config {
			if p == password {
				changed = true
			} else {
				newConfigAuth = append(newConfigAuth, p)
			}
		}
		if changed {
			config.Auth.Config = newConfigAuth
			saveConfig(config)
			restartService()
		}
	}
}

func enableUser(password string) {
	mutex.Lock()
	defer mutex.Unlock()

	config, err := loadConfig()
	if err != nil || !usesPasswordAuth(config) {
		return
	}

	exists := false
	for _, p := range config.Auth.Config {
		if p == password {
			exists = true
			break
		}
	}

	if !exists {
		config.Auth.Config = append(config.Auth.Config, password)
		saveConfig(config)
		restartService()
	}
}

type authRequest struct {
	Credential string
	ClientIP   string
}

func parseAuthRequest(body []byte, r *http.Request) authRequest {
	var payload map[string]interface{}
	authReq := authRequest{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &payload); err == nil {
			authReq.Credential = firstNonEmpty(
				getString(payload, "auth"),
				getString(payload, "password"),
				getString(payload, "username"),
				getString(payload, "token"),
				getString(payload, "user"),
			)
			authReq.ClientIP = firstNonEmpty(
				stripPort(getString(payload, "addr")),
				stripPort(getString(payload, "address")),
				getString(payload, "client_ip"),
				getString(payload, "ip"),
				stripPort(getString(payload, "remote_addr")),
			)
		}
	}

	if authReq.Credential == "" {
		authReq.Credential = firstNonEmpty(
			r.URL.Query().Get("auth"),
			r.URL.Query().Get("password"),
			r.URL.Query().Get("username"),
			r.URL.Query().Get("token"),
		)
	}

	if authReq.ClientIP == "" {
		authReq.ClientIP = firstNonEmpty(
			stripPort(r.URL.Query().Get("addr")),
			stripPort(r.URL.Query().Get("address")),
			r.URL.Query().Get("client_ip"),
			r.URL.Query().Get("ip"),
		)
	}

	if authReq.ClientIP == "" {
		authReq.ClientIP = stripPort(r.RemoteAddr)
	}

	return authReq
}

func getString(payload map[string]interface{}, key string) string {
	if payload == nil {
		return ""
	}
	if val, ok := payload[key]; ok {
		if str, ok := val.(string); ok {
			return strings.TrimSpace(str)
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func stripPort(addr string) string {
	if addr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

func findUser(users []UserStore, credential string) (UserStore, bool) {
	for _, u := range users {
		if u.Password == credential {
			return u, true
		}
	}
	return UserStore{}, false
}

func isExpired(dateStr string) bool {
	if dateStr == "" {
		return false
	}
	expiry, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		return false
	}
	return expiry.Before(time.Now())
}

func getEnvInt(key string, defaultValue int) int {
	if val := strings.TrimSpace(os.Getenv(key)); val != "" {
		if parsed, err := strconv.Atoi(val); err == nil {
			return parsed
		}
	}
	return defaultValue
}

func defaultIpLimit(limit int) int {
	if limit <= 0 {
		return 1
	}
	return limit
}

func resolveUsername(req UserRequest) string {
	if req.Username != "" {
		return strings.TrimSpace(req.Username)
	}
	return strings.TrimSpace(req.Password)
}

func usesPasswordAuth(config Config) bool {
	mode := strings.ToLower(config.Auth.Mode)
	authType := strings.ToLower(config.Auth.Type)
	return mode == "passwords" || mode == "password" || authType == "passwords" || authType == "password"
}

type userSessions map[string]map[string]int64

func loadUserSessions() (userSessions, error) {
	sessions := make(userSessions)
	file, err := ioutil.ReadFile(UserIPSessionsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return sessions, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(file, &sessions); err != nil {
		return nil, err
	}
	return sessions, nil
}

func saveUserSessions(sessions userSessions) error {
	data, err := json.MarshalIndent(sessions, "", "  ")
	if err != nil {
		return err
	}
	return ioutil.WriteFile(UserIPSessionsFile, data, 0644)
}

func shouldNotifyMultiLogin(username string, now int64) bool {
	notifyMutex.Lock()
	defer notifyMutex.Unlock()

	cooldown := int64(getEnvInt("ZIVPN_MULTI_LOGIN_NOTIFY_COOLDOWN_SECONDS", 300))
	if cooldown <= 0 {
		return true
	}

	data := map[string]int64{}
	file, err := ioutil.ReadFile(MultiLoginNotifiedFile)
	if err == nil {
		_ = json.Unmarshal(file, &data)
	}

	lastNotified := data[username]
	if now-lastNotified < cooldown {
		return false
	}

	data[username] = now
	updated, err := json.MarshalIndent(data, "", "  ")
	if err == nil {
		_ = ioutil.WriteFile(MultiLoginNotifiedFile, updated, 0644)
	}

	return true
}

func sendMultiLoginNotification(username, clientIP string, ipLimit, activeCount int) error {
	config, err := loadBotConfig()
	if err != nil {
		return err
	}

	ispName := getIspName(clientIP)
	domain := getDomainName()

	message := fmt.Sprintf("┌───────────────────┐\n   NOTIF MULTI LOGIN\n└───────────────────┘\n Domain   : %s\n Username : %s\n Isp      : %s\n Limit IP : %d\n Login IP : %d\n└───────────────────┘",
		domain, username, ispName, ipLimit, activeCount,
	)

	payload := map[string]interface{}{
		"chat_id": config.AdminID,
		"text":    message,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", config.BotToken), strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("telegram returned %s", resp.Status)
	}
	return nil
}

type botConfig struct {
	BotToken string `json:"bot_token"`
	AdminID  int64  `json:"admin_id"`
}

func loadBotConfig() (botConfig, error) {
	var cfg botConfig
	file, err := ioutil.ReadFile(BotConfigFile)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(file, &cfg); err != nil {
		return cfg, err
	}
	if cfg.BotToken == "" || cfg.AdminID == 0 {
		return cfg, fmt.Errorf("bot config tidak lengkap")
	}
	return cfg, nil
}

func getDomainName() string {
	if envDomain := strings.TrimSpace(os.Getenv("ZIVPN_DOMAIN")); envDomain != "" {
		return envDomain
	}
	if domainBytes, err := ioutil.ReadFile(DomainFile); err == nil {
		return strings.TrimSpace(string(domainBytes))
	}
	return "Tidak diatur"
}

func getIspName(ip string) string {
	if ip == "" {
		return "Unknown"
	}
	ispCache.Lock()
	defer ispCache.Unlock()

	if cached, ok := cachedIsp[ip]; ok && time.Now().Before(cached.ExpiresAt) {
		return cached.Name
	}

	ispName := lookupIsp(ip)
	cachedIsp[ip] = cachedISP{
		Name:      ispName,
		ExpiresAt: time.Now().Add(1 * time.Hour),
	}
	return ispName
}

func lookupIsp(ip string) string {
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest("GET", fmt.Sprintf("https://ipapi.co/%s/json/", ip), nil)
	if err != nil {
		return "Unknown"
	}

	resp, err := client.Do(req)
	if err != nil {
		return "Unknown"
	}
	defer resp.Body.Close()

	var data struct {
		Org string `json:"org"`
		Asn string `json:"asn"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "Unknown"
	}

	if data.Org != "" {
		return data.Org
	}
	if data.Asn != "" {
		return data.Asn
	}
	return "Unknown"
}

func loadConfig() (Config, error) {
	var config Config
	file, err := ioutil.ReadFile(ConfigFile)
	if err != nil {
		return config, err
	}
	err = json.Unmarshal(file, &config)
	return config, err
}

func saveConfig(config Config) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return ioutil.WriteFile(ConfigFile, data, 0644)
}

func loadUsers() ([]UserStore, error) {
	var users []UserStore
	file, err := ioutil.ReadFile(UserDB)
	if err != nil {
		if os.IsNotExist(err) {
			return users, nil
		}
		return nil, err
	}
	err = json.Unmarshal(file, &users)
	if err != nil {
		return nil, err
	}
	for i := range users {
		users[i].IpLimit = defaultIpLimit(users[i].IpLimit)
	}
	return users, err
}

func saveUsers(users []UserStore) error {
	data, err := json.MarshalIndent(users, "", "  ")
	if err != nil {
		return err
	}
	return ioutil.WriteFile(UserDB, data, 0644)
}

func restartService() error {
	cmd := exec.Command("systemctl", "restart", "zivpn.service")
	return cmd.Run()
}
