package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"zivpn/internal/botshared"
)

// ==========================================
// Constants & Configuration
// ==========================================

type BotConfig struct {
	BotToken string `json:"bot_token"`
	AdminID  int64  `json:"admin_id"`
	Mode     string `json:"mode"`   // "public" or "private"
	Domain   string `json:"domain"` // Domain from setup
}

// ==========================================
// Global State
// ==========================================

var userStates = make(map[int64]string)
var tempUserData = make(map[int64]map[string]string)
var lastMessageIDs = make(map[int64]int)

// ==========================================
// Main Entry Point
// ==========================================

func main() {
	// Load API Key
	if keyBytes, err := ioutil.ReadFile(botshared.ApiKeyFile); err == nil {
		botshared.ApiKey = strings.TrimSpace(string(keyBytes))
	}

	// Load API Port
	if portBytes, err := ioutil.ReadFile(botshared.ApiPortFile); err == nil {
		port := strings.TrimSpace(string(portBytes))
		botshared.ApiUrl = fmt.Sprintf("http://127.0.0.1:%s/api", port)
	}

	// Load Config
	config, err := loadConfig()
	if err != nil {
		log.Fatal("Gagal memuat konfigurasi bot:", err)
	}

	// Initialize Bot
	bot, err := tgbotapi.NewBotAPI(config.BotToken)
	if err != nil {
		log.Panic(err)
	}

	bot.Debug = false
	log.Printf("Authorized on account %s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	// Main Loop
	for update := range updates {
		if update.Message != nil {
			handleMessage(bot, update.Message, &config)
		} else if update.CallbackQuery != nil {
			handleCallback(bot, update.CallbackQuery, &config)
		}
	}
}

// ==========================================
// Telegram Event Handlers
// ==========================================

func handleMessage(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, config *BotConfig) {
	// Access Control
	if !isAllowed(config, msg.From.ID) {
		replyError(bot, msg.Chat.ID, "⛔ Akses Ditolak. Bot ini Private.")
		return
	}

	// Handle Document Upload (Restore)
	if msg.Document != nil && msg.From.ID == config.AdminID {
		if state, exists := userStates[msg.From.ID]; exists && state == "waiting_restore_file" {
			processRestoreFile(bot, msg, config)
			return
		}
	}

	// Handle State (User Input)
	if state, exists := userStates[msg.From.ID]; exists {
		handleState(bot, msg, state, config)
		return
	}

	// Handle Commands
	if msg.IsCommand() {
		switch msg.Command() {
		case "start":
			showMainMenu(bot, msg.Chat.ID, config)
		case "lock":
			handleLockCommand(bot, msg, config, true)
		case "unlock":
			handleLockCommand(bot, msg, config, false)
		default:
			replyError(bot, msg.Chat.ID, "Perintah tidak dikenal.")
		}
	}
}

func handleCallback(bot *tgbotapi.BotAPI, query *tgbotapi.CallbackQuery, config *BotConfig) {
	// Access Control (Special case for toggle_mode)
	if !isAllowed(config, query.From.ID) {
		if query.Data != "toggle_mode" || query.From.ID != config.AdminID {
			bot.Request(tgbotapi.NewCallback(query.ID, "Akses Ditolak"))
			return
		}
	}

	chatID := query.Message.Chat.ID
	userID := query.From.ID

	switch {
	// --- Menu Navigation ---
	case query.Data == "menu_create":
		startCreateUser(bot, chatID, userID)
	case query.Data == "menu_delete":
		showUserSelection(bot, chatID, 1, "delete")
	case query.Data == "menu_renew":
		showUserSelection(bot, chatID, 1, "renew")
	case query.Data == "menu_list":
		if userID == config.AdminID {
			listUsers(bot, chatID)
		}
	case query.Data == "menu_cleanup":
		if userID == config.AdminID {
			cleanupExpiredUsers(bot, chatID, config)
		}
	case query.Data == "menu_info":
		if userID == config.AdminID {
			systemInfo(bot, chatID, config)
		}
	case query.Data == "menu_backup_restore":
		if userID == config.AdminID {
			showBackupRestoreMenu(bot, chatID)
		}
	case query.Data == "menu_backup_action":
		if userID == config.AdminID {
			performBackup(bot, chatID)
		}
	case query.Data == "menu_restore_action":
		if userID == config.AdminID {
			startRestore(bot, chatID, userID)
		}
	case query.Data == "cancel":
		cancelOperation(bot, chatID, userID, config)

	// --- Pagination ---
	case strings.HasPrefix(query.Data, "page_"):
		handlePagination(bot, chatID, query.Data)

	// --- Action Selection ---
	case strings.HasPrefix(query.Data, "select_renew:"):
		startRenewUser(bot, chatID, userID, query.Data)
	case strings.HasPrefix(query.Data, "select_delete:"):
		confirmDeleteUser(bot, chatID, query.Data)

	// --- Action Confirmation ---
	case strings.HasPrefix(query.Data, "confirm_delete:"):
		username := strings.TrimPrefix(query.Data, "confirm_delete:")
		deleteUser(bot, chatID, username, config)

	// --- Admin Actions ---
	case query.Data == "toggle_mode":
		toggleMode(bot, chatID, userID, config)
	}

	bot.Request(tgbotapi.NewCallback(query.ID, ""))
}

func handleState(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, state string, config *BotConfig) {
	userID := msg.From.ID
	text := strings.TrimSpace(msg.Text)
	chatID := msg.Chat.ID

	switch state {
	case "create_username":
		if !validateUsername(bot, chatID, text) {
			return
		}
		tempUserData[userID]["username"] = text
		userStates[userID] = "create_days"
		sendMessage(bot, chatID, "⏳ Masukkan Durasi (hari):")

	case "create_days":
		_, ok := validateNumber(bot, chatID, text, 1, 9999, "Durasi")
		if !ok {
			return
		}
		tempUserData[userID]["days"] = text
		userStates[userID] = "create_ip_limit"
		sendMessage(bot, chatID, "🌐 Masukkan Limit IP (angka, default 1):")

	case "create_ip_limit":
		ipLimit, ok := validateNumber(bot, chatID, text, 1, 9999, "Limit IP")
		if !ok {
			return
		}
		tempUserData[userID]["ip_limit"] = strconv.Itoa(ipLimit)
		days, _ := strconv.Atoi(tempUserData[userID]["days"])
		createUser(bot, chatID, tempUserData[userID]["username"], days, ipLimit, config)
		resetState(userID)

	case "renew_days":
		days, ok := validateNumber(bot, chatID, text, 1, 9999, "Durasi")
		if !ok {
			return
		}
		renewUser(bot, chatID, tempUserData[userID]["username"], days, config)
		resetState(userID)
	}
}

// ==========================================
// Feature Implementation
// ==========================================

func startCreateUser(bot *tgbotapi.BotAPI, chatID int64, userID int64) {
	userStates[userID] = "create_username"
	tempUserData[userID] = make(map[string]string)
	sendMessage(bot, chatID, "👤 Masukkan Password:")
}

func startRenewUser(bot *tgbotapi.BotAPI, chatID int64, userID int64, data string) {
	username := strings.TrimPrefix(data, "select_renew:")
	tempUserData[userID] = map[string]string{"username": username}
	userStates[userID] = "renew_days"
	sendMessage(bot, chatID, fmt.Sprintf("🔄 Renewing %s\n⏳ Masukkan Tambahan Durasi (hari):", username))
}

func confirmDeleteUser(bot *tgbotapi.BotAPI, chatID int64, data string) {
	username := strings.TrimPrefix(data, "select_delete:")
	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("❓ Yakin ingin menghapus user `%s`?", username))
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Ya, Hapus", "confirm_delete:"+username),
			tgbotapi.NewInlineKeyboardButtonData("❌ Batal", "cancel"),
		),
	)
	sendAndTrack(bot, msg)
}

func cancelOperation(bot *tgbotapi.BotAPI, chatID int64, userID int64, config *BotConfig) {
	resetState(userID)
	showMainMenu(bot, chatID, config)
}

func handlePagination(bot *tgbotapi.BotAPI, chatID int64, data string) {
	parts := strings.Split(data, ":")
	action := parts[0][5:] // remove "page_"
	page, _ := strconv.Atoi(parts[1])
	showUserSelection(bot, chatID, page, action)
}

func toggleMode(bot *tgbotapi.BotAPI, chatID int64, userID int64, config *BotConfig) {
	if userID != config.AdminID {
		return
	}
	if config.Mode == "public" {
		config.Mode = "private"
	} else {
		config.Mode = "public"
	}
	saveConfig(config)
	showMainMenu(bot, chatID, config)
}

func createUser(bot *tgbotapi.BotAPI, chatID int64, username string, days int, ipLimit int, config *BotConfig) {
	res, err := botshared.ApiCall("POST", "/user/create", map[string]interface{}{
		"password": username,
		"days":     days,
		"ip_limit": ipLimit,
	})

	if err != nil {
		replyError(bot, chatID, "Error API: "+err.Error())
		return
	}

	if res["success"] == true {
		data := res["data"].(map[string]interface{})
		sendAccountInfo(bot, chatID, data, config)
	} else {
		replyError(bot, chatID, fmt.Sprintf("Gagal: %s", res["message"]))
		showMainMenu(bot, chatID, config)
	}
}

func renewUser(bot *tgbotapi.BotAPI, chatID int64, username string, days int, config *BotConfig) {
	res, err := botshared.ApiCall("POST", "/user/renew", map[string]interface{}{
		"password": username,
		"days":     days,
	})

	if err != nil {
		replyError(bot, chatID, "Error API: "+err.Error())
		return
	}

	if res["success"] == true {
		data := res["data"].(map[string]interface{})
		// For renew, we might not have the limit handy, so passing 0 or fetching it would be ideal.
		// But for now, let's just display what we have.
		sendAccountInfo(bot, chatID, data, config)
	} else {
		replyError(bot, chatID, fmt.Sprintf("Gagal: %s", res["message"]))
		showMainMenu(bot, chatID, config)
	}
}

func deleteUser(bot *tgbotapi.BotAPI, chatID int64, username string, config *BotConfig) {
	res, err := botshared.ApiCall("POST", "/user/delete", map[string]interface{}{
		"password": username,
	})

	if err != nil {
		replyError(bot, chatID, "Error API: "+err.Error())
		return
	}

	if res["success"] == true {
		msg := tgbotapi.NewMessage(chatID, "✅ Password berhasil dihapus.")
		deleteLastMessage(bot, chatID)
		bot.Send(msg)
		showMainMenu(bot, chatID, config)
	} else {
		replyError(bot, chatID, fmt.Sprintf("Gagal: %s", res["message"]))
		showMainMenu(bot, chatID, config)
	}
}

func handleLockCommand(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, config *BotConfig, lock bool) {
	if msg.From.ID != config.AdminID {
		replyError(bot, msg.Chat.ID, "⛔ Akses Ditolak. Hanya admin.")
		return
	}

	username := strings.TrimSpace(msg.CommandArguments())
	if username == "" {
		action := "lock"
		if !lock {
			action = "unlock"
		}
		sendMessage(bot, msg.Chat.ID, fmt.Sprintf("Format: /%s <username>", action))
		return
	}

	endpoint := "/user/lock"
	successMessage := "✅ User berhasil di-lock."
	if !lock {
		endpoint = "/user/unlock"
		successMessage = "✅ User berhasil di-unlock."
	}

	res, err := botshared.ApiCall("POST", endpoint, map[string]interface{}{
		"username": username,
	})
	if err != nil {
		replyError(bot, msg.Chat.ID, "Error API: "+err.Error())
		return
	}

	if res["success"] == true {
		sendMessage(bot, msg.Chat.ID, successMessage)
	} else {
		replyError(bot, msg.Chat.ID, fmt.Sprintf("Gagal: %s", res["message"]))
	}
}

func listUsers(bot *tgbotapi.BotAPI, chatID int64) {
	res, err := botshared.ApiCall("GET", "/users", nil)
	if err != nil {
		replyError(bot, chatID, "Error API: "+err.Error())
		return
	}

	if res["success"] == true {
		users := res["data"].([]interface{})
		if len(users) == 0 {
			sendMessage(bot, chatID, "📂 Tidak ada user.")
			return
		}

		msg := "📋 *List Passwords*\n"
		for _, u := range users {
			user := u.(map[string]interface{})
			status := "🟢"
			if user["status"] == "Expired" {
				status = "🔴"
			} else if user["status"] == "Locked" {
				status = "🔒"
			}
			msg += fmt.Sprintf("\n%s `%s` (%s)", status, user["password"], user["expired"])
		}

		reply := tgbotapi.NewMessage(chatID, msg)
		reply.ParseMode = "Markdown"
		sendAndTrack(bot, reply)
	} else {
		replyError(bot, chatID, "Gagal mengambil data.")
	}
}

func systemInfo(bot *tgbotapi.BotAPI, chatID int64, config *BotConfig) {
	res, err := botshared.ApiCall("GET", "/info", nil)
	if err != nil {
		replyError(bot, chatID, "Error API: "+err.Error())
		return
	}

	if res["success"] == true {
		data := res["data"].(map[string]interface{})
		ipInfo, _ := botshared.GetIpInfo()

		msg := fmt.Sprintf("```\n━━━━━━━━━━━━━━━━━━━━━\n    INFO ZIVPN UDP\n━━━━━━━━━━━━━━━━━━━━━\nDomain         : %s\nIP Public      : %s\nPort           : %s\nService        : %s\nCITY           : %s\nISP            : %s\n━━━━━━━━━━━━━━━━━━━━━\n```",
			config.Domain, data["public_ip"], data["port"], data["service"], ipInfo.City, ipInfo.Isp)

		reply := tgbotapi.NewMessage(chatID, msg)
		reply.ParseMode = "Markdown"
		deleteLastMessage(bot, chatID)
		bot.Send(reply)
		showMainMenu(bot, chatID, config)
	} else {
		replyError(bot, chatID, "Gagal mengambil info.")
	}
}

func cleanupExpiredUsers(bot *tgbotapi.BotAPI, chatID int64, config *BotConfig) {
	sendMessage(bot, chatID, "⏳ Sedang membersihkan akun expired...")

	res, err := botshared.ApiCall("POST", "/cron/cleanup", nil)
	if err != nil {
		replyError(bot, chatID, "Error API: "+err.Error())
		showMainMenu(bot, chatID, config)
		return
	}

	if res["success"] == true {
		data := res["data"]
		if data != nil {
			dataMap := data.(map[string]interface{})
			count := int(dataMap["deleted_count"].(float64))
			if count > 0 {
				deletedUsers := dataMap["deleted_users"].([]interface{})
				userList := ""
				for _, u := range deletedUsers {
					userList += fmt.Sprintf("\n• `%s`", u.(string))
				}
				msg := fmt.Sprintf("✅ Berhasil menghapus %d akun expired:%s", count, userList)
				reply := tgbotapi.NewMessage(chatID, msg)
				reply.ParseMode = "Markdown"
				deleteLastMessage(bot, chatID)
				bot.Send(reply)
			} else {
				sendMessage(bot, chatID, "✅ Tidak ada akun expired yang perlu dihapus.")
			}
		} else {
			sendMessage(bot, chatID, "✅ "+res["message"].(string))
		}
	} else {
		replyError(bot, chatID, fmt.Sprintf("Gagal: %s", res["message"]))
	}
	showMainMenu(bot, chatID, config)
}

func showBackupRestoreMenu(bot *tgbotapi.BotAPI, chatID int64) {
	msg := tgbotapi.NewMessage(chatID, "💾 *Backup & Restore*\nSilakan pilih menu:")
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⬇️ Backup Data", "menu_backup_action"),
			tgbotapi.NewInlineKeyboardButtonData("⬆️ Restore Data", "menu_restore_action"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("❌ Kembali", "cancel"),
		),
	)
	sendAndTrack(bot, msg)
}

func performBackup(bot *tgbotapi.BotAPI, chatID int64) {
	sendMessage(bot, chatID, "⏳ Sedang membuat backup...")

	// Files to backup
	files := []string{
		"/etc/zivpn/config.json",
		"/etc/zivpn/users.json",
		"/etc/zivpn/domain",
	}

	buf := new(bytes.Buffer)
	zipWriter := zip.NewWriter(buf)

	for _, file := range files {
		if _, err := os.Stat(file); os.IsNotExist(err) {
			continue
		}

		f, err := os.Open(file)
		if err != nil {
			continue
		}
		defer f.Close()

		w, err := zipWriter.Create(filepath.Base(file))
		if err != nil {
			continue
		}

		if _, err := io.Copy(w, f); err != nil {
			continue
		}
	}

	zipWriter.Close()

	fileName := fmt.Sprintf("zivpn-backup-%s.zip", time.Now().Format("20060102-150405"))

	// Create a temporary file for the upload
	tmpFile := "/tmp/" + fileName
	if err := ioutil.WriteFile(tmpFile, buf.Bytes(), 0644); err != nil {
		replyError(bot, chatID, "Gagal membuat file backup.")
		return
	}
	defer os.Remove(tmpFile)

	doc := tgbotapi.NewDocument(chatID, tgbotapi.FilePath(tmpFile))
	doc.Caption = "✅ Backup Data ZiVPN"

	deleteLastMessage(bot, chatID)
	bot.Send(doc)
}

func startRestore(bot *tgbotapi.BotAPI, chatID int64, userID int64) {
	userStates[userID] = "waiting_restore_file"
	sendMessage(bot, chatID, "⬆️ *Restore Data*\n\nSilakan kirim file ZIP backup Anda sekarang.\n\n⚠️ PERINGATAN: Data saat ini akan ditimpa!")
}

func processRestoreFile(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, config *BotConfig) {
	chatID := msg.Chat.ID
	userID := msg.From.ID

	resetState(userID)
	sendMessage(bot, chatID, "⏳ Sedang memproses file...")

	// Download file
	fileID := msg.Document.FileID
	file, err := bot.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		replyError(bot, chatID, "Gagal mengunduh file.")
		return
	}

	fileUrl := file.Link(config.BotToken)
	resp, err := http.Get(fileUrl)
	if err != nil {
		replyError(bot, chatID, "Gagal mengunduh file content.")
		return
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		replyError(bot, chatID, "Gagal membaca file.")
		return
	}

	// Unzip
	zipReader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		replyError(bot, chatID, "File bukan format ZIP yang valid.")
		return
	}

	for _, f := range zipReader.File {
		// Security check: only allow specific files
		validFiles := map[string]bool{
			"config.json":     true,
			"users.json":      true,
			"bot-config.json": true,
			"domain":          true,
			"apikey":          true,
		}

		if !validFiles[f.Name] {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			continue
		}
		defer rc.Close()

		dstPath := filepath.Join("/etc/zivpn", f.Name)
		dst, err := os.Create(dstPath)
		if err != nil {
			continue
		}
		defer dst.Close()

		io.Copy(dst, rc)
	}

	// Restart Services
	exec.Command("systemctl", "restart", "zivpn").Run()
	exec.Command("systemctl", "restart", "zivpn-api").Run()

	msgSuccess := tgbotapi.NewMessage(chatID, "✅ Restore Berhasil!\nService ZiVPN, API, dan Bot telah direstart.")
	bot.Send(msgSuccess)

	// Restart Bot with delay to allow message sending
	go func() {
		time.Sleep(2 * time.Second)
		exec.Command("systemctl", "restart", "zivpn-bot").Run()
	}()

	showMainMenu(bot, chatID, config)
}

// ==========================================
// UI & Helpers
// ==========================================

func showMainMenu(bot *tgbotapi.BotAPI, chatID int64, config *BotConfig) {
	ipInfo, _ := botshared.GetIpInfo()
	domain := config.Domain
	if domain == "" {
		domain = "(Not Configured)"
	}

	msgText := fmt.Sprintf("```\n━━━━━━━━━━━━━━━━━━━━━\n    MENU ZIVPN UDP\n━━━━━━━━━━━━━━━━━━━━━\n • Domain   : %s\n • City     : %s\n • ISP      : %s\n━━━━━━━━━━━━━━━━━━━━━\n```\n👇 Silakan pilih menu dibawah ini:", domain, ipInfo.City, ipInfo.Isp)

	msg := tgbotapi.NewMessage(chatID, msgText)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = getMainMenuKeyboard(config, chatID)
	sendAndTrack(bot, msg)
}

func getMainMenuKeyboard(config *BotConfig, userID int64) tgbotapi.InlineKeyboardMarkup {
	// Public Menu (Everyone)
	rows := [][]tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("👤 Create Password", "menu_create"),
			tgbotapi.NewInlineKeyboardButtonData("🗑️ Delete Password", "menu_delete"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Renew Password", "menu_renew"),
		),
	}

	// Admin Menu (Admin Only)
	if userID == config.AdminID {
		modeLabel := "🔐 Mode: Private"
		if config.Mode == "public" {
			modeLabel = "🌍 Mode: Public"
		}

		rows[1] = append(rows[1], tgbotapi.NewInlineKeyboardButtonData("📋 List Passwords", "menu_list"))

		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📊 System Info", "menu_info"),
			tgbotapi.NewInlineKeyboardButtonData("💾 Backup & Restore", "menu_backup_restore"),
		))
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🧹 Cleanup Expired", "menu_cleanup"),
			tgbotapi.NewInlineKeyboardButtonData(modeLabel, "toggle_mode"),
		))
	}

	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func sendAccountInfo(bot *tgbotapi.BotAPI, chatID int64, data map[string]interface{}, config *BotConfig) {
	ipInfo, _ := botshared.GetIpInfo()
	domain := config.Domain
	if domain == "" {
		domain = "(Not Configured)"
	}

	ipLimit := "1"
	if val, ok := data["ip_limit"]; ok {
		switch v := val.(type) {
		case float64:
			ipLimit = strconv.Itoa(int(v))
		case int:
			ipLimit = strconv.Itoa(v)
		case string:
			ipLimit = v
		}
	}

	msg := fmt.Sprintf("```\n━━━━━━━━━━━━━━━━━━━━━\n  ACCOUNT ZIVPN UDP\n━━━━━━━━━━━━━━━━━━━━━\nPassword   : %s\nLimit IP   : %s\nCITY       : %s\nISP        : %s\nIP ISP     : %s\nDomain     : %s\nExpired On : %s\n━━━━━━━━━━━━━━━━━━━━━\n```",
		data["password"],
		ipLimit,
		ipInfo.City,
		ipInfo.Isp,
		ipInfo.Query,
		domain,
		data["expired"],
	)

	reply := tgbotapi.NewMessage(chatID, msg)
	reply.ParseMode = "Markdown"
	deleteLastMessage(bot, chatID)
	bot.Send(reply)
	showMainMenu(bot, chatID, config)
}

func showUserSelection(bot *tgbotapi.BotAPI, chatID int64, page int, action string) {
	users, err := botshared.GetUsers()
	if err != nil {
		replyError(bot, chatID, "Gagal mengambil data user.")
		return
	}

	if len(users) == 0 {
		sendMessage(bot, chatID, "📂 Tidak ada user.")
		return
	}

	perPage := 10
	totalPages := (len(users) + perPage - 1) / perPage

	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}

	start := (page - 1) * perPage
	end := start + perPage
	if end > len(users) {
		end = len(users)
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	for _, u := range users[start:end] {
		label := fmt.Sprintf("%s (%s)", u.Password, u.Status)
		if u.Status == "Expired" {
			label = fmt.Sprintf("🔴 %s", label)
		} else {
			label = fmt.Sprintf("🟢 %s", label)
		}
		data := fmt.Sprintf("select_%s:%s", action, u.Password)
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(label, data),
		))
	}

	var navRow []tgbotapi.InlineKeyboardButton
	if page > 1 {
		navRow = append(navRow, tgbotapi.NewInlineKeyboardButtonData("⬅️ Prev", fmt.Sprintf("page_%s:%d", action, page-1)))
	}
	if page < totalPages {
		navRow = append(navRow, tgbotapi.NewInlineKeyboardButtonData("Next ➡️", fmt.Sprintf("page_%s:%d", action, page+1)))
	}
	if len(navRow) > 0 {
		rows = append(rows, navRow)
	}

	rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("❌ Batal", "cancel")))

	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("📋 Pilih User untuk %s (Halaman %d/%d):", strings.Title(action), page, totalPages))
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
	sendAndTrack(bot, msg)
}

func validateUsername(bot *tgbotapi.BotAPI, chatID int64, text string) bool {
	if len(text) < 3 || len(text) > 20 {
		sendMessage(bot, chatID, "❌ Password harus 3-20 karakter. Coba lagi:")
		return false
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString(text) {
		sendMessage(bot, chatID, "❌ Password hanya boleh huruf, angka, - dan _. Coba lagi:")
		return false
	}
	return true
}

func validateNumber(bot *tgbotapi.BotAPI, chatID int64, text string, min, max int, fieldName string) (int, bool) {
	val, err := strconv.Atoi(text)
	if err != nil || val < min || val > max {
		sendMessage(bot, chatID, fmt.Sprintf("❌ %s harus angka positif (%d-%d). Coba lagi:", fieldName, min, max))
		return 0, false
	}
	return val, true
}

func sendMessage(bot *tgbotapi.BotAPI, chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	if _, inState := userStates[chatID]; inState {
		cancelKb := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("❌ Batal", "cancel")),
		)
		msg.ReplyMarkup = cancelKb
	}
	sendAndTrack(bot, msg)
}

func replyError(bot *tgbotapi.BotAPI, chatID int64, text string) {
	sendMessage(bot, chatID, "❌ "+text)
}

func sendAndTrack(bot *tgbotapi.BotAPI, msg tgbotapi.MessageConfig) {
	deleteLastMessage(bot, msg.ChatID)
	sentMsg, err := bot.Send(msg)
	if err == nil {
		lastMessageIDs[msg.ChatID] = sentMsg.MessageID
	}
}

func deleteLastMessage(bot *tgbotapi.BotAPI, chatID int64) {
	if msgID, ok := lastMessageIDs[chatID]; ok {
		deleteMsg := tgbotapi.NewDeleteMessage(chatID, msgID)
		bot.Request(deleteMsg)
		delete(lastMessageIDs, chatID)
	}
}

func resetState(userID int64) {
	delete(userStates, userID)
	delete(tempUserData, userID)
}

// ==========================================
// Configuration & Utils
// ==========================================

func isAllowed(config *BotConfig, userID int64) bool {
	return config.Mode == "public" || userID == config.AdminID
}

func saveConfig(config *BotConfig) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return ioutil.WriteFile(botshared.BotConfigFile, data, 0644)
}

func loadConfig() (BotConfig, error) {
	var config BotConfig
	file, err := ioutil.ReadFile(botshared.BotConfigFile)
	if err != nil {
		return config, err
	}
	err = json.Unmarshal(file, &config)

	// Jika domain kosong di config, coba baca dari file domain
	if config.Domain == "" {
		if domainBytes, err := ioutil.ReadFile(botshared.DomainFile); err == nil {
			config.Domain = strings.TrimSpace(string(domainBytes))
		}
	}

	return config, err
}
