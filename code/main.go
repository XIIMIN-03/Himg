package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	"io"
	"log"
	"math"
	"mime"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chai2010/webp"
	_ "github.com/mattn/go-sqlite3"
	"github.com/minio/minio-go/v7"
)

const (
	addr       = ":8080"
	cookieName = "himg_token"
	loginFailureBlockWindow = 10 * time.Minute
	loginFailureBlockLimit  = 10
)

type app struct {
	db         *sql.DB
	password   string
	passwordMu sync.RWMutex
	token      string
	storage    storageBackend
	storageMu  sync.RWMutex
	theme      string
	themeMu    sync.RWMutex
	security   securityConfig
	securityMu sync.RWMutex
	rateMu     sync.Mutex
	rateHits   map[string][]time.Time
}

type imageItem struct {
	ID           int64    `json:"id"`
	Name         string   `json:"name"`
	URL          string   `json:"url"`
	Size         int64    `json:"size"`
	Width        int      `json:"width"`
	Height       int      `json:"height"`
	UploadIP     string   `json:"upload_ip"`
	StorageType  string   `json:"storage_type"`
	StorageLabel string   `json:"storage_label"`
	ViewCount    int64    `json:"view_count"`
	Hidden       bool     `json:"hidden"`
	Tags         []string `json:"tags"`
	Score        float64  `json:"score,omitempty"`
	CreatedAt    string   `json:"created_at"`
}

type noticeItem struct {
	Content   string `json:"content"`
	Enabled   bool   `json:"enabled"`
	UpdatedAt string `json:"updated_at"`
}

type overviewImageItem struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Size      int64  `json:"size"`
	Hidden    bool   `json:"hidden"`
	CreatedAt string `json:"created_at"`
}

type overviewImageStats struct {
	Total               int64                 `json:"total"`
	TotalSize           int64                 `json:"total_size"`
	TotalViews          int64                 `json:"total_views"`
	HomepageTotalViews  int64                 `json:"homepage_total_views"`
	TodayTotal          int64                 `json:"today_total"`
	TodayViews          int64                 `json:"today_views"`
	Last7dTotal         int64                 `json:"last_7d_total"`
	Last7dViews         int64                 `json:"last_7d_views"`
	HomepageTodayViews  int64                 `json:"homepage_today_views"`
	HomepageLast7dViews int64                 `json:"homepage_last_7d_views"`
	DailyBuckets        []overviewDailyBucket `json:"daily_buckets"`
	Latest              *overviewImageItem    `json:"latest"`
}

type overviewDailyBucket struct {
	Date  string `json:"date"`
	Count int64  `json:"count"`
}

type overviewNoticeStats struct {
	Content    string `json:"content"`
	Enabled    bool   `json:"enabled"`
	UpdatedAt  string `json:"updated_at"`
	HasContent bool   `json:"has_content"`
}

type overviewThemeStats struct {
	Active string   `json:"active"`
	Items  []string `json:"items"`
	Count  int      `json:"count"`
}

type aiConfig struct {
	BaseURL      string `json:"base_url"`
	WireAPI      string `json:"wire_api"`
	Model        string `json:"model"`
	Key          string `json:"key"`
	TagPrompt    string `json:"tag_prompt"`
	ReviewPrompt string `json:"review_prompt"`
}

type siteConfig struct {
	HomeTagline  string `json:"home_tagline"`
	BrowserTitle string `json:"browser_title"`
}

type notifyServerChanConfig struct {
	Enabled bool   `json:"enabled"`
	SendKey string `json:"send_key"`
}

type notifyEmailConfig struct {
	Enabled  bool   `json:"enabled"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	From     string `json:"from"`
	To       string `json:"to"`
	UseSSL   bool   `json:"use_ssl"`
}

type notifyTelegramConfig struct {
	Enabled  bool   `json:"enabled"`
	BotToken string `json:"bot_token"`
	ChatID   string `json:"chat_id"`
}

type notifyEventsConfig struct {
	NewUpload       bool `json:"new_upload"`
	SensitiveReview bool `json:"sensitive_review"`
}

type notifyConfig struct {
	Enabled  bool                   `json:"enabled"`
	Server   notifyServerChanConfig `json:"server_chan"`
	Email    notifyEmailConfig      `json:"email"`
	Telegram notifyTelegramConfig   `json:"telegram"`
	Events   notifyEventsConfig     `json:"events"`
}

type guestUploadConfig struct {
	Enabled                bool     `json:"enabled"`
	MaxUploadsPerMinute    int      `json:"max_uploads_per_minute"`
	UploadSpeedKBPerSecond int      `json:"upload_speed_kb_per_second"`
	BlockedIPs             []string `json:"blocked_ips"`
}

type securityConfig struct {
	Enabled                bool     `json:"enabled"`
	SecurityHeadersEnabled bool     `json:"security_headers_enabled"`
	RateLimitEnabled       bool     `json:"rate_limit_enabled"`
	RequestsPerMinute      int      `json:"requests_per_minute"`
	MaxBodyMB              int      `json:"max_body_mb"`
	InjectionFilterEnabled bool     `json:"injection_filter_enabled"`
	BlockedIPs             []string `json:"blocked_ips"`
}

type overviewItem struct {
	GeneratedAt string              `json:"generated_at"`
	Images      overviewImageStats  `json:"images"`
	Notice      overviewNoticeStats `json:"notice"`
	Storage     storageConfig       `json:"storage"`
	Themes      overviewThemeStats  `json:"themes"`
	AI          aiConfig            `json:"ai"`
}

type loginLogItem struct {
	ID        int64  `json:"id"`
	IP        string `json:"ip"`
	Status    string `json:"status"`
	Message   string `json:"message"`
	CreatedAt string `json:"created_at"`
}

type operationLogItem struct {
	ID        int64  `json:"id"`
	IP        string `json:"ip"`
	Action    string `json:"action"`
	Target    string `json:"target"`
	Detail    string `json:"detail"`
	CreatedAt string `json:"created_at"`
}

func main() {
	loadRootEnvFile()

	dbPath := databaseFile()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		log.Fatal(err)
	}
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err = initDB(db); err != nil {
		log.Fatal(err)
	}

	a := &app{
		db:       db,
		password: env("HIMG_PASSWORD", "admin123"),
		token:    env("HIMG_TOKEN", mustToken()),
		rateHits: make(map[string][]time.Time),
	}
	if storedPassword, loadErr := a.loadAdminPassword(); loadErr == nil && strings.TrimSpace(storedPassword) != "" {
		a.setPassword(storedPassword)
	}
	cfg, err := a.loadStorageConfig()
	if err != nil {
		log.Fatal(err)
	}
	if a.storage, err = newStorageFromConfig(cfg); err != nil {
		log.Fatal(err)
	}
	if err = ensureThemesDir(); err != nil {
		log.Fatal(err)
	}
	if a.theme, err = a.loadThemeName(); err != nil {
		log.Fatal(err)
	}
	securityCfg, err := a.loadSecurityConfig()
	if err != nil {
		log.Fatal(err)
	}
	a.setSecurityConfig(securityCfg)
	mux := http.NewServeMux()
	mux.HandleFunc("/theme/", a.serveTheme)
	mux.HandleFunc("/uploads/", a.serveUpload)
	mux.HandleFunc("/", a.indexPage)
	mux.HandleFunc("/admin", a.adminPage)
	mux.HandleFunc("/api/about", a.about)
	mux.HandleFunc("/api/upload-policy", a.uploadPolicy)
	mux.HandleFunc("/api/session", a.session)
	mux.HandleFunc("/api/site-config", a.siteConfigAPI)
	mux.HandleFunc("/api/login", a.login)
	mux.HandleFunc("/api/logout", a.auth(a.logout))
	mux.HandleFunc("/api/password", a.auth(a.changePassword))
	mux.HandleFunc("/api/upload", a.upload)
	mux.HandleFunc("/api/images", a.auth(a.images))
	mux.HandleFunc("/api/images/search", a.auth(a.searchImages))
	mux.HandleFunc("/api/images/generate-tags", a.auth(a.generateMissingImageTags))
	mux.HandleFunc("/api/image/preview", a.auth(a.imagePreview))
	mux.HandleFunc("/api/overview", a.auth(a.overview))
	mux.HandleFunc("/api/image/update", a.auth(a.updateImage))
	mux.HandleFunc("/api/delete", a.auth(a.deleteImage))
	mux.HandleFunc("/api/notice", a.notice)
	mux.HandleFunc("/api/storage", a.auth(a.storageConfigAPI))
	mux.HandleFunc("/api/ai-config", a.auth(a.aiConfigAPI))
	mux.HandleFunc("/api/notify-config", a.auth(a.notifyConfigAPI))
	mux.HandleFunc("/api/notify-test", a.auth(a.notifyTestAPI))
	mux.HandleFunc("/api/guest-upload-config", a.auth(a.guestUploadConfigAPI))
	mux.HandleFunc("/api/security-config", a.auth(a.securityConfigAPI))
	mux.HandleFunc("/api/themes", a.auth(a.themesAPI))
	mux.HandleFunc("/api/themes/upload", a.auth(a.uploadTheme))
	mux.HandleFunc("/api/themes/activate", a.auth(a.activateTheme))
	mux.HandleFunc("/api/themes/delete", a.auth(a.deleteTheme))
	mux.HandleFunc("/api/login-logs", a.auth(a.loginLogsAPI))
	mux.HandleFunc("/api/operation-logs", a.auth(a.operationLogsAPI))

	log.Printf("Himg 已启动: http://127.0.0.1%s  存储: %s  主题: %s", addr, a.currentStorage().Name(), a.currentTheme())
	log.Fatal(http.ListenAndServe(addr, a.securityMiddleware(cors(logRequest(mux)))))
}

// 初始化图片记录表。
func initDB(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS images (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			path TEXT NOT NULL,
			size INTEGER NOT NULL,
			width INTEGER NOT NULL DEFAULT 0,
			height INTEGER NOT NULL DEFAULT 0,
			upload_ip TEXT NOT NULL DEFAULT '',
			view_count INTEGER NOT NULL DEFAULT 0,
			hidden INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		return err
	}
	if err = ensureColumn(db, "images", "hidden", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err = ensureColumn(db, "images", "width", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err = ensureColumn(db, "images", "height", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err = ensureColumn(db, "images", "upload_ip", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err = ensureColumn(db, "images", "view_count", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err = ensureColumn(db, "images", "tags", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err = ensureColumn(db, "images", "storage_type", "TEXT NOT NULL DEFAULT 'local'"); err != nil {
		return err
	}
	if err = ensureColumn(db, "images", "storage_config", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if _, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS image_views (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			image_path TEXT NOT NULL,
			view_ip TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)
	`); err != nil {
		return err
	}
	if _, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_image_views_created_at ON image_views(created_at)`); err != nil {
		return err
	}
	if _, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS homepage_views (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			view_ip TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)
	`); err != nil {
		return err
	}
	if _, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_homepage_views_created_at ON homepage_views(created_at)`); err != nil {
		return err
	}
	if _, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS login_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ip TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT '',
			message TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)
	`); err != nil {
		return err
	}
	if _, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_login_logs_created_at ON login_logs(created_at)`); err != nil {
		return err
	}
	if _, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS operation_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ip TEXT NOT NULL DEFAULT '',
			action TEXT NOT NULL DEFAULT '',
			target TEXT NOT NULL DEFAULT '',
			detail TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)
	`); err != nil {
		return err
	}
	if _, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_operation_logs_created_at ON operation_logs(created_at)`); err != nil {
		return err
	}

	// 公告和存储配置只保留一条记录，固定使用 id=1。
	if _, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS settings (
			id INTEGER PRIMARY KEY,
			notice TEXT NOT NULL DEFAULT '',
			notice_enabled INTEGER NOT NULL DEFAULT 1,
			storage_type TEXT NOT NULL DEFAULT 'local',
			storage_config TEXT NOT NULL DEFAULT '',
			active_theme TEXT NOT NULL DEFAULT '',
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)
	`); err != nil {
		return err
	}
	if err = ensureColumn(db, "settings", "storage_type", "TEXT NOT NULL DEFAULT 'local'"); err != nil {
		return err
	}
	if err = ensureColumn(db, "settings", "notice_enabled", "INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	if err = ensureColumn(db, "settings", "storage_config", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err = ensureColumn(db, "settings", "active_theme", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err = ensureColumn(db, "settings", "admin_password", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err = ensureColumn(db, "settings", "ai_base_url", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err = ensureColumn(db, "settings", "ai_model", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err = ensureColumn(db, "settings", "ai_key", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err = ensureColumn(db, "settings", "ai_wire_api", "TEXT NOT NULL DEFAULT 'responses'"); err != nil {
		return err
	}
	if err = ensureColumn(db, "settings", "ai_tag_prompt", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err = ensureColumn(db, "settings", "ai_review_prompt", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err = ensureColumn(db, "settings", "notify_config", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err = ensureColumn(db, "settings", "notify_state", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err = ensureColumn(db, "settings", "guest_upload_config", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err = ensureColumn(db, "settings", "security_config", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err = ensureColumn(db, "settings", "home_tagline", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err = ensureColumn(db, "settings", "browser_title", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	_, err = db.Exec(`
		INSERT INTO settings(id, notice)
		SELECT 1, ''
		WHERE NOT EXISTS (SELECT 1 FROM settings WHERE id = 1)
	`)
	if err != nil {
		return err
	}
	if err = cleanupLegacyDailyNotifyTasks(db); err != nil {
		return err
	}
	return backfillImageStorageMetadata(db)
}

func backfillImageStorageMetadata(db *sql.DB) error {
	var storageType, rawStorageConfig string
	if err := db.QueryRow(`SELECT storage_type, storage_config FROM settings WHERE id = 1`).Scan(&storageType, &rawStorageConfig); err != nil {
		return err
	}
	currentCfg := parseStorageConfig(rawStorageConfig)
	if strings.TrimSpace(currentCfg.Type) == "" {
		currentCfg.Type = storageType
	}
	currentCfg = normalizeStorageConfig(currentCfg)
	currentStorageType := normalizeImageStorageType(currentCfg.Type)
	currentStorageConfig := marshalStorageConfig(currentCfg)

	localCfg := currentCfg
	localCfg.Type = "local"
	if strings.TrimSpace(localCfg.LocalDir) == "" {
		localCfg.LocalDir = defaultLocalUploadDir()
	}
	localStorageConfig := marshalStorageConfig(localCfg)

	rows, err := db.Query(`SELECT id, path FROM images WHERE storage_config = ''`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type pendingImageStorage struct {
		id            int64
		storageType   string
		storageConfig string
	}
	pending := make([]pendingImageStorage, 0)
	for rows.Next() {
		var id int64
		var imagePath string
		if err := rows.Scan(&id, &imagePath); err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(localCfg.LocalDir, imagePath)); err == nil {
			pending = append(pending, pendingImageStorage{id: id, storageType: "local", storageConfig: localStorageConfig})
			continue
		}
		pending = append(pending, pendingImageStorage{id: id, storageType: currentStorageType, storageConfig: currentStorageConfig})
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, item := range pending {
		if _, err := db.Exec(`UPDATE images SET storage_type = ?, storage_config = ? WHERE id = ?`, item.storageType, item.storageConfig, item.id); err != nil {
			return err
		}
	}
	return nil
}

// 首页。
func (a *app) indexPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet {
		a.recordHomepageView(clientIP(r))
	}
	a.servePage(w, r, "index.html")
}

// 管理页。
func (a *app) adminPage(w http.ResponseWriter, r *http.Request) {
	if !a.isAuthed(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	a.servePage(w, r, "admin.html")
}

// 返回 API 使用说明，便于脚本和第三方程序接入。
func (a *app) about(w http.ResponseWriter, r *http.Request) {
	baseURL := requestBaseURL(r)
	examplePassword := "YOUR_ADMIN_PASSWORD"
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    "Himg",
		"version": "0.1",
		"storage": a.currentStorage().Name(),
		"auth": map[string]string{
			"cookie": "后台页先调用 POST /api/login 登录，后续自动带 Cookie",
			"header": "X-API-Password: 管理密码",
			"bearer": "Authorization: Bearer 管理密码",
			"remark": "上传和读取公告可公开访问，管理接口需要登录或密码请求头",
		},
		"endpoints": []map[string]string{
			{
				"name":   "登录",
				"method": "POST",
				"path":   "/api/login",
			},
			{
				"name":   "退出",
				"method": "POST",
				"path":   "/api/logout",
			},
			{
				"name":   "修改密码",
				"method": "POST",
				"path":   "/api/password",
			},
			{
				"name":   "上传图片",
				"method": "POST",
				"path":   "/api/upload",
			},
			{
				"name":   "图片列表",
				"method": "GET",
				"path":   "/api/images",
			},
			{
				"name":   "图片向量搜索",
				"method": "GET",
				"path":   "/api/images/search",
			},
			{
				"name":   "后台概览",
				"method": "GET",
				"path":   "/api/overview",
			},
			{
				"name":   "更新图片信息",
				"method": "POST",
				"path":   "/api/image/update",
			},
			{
				"name":   "删除图片",
				"method": "POST",
				"path":   "/api/delete",
			},
			{
				"name":   "公告读取/更新",
				"method": "GET/POST",
				"path":   "/api/notice",
			},
			{
				"name":   "存储配置读取/更新",
				"method": "GET/POST",
				"path":   "/api/storage",
			},
			{
				"name":   "AI 配置读取/更新",
				"method": "GET/POST",
				"path":   "/api/ai-config",
			},
			{
				"name":   "通知配置读取/更新",
				"method": "GET/POST",
				"path":   "/api/notify-config",
			},
			{
				"name":   "发送测试通知",
				"method": "POST",
				"path":   "/api/notify-test",
			},
			{
				"name":   "主题列表/上传/切换",
				"method": "GET/POST",
				"path":   "/api/themes*",
			},
		},
		"examples": map[string]string{
			"upload":        fmt.Sprintf("curl -X POST -F image=@demo.png %s/api/upload", baseURL),
			"list":          fmt.Sprintf("curl -H 'X-API-Password: %s' %s/api/images", examplePassword, baseURL),
			"rename_hide":   fmt.Sprintf("curl -X POST -H 'Content-Type: application/json' -H 'X-API-Password: %s' -d '{\"id\":1,\"name\":\"demo.webp\",\"hidden\":true}' %s/api/image/update", examplePassword, baseURL),
			"delete":        fmt.Sprintf("curl -X POST -H 'Content-Type: application/json' -H 'X-API-Password: %s' -d '{\"id\":1}' %s/api/delete", examplePassword, baseURL),
			"notice":        fmt.Sprintf("curl -X POST -H 'Content-Type: application/json' -H 'X-API-Password: %s' -d '{\"content\":\"今晚 23:00 维护\"}' %s/api/notice", examplePassword, baseURL),
			"storage":       fmt.Sprintf("curl -X POST -H 'Content-Type: application/json' -H 'X-API-Password: %s' -d '{\"type\":\"s3\"}' %s/api/storage", examplePassword, baseURL),
			"ai_config":     fmt.Sprintf("curl -X POST -H 'Content-Type: application/json' -H 'X-API-Password: %s' -d '{\"base_url\":\"https://api.openai.com/v1\",\"wire_api\":\"responses\",\"model\":\"gpt-5\",\"key\":\"sk-***\",\"tag_prompt\":\"返回 3-12 个中文标签\",\"review_prompt\":\"判断是否需要隐藏\"}' %s/api/ai-config", examplePassword, baseURL),
			"notify_config": fmt.Sprintf("curl -X POST -H 'Content-Type: application/json' -H 'X-API-Password: %s' -d '{\"enabled\":true,\"events\":{\"new_upload\":true,\"sensitive_review\":true},\"server_chan\":{\"enabled\":true,\"send_key\":\"SCTxxxx\"}}' %s/api/notify-config", examplePassword, baseURL),
			"notify_test":   fmt.Sprintf("curl -X POST -H 'X-API-Password: %s' %s/api/notify-test", examplePassword, baseURL),
			"theme":         fmt.Sprintf("curl -X POST -H 'X-API-Password: %s' -F theme=@my-theme.zip %s/api/themes/upload", examplePassword, baseURL),
			"password":      fmt.Sprintf("curl -X POST -H 'Content-Type: application/json' -H 'X-API-Password: %s' -d '{\"old_password\":\"CURRENT_PASSWORD\",\"new_password\":\"newpass123\"}' %s/api/password", examplePassword, baseURL),
			"theme_package": "主题压缩包根目录至少包含 theme.css，可选附带 theme.js、index.html、admin.html 和 assets/ 资源目录",
		},
		"storage_env": []string{
			"HIMG_STORAGE=local|webdav|s3",
			"HIMG_PUBLIC_BASE_URL=公开访问前缀，可选",
			"WEBDAV_URL / WEBDAV_USER / WEBDAV_PASSWORD / WEBDAV_BASE_PATH",
			"S3_ENDPOINT / S3_ACCESS_KEY / S3_SECRET_KEY / S3_BUCKET / S3_REGION / S3_USE_SSL / S3_PREFIX",
		},
	})
}

func (a *app) uploadPolicy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "请求方式错误"})
		return
	}
	cfg, err := a.loadGuestUploadConfig()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"upload_speed_kb_per_second": 0,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"upload_speed_kb_per_second": cfg.UploadSpeedKBPerSecond,
	})
}

func (a *app) session(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "请求方式错误"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{
		"authenticated": a.isAuthed(r),
	})
}

func defaultSiteConfig() siteConfig {
	return siteConfig{
		HomeTagline:  "Himg beta",
		BrowserTitle: "氢图床 Himg v0.1",
	}
}

func normalizeSiteConfig(cfg siteConfig) siteConfig {
	defaults := defaultSiteConfig()
	cfg.HomeTagline = strings.TrimSpace(cfg.HomeTagline)
	cfg.BrowserTitle = strings.TrimSpace(cfg.BrowserTitle)
	if cfg.HomeTagline == "" {
		cfg.HomeTagline = defaults.HomeTagline
	}
	if cfg.BrowserTitle == "" {
		cfg.BrowserTitle = defaults.BrowserTitle
	}
	if len([]rune(cfg.HomeTagline)) > 40 {
		cfg.HomeTagline = string([]rune(cfg.HomeTagline)[:40])
	}
	if len([]rune(cfg.BrowserTitle)) > 80 {
		cfg.BrowserTitle = string([]rune(cfg.BrowserTitle)[:80])
	}
	return cfg
}

func (a *app) siteConfigAPI(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg, err := a.loadSiteConfig()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取站点文案失败"})
			return
		}
		writeJSON(w, http.StatusOK, cfg)
	case http.MethodPost:
		if !a.isAuthed(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "请先登录"})
			return
		}
		var cfg siteConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "参数错误"})
			return
		}
		cfg = normalizeSiteConfig(cfg)
		if err := a.saveSiteConfig(cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存站点文案失败"})
			return
		}
		a.recordOperationLog(clientIP(r), "更新站点文案", "site-config", fmt.Sprintf("主页标识 %s，浏览器标题 %s", cfg.HomeTagline, cfg.BrowserTitle))
		writeJSON(w, http.StatusOK, map[string]any{
			"message": "站点文案已更新",
			"item":    cfg,
		})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "请求方式错误"})
	}
}

// 只用密码登录，成功后写入 Cookie。
func (a *app) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "请求方式错误"})
		return
	}

	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "参数错误"})
		return
	}
	if body.Password != a.currentPassword() {
		ip := clientIP(r)
		a.recordLoginLog(ip, "failed", "登录失败：密码错误")
		if blocked, err := a.blockIPAfterRepeatedLoginFailures(ip); err != nil {
			log.Printf("自动封禁登录失败 IP 失败: %v", err)
		} else if blocked {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "密码错误，当前 IP 已加入安全黑名单"})
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "密码错误"})
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    a.token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400 * 7,
	})
	a.recordLoginLog(clientIP(r), "success", "登录成功")
	writeJSON(w, http.StatusOK, map[string]string{"message": "登录成功"})
}

func (a *app) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	a.recordLoginLog(clientIP(r), "logout", "退出登录")
	writeJSON(w, http.StatusOK, map[string]string{"message": "已退出登录"})
}

func (a *app) changePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "请求方式错误"})
		return
	}

	var body struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "参数错误"})
		return
	}

	oldPassword := strings.TrimSpace(body.OldPassword)
	newPassword := strings.TrimSpace(body.NewPassword)
	if oldPassword == "" || newPassword == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "旧密码和新密码不能为空"})
		return
	}
	if oldPassword != a.currentPassword() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "旧密码不正确"})
		return
	}
	if len(newPassword) < 6 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "新密码至少 6 位"})
		return
	}
	if newPassword == oldPassword {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "新密码不能与旧密码相同"})
		return
	}

	if err := a.saveAdminPassword(newPassword); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存新密码失败"})
		return
	}
	a.setPassword(newPassword)
	a.recordOperationLog(clientIP(r), "修改密码", "admin", "后台管理员密码已更新")
	writeJSON(w, http.StatusOK, map[string]string{"message": "密码已更新"})
}

// 上传图片并写入数据库。
func (a *app) upload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "请求方式错误"})
		return
	}
	uploadIP := clientIP(r)
	guestCfg, err := a.checkGuestUploadAccess(uploadIP)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	if guestCfg.UploadSpeedKBPerSecond > 0 && r.Body != nil {
		r.Body = newThrottledReadCloser(r.Body, guestCfg.UploadSpeedKBPerSecond)
	}
	if err := r.ParseMultipartForm(20 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "表单错误或文件过大"})
		return
	}
	if r.MultipartForm == nil || len(r.MultipartForm.File["image"]) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请选择图片"})
		return
	}
	if len(r.MultipartForm.File["image"]) > 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "每次只能上传一张图片"})
		return
	}

	file, header, err := r.FormFile("image")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请选择图片"})
		return
	}
	defer file.Close()

	if !isImage(header.Filename) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "仅支持 jpg png gif webp"})
		return
	}

	uid := fmt.Sprintf("%d", time.Now().UnixNano())
	name := uid + ".webp"

	data, err := io.ReadAll(file)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取失败"})
		return
	}

	img, err := decodeImage(data, header.Filename)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "图片解析失败"})
		return
	}

	var buf bytes.Buffer
	// 统一转成 WebP，减少体积并保持外链格式一致。
	if err = webp.Encode(&buf, img, &webp.Options{Lossless: false, Quality: 80}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "WebP 转换失败"})
		return
	}

	size := int64(buf.Len())
	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	storage := a.currentStorage()
	storageType := normalizeImageStorageType(storage.Name())
	storageConfig, err := a.loadStorageConfig()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取存储配置失败"})
		return
	}
	if err = storage.Save(r.Context(), name, buf.Bytes(), "image/webp"); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存失败"})
		return
	}

	tags := make([]string, 0)
	autoHidden, hideReason := shouldAutoHideByPolicy(header.Filename, tags)

	res, err := a.db.Exec(`INSERT INTO images(name, path, size, width, height, upload_ip, hidden, tags, storage_type, storage_config) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, name, name, size, width, height, uploadIP, boolToInt(autoHidden), marshalTags(tags), storageType, marshalStorageConfig(storageConfig))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "数据库写入失败"})
		return
	}
	id, _ := res.LastInsertId()

	publicURL := storage.PublicURL(r, name)
	message := "上传成功"
	if autoHidden {
		message = "上传成功，已默认隐藏（可在管理界面手动显示）"
	}
	imageData := append([]byte(nil), buf.Bytes()...)
	a.recordOperationLog(uploadIP, "上传图片", name, fmt.Sprintf("尺寸 %dx%d，大小 %s", width, height, formatBytesSimple(size)))
	writeJSON(w, http.StatusOK, map[string]any{
		"message": message,
		"item": imageItem{
			ID:           id,
			Name:         name,
			URL:          publicURL,
			Size:         size,
			Width:        width,
			Height:       height,
			UploadIP:     uploadIP,
			StorageType:  storageType,
			StorageLabel: storageTypeLabel(storageType),
			ViewCount:    0,
			Hidden:       autoHidden,
			Tags:         tags,
			CreatedAt:    time.Now().Format("2006-01-02 15:04:05"),
		},
		"url":                publicURL,
		"markdown":           fmt.Sprintf("![](%s)", publicURL),
		"auto_hidden":        autoHidden,
		"auto_hidden_reason": hideReason,
	})
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	go a.processUploadedImageAI(id, name, header.Filename, imageData, publicURL, size, width, height, uploadIP, autoHidden, hideReason)
}

func (a *app) processUploadedImageAI(imageID int64, name, originalFilename string, imageData []byte, publicURL string, size int64, width, height int, uploadIP string, initialHidden bool, initialHideReason string) {
	ctx := context.Background()
	tags := make([]string, 0)
	if aiTags, err := a.generateImageTags(ctx, imageData); err == nil {
		tags = aiTags
	} else {
		log.Printf("AI 标签生成失败 image_id=%d name=%s err=%v", imageID, name, err)
	}

	autoHidden := initialHidden
	hideReason := initialHideReason
	if policyHidden, policyReason := shouldAutoHideByPolicy(originalFilename, tags); policyHidden {
		autoHidden = true
		hideReason = policyReason
	}
	if !autoHidden {
		if aiHidden, aiReason, err := a.shouldAutoHideByAI(ctx, imageData, originalFilename, tags); err == nil && aiHidden {
			autoHidden = true
			hideReason = aiReason
		} else if err != nil {
			log.Printf("AI 审核失败 image_id=%d name=%s err=%v", imageID, name, err)
		}
	}

	if _, err := a.db.Exec(`UPDATE images SET tags = ?, hidden = ? WHERE id = ?`, marshalTags(tags), boolToInt(autoHidden), imageID); err != nil {
		log.Printf("AI 标签/审核结果写入失败 image_id=%d name=%s err=%v", imageID, name, err)
		return
	}
	a.notifyUploadEvents(name, publicURL, size, width, height, uploadIP, autoHidden, hideReason, tags)
}

// 读取图片列表。
func (a *app) images(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pageRaw := strings.TrimSpace(q.Get("page"))
	pageSizeRaw := strings.TrimSpace(q.Get("page_size"))
	if pageRaw == "" && pageSizeRaw == "" {
		rows, err := a.db.Query(`SELECT id, name, path, size, width, height, upload_ip, storage_type, storage_config, view_count, hidden, tags, created_at FROM images ORDER BY id DESC`)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取失败"})
			return
		}
		defer rows.Close()

		list := make([]imageItem, 0, 16)
		for rows.Next() {
			var item imageItem
			var path string
			var rawStorageConfig string
			var hidden int
			var rawTags string
			if err = rows.Scan(&item.ID, &item.Name, &path, &item.Size, &item.Width, &item.Height, &item.UploadIP, &item.StorageType, &rawStorageConfig, &item.ViewCount, &hidden, &rawTags, &item.CreatedAt); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "解析失败"})
				return
			}
			item.StorageType = normalizeImageStorageType(item.StorageType)
			item.StorageLabel = storageTypeLabel(item.StorageType)
			item.Hidden = hidden == 1
			item.Tags = parseTags(rawTags)
			item.URL = imagePublicURL(r, item.StorageType, rawStorageConfig, path)
			list = append(list, item)
		}

		writeJSON(w, http.StatusOK, map[string]any{"items": list})
		return
	}

	page := 1
	pageSize := 20
	var err error
	if pageRaw != "" {
		page, err = strconv.Atoi(pageRaw)
		if err != nil || page < 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "page 参数错误"})
			return
		}
	}
	if pageSizeRaw != "" {
		pageSize, err = strconv.Atoi(pageSizeRaw)
		if err != nil || pageSize < 1 || pageSize > 100 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "page_size 参数错误，范围 1-100"})
			return
		}
	}

	var total int64
	if err = a.db.QueryRow(`SELECT COUNT(*) FROM images`).Scan(&total); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取总数失败"})
		return
	}
	totalPages := int((total + int64(pageSize) - 1) / int64(pageSize))
	if totalPages == 0 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}
	offset := (page - 1) * pageSize

	rows, err := a.db.Query(`SELECT id, name, path, size, width, height, upload_ip, storage_type, storage_config, view_count, hidden, tags, created_at FROM images ORDER BY id DESC LIMIT ? OFFSET ?`, pageSize, offset)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取失败"})
		return
	}
	defer rows.Close()

	list := make([]imageItem, 0, 16)
	for rows.Next() {
		var item imageItem
		var path string
		var rawStorageConfig string
		var hidden int
		var rawTags string
		if err = rows.Scan(&item.ID, &item.Name, &path, &item.Size, &item.Width, &item.Height, &item.UploadIP, &item.StorageType, &rawStorageConfig, &item.ViewCount, &hidden, &rawTags, &item.CreatedAt); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "解析失败"})
			return
		}
		item.StorageType = normalizeImageStorageType(item.StorageType)
		item.StorageLabel = storageTypeLabel(item.StorageType)
		item.Hidden = hidden == 1
		item.Tags = parseTags(rawTags)
		item.URL = imagePublicURL(r, item.StorageType, rawStorageConfig, path)
		list = append(list, item)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"items":       list,
		"total":       total,
		"page":        page,
		"page_size":   pageSize,
		"total_pages": totalPages,
		"has_prev":    page > 1,
		"has_next":    page < totalPages,
	})
}

func (a *app) searchImages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "请求方式错误"})
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		q = strings.TrimSpace(r.URL.Query().Get("keyword"))
	}
	if q == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请提供搜索关键词"})
		return
	}

	page := 1
	pageSize := 10
	var err error
	if raw := strings.TrimSpace(r.URL.Query().Get("page")); raw != "" {
		page, err = strconv.Atoi(raw)
		if err != nil || page < 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "page 参数错误"})
			return
		}
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("page_size")); raw != "" {
		pageSize, err = strconv.Atoi(raw)
		if err != nil || pageSize < 1 || pageSize > 100 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "page_size 参数错误，范围 1-100"})
			return
		}
	}

	rows, err := a.db.Query(`SELECT id, name, path, size, width, height, upload_ip, storage_type, storage_config, view_count, hidden, tags, created_at FROM images ORDER BY id DESC`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取失败"})
		return
	}
	defer rows.Close()

	queryVec := textVector(strings.ToLower(q), 256)
	expandedTerms := sanitizeTags([]string{q})
	list := make([]imageItem, 0, 64)
	for rows.Next() {
		var item imageItem
		var path string
		var rawStorageConfig string
		var hidden int
		var rawTags string
		if err = rows.Scan(&item.ID, &item.Name, &path, &item.Size, &item.Width, &item.Height, &item.UploadIP, &item.StorageType, &rawStorageConfig, &item.ViewCount, &hidden, &rawTags, &item.CreatedAt); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "解析失败"})
			return
		}
		item.StorageType = normalizeImageStorageType(item.StorageType)
		item.StorageLabel = storageTypeLabel(item.StorageType)
		item.Hidden = hidden == 1
		item.Tags = parseTags(rawTags)
		item.URL = imagePublicURL(r, item.StorageType, rawStorageConfig, path)

		corpus := strings.ToLower(item.Name + " " + strings.Join(item.Tags, " "))
		item.Score = cosineSimilarity(queryVec, textVector(corpus, 256))
		item.Score += lexicalMatchScore(corpus, expandedTerms)
		if item.Score > 1 {
			item.Score = 1
		}
		list = append(list, item)
	}

	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Score == list[j].Score {
			return list[i].ID > list[j].ID
		}
		return list[i].Score > list[j].Score
	})
	filtered := make([]imageItem, 0, len(list))
	lowerQ := strings.ToLower(q)
	for _, item := range list {
		corpus := strings.ToLower(item.Name + " " + strings.Join(item.Tags, " "))
		if item.Score >= 0.07 || strings.Contains(corpus, lowerQ) || lexicalMatchScore(corpus, expandedTerms) >= 0.12 {
			filtered = append(filtered, item)
		}
	}
	if len(filtered) == 0 {
		fallback := len(list)
		if fallback > 20 {
			fallback = 20
		}
		filtered = append(filtered, list[:fallback]...)
	}
	list = filtered

	total := len(list)
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages == 0 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * pageSize
	end := start + pageSize
	if end > total {
		end = total
	}
	if start > end {
		start = end
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"items":       list[start:end],
		"total":       total,
		"page":        page,
		"page_size":   pageSize,
		"total_pages": totalPages,
		"has_prev":    page > 1,
		"has_next":    page < totalPages,
		"keyword":     q,
	})
}

func (a *app) imagePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "请求方式错误"})
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("id")), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id 参数错误"})
		return
	}

	var path, storageType, rawStorageConfig string
	if err = a.db.QueryRow(`SELECT path, storage_type, storage_config FROM images WHERE id = ?`, id).Scan(&path, &storageType, &rawStorageConfig); err != nil {
		if err == sql.ErrNoRows {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取图片失败"})
		return
	}

	storage, err := imageStorageBackend(storageType, rawStorageConfig)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "解析图片存储失败"})
		return
	}
	raw, err := readImageBytesFromStorage(r.Context(), r, storage, path)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取原图失败"})
		return
	}
	img, err := decodeImage(raw, path)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "解码原图失败"})
		return
	}

	out := downscaleIfNeeded(img, 720)
	var buf bytes.Buffer
	if err = webp.Encode(&buf, out, &webp.Options{Lossless: false, Quality: 45}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "生成预览失败"})
		return
	}

	w.Header().Set("Content-Type", "image/webp")
	w.Header().Set("Cache-Control", "private, max-age=300")
	_, _ = w.Write(buf.Bytes())
}

func (a *app) updateImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "请求方式错误"})
		return
	}

	var body struct {
		ID     int64     `json:"id"`
		Name   *string   `json:"name"`
		Hidden *bool     `json:"hidden"`
		Tags   *[]string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "参数错误"})
		return
	}
	if body.ID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请提供有效 id"})
		return
	}
	if body.Name == nil && body.Hidden == nil && body.Tags == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "至少更新一个字段"})
		return
	}

	sets := make([]string, 0, 3)
	args := make([]any, 0, 4)
	if body.Name != nil {
		name := strings.TrimSpace(*body.Name)
		if name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "名称不能为空"})
			return
		}
		sets = append(sets, "name = ?")
		args = append(args, name)
	}
	if body.Hidden != nil {
		sets = append(sets, "hidden = ?")
		args = append(args, boolToInt(*body.Hidden))
	}
	if body.Tags != nil {
		sets = append(sets, "tags = ?")
		args = append(args, marshalTags(*body.Tags))
	}

	args = append(args, body.ID)
	query := fmt.Sprintf("UPDATE images SET %s WHERE id = ?", strings.Join(sets, ", "))
	res, err := a.db.Exec(query, args...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "更新失败"})
		return
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "图片不存在"})
		return
	}

	var item imageItem
	var path string
	var rawStorageConfig string
	var hidden int
	var rawTags string
	if err = a.db.QueryRow(`SELECT id, name, path, size, width, height, upload_ip, storage_type, storage_config, view_count, hidden, tags, created_at FROM images WHERE id = ?`, body.ID).
		Scan(&item.ID, &item.Name, &path, &item.Size, &item.Width, &item.Height, &item.UploadIP, &item.StorageType, &rawStorageConfig, &item.ViewCount, &hidden, &rawTags, &item.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "图片不存在"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取更新结果失败"})
		return
	}
	item.StorageType = normalizeImageStorageType(item.StorageType)
	item.StorageLabel = storageTypeLabel(item.StorageType)
	item.Hidden = hidden == 1
	item.Tags = parseTags(rawTags)
	item.URL = imagePublicURL(r, item.StorageType, rawStorageConfig, path)
	changes := make([]string, 0, 3)
	if body.Name != nil {
		changes = append(changes, "名称")
	}
	if body.Hidden != nil {
		if *body.Hidden {
			changes = append(changes, "隐藏")
		} else {
			changes = append(changes, "取消隐藏")
		}
	}
	if body.Tags != nil {
		changes = append(changes, "标签")
	}
	a.recordOperationLog(clientIP(r), "更新图片", item.Name, fmt.Sprintf("更新字段：%s", strings.Join(changes, "、")))
	writeJSON(w, http.StatusOK, map[string]any{
		"message": "图片信息已更新",
		"item":    item,
	})
}

func (a *app) generateMissingImageTags(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "请求方式错误"})
		return
	}

	cfg, err := a.loadAIConfig()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取 AI 配置失败"})
		return
	}
	if strings.TrimSpace(cfg.BaseURL) == "" || strings.TrimSpace(cfg.Model) == "" || strings.TrimSpace(cfg.Key) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请先完成 AI 配置"})
		return
	}

	type tagTarget struct {
		ID               int64
		Name             string
		Path             string
		StorageType      string
		RawStorageConfig string
		RawTags          string
	}
	rows, err := a.db.Query(`SELECT id, name, path, storage_type, storage_config, tags FROM images ORDER BY id ASC`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取图片失败"})
		return
	}
	defer rows.Close()

	targets := make([]tagTarget, 0, 16)
	for rows.Next() {
		var item tagTarget
		if err := rows.Scan(&item.ID, &item.Name, &item.Path, &item.StorageType, &item.RawStorageConfig, &item.RawTags); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "解析图片失败"})
			return
		}
		if len(parseTags(item.RawTags)) == 0 {
			targets = append(targets, item)
		}
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取图片失败"})
		return
	}
	if len(targets) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"message": "没有需要补全标签的图片",
			"total":   0,
			"updated": 0,
			"failed":  0,
		})
		return
	}

	updated := 0
	failed := 0
	for _, item := range targets {
		storage, err := imageStorageBackend(item.StorageType, item.RawStorageConfig)
		if err != nil {
			failed++
			log.Printf("补全图片标签失败 id=%d name=%s err=%v", item.ID, item.Name, err)
			continue
		}
		raw, err := readImageBytesFromStorage(r.Context(), r, storage, item.Path)
		if err != nil {
			failed++
			log.Printf("补全图片标签读取失败 id=%d name=%s err=%v", item.ID, item.Name, err)
			continue
		}
		tags, err := a.generateImageTags(r.Context(), raw)
		if err != nil || len(tags) == 0 {
			failed++
			if err != nil {
				log.Printf("补全图片标签 AI 失败 id=%d name=%s err=%v", item.ID, item.Name, err)
			}
			continue
		}
		if _, err := a.db.Exec(`UPDATE images SET tags = ? WHERE id = ? AND tags = ?`, marshalTags(tags), item.ID, item.RawTags); err != nil {
			failed++
			log.Printf("补全图片标签写入失败 id=%d name=%s err=%v", item.ID, item.Name, err)
			continue
		}
		updated++
	}

	a.recordOperationLog(clientIP(r), "AI 补全标签", "images", fmt.Sprintf("检测 %d 张，更新 %d 张，失败 %d 张", len(targets), updated, failed))
	writeJSON(w, http.StatusOK, map[string]any{
		"message": fmt.Sprintf("已补全 %d 张图片标签", updated),
		"total":   len(targets),
		"updated": updated,
		"failed":  failed,
	})
}

func (a *app) overview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "请求方式错误"})
		return
	}

	item, err := a.buildOverview(r)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取概览失败"})
		return
	}
	writeJSON(w, http.StatusOK, item)
}

// 删除图片，同时清理数据库记录和磁盘文件。
func (a *app) deleteImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "请求方式错误"})
		return
	}

	var body struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "参数错误"})
		return
	}

	var (
		id               int64
		name             string
		path             string
		storageType      string
		rawStorageConfig string
	)
	switch {
	case body.ID > 0:
		err := a.db.QueryRow(`SELECT id, name, path, storage_type, storage_config FROM images WHERE id = ?`, body.ID).Scan(&id, &name, &path, &storageType, &rawStorageConfig)
		if err == sql.ErrNoRows {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "图片不存在"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取失败"})
			return
		}
	case strings.TrimSpace(body.Name) != "":
		err := a.db.QueryRow(`SELECT id, name, path, storage_type, storage_config FROM images WHERE name = ?`, strings.TrimSpace(body.Name)).Scan(&id, &name, &path, &storageType, &rawStorageConfig)
		if err == sql.ErrNoRows {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "图片不存在"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取失败"})
			return
		}
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请提供 id 或 name"})
		return
	}

	if _, err := a.db.Exec(`DELETE FROM images WHERE id = ?`, id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "删除记录失败"})
		return
	}
	storage, err := imageStorageBackend(storageType, rawStorageConfig)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "解析图片存储失败"})
		return
	}
	if err := storage.Delete(r.Context(), path); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "删除文件失败"})
		return
	}
	a.recordOperationLog(clientIP(r), "删除图片", name, fmt.Sprintf("图片 ID %d 已删除", id))

	writeJSON(w, http.StatusOK, map[string]any{
		"message": "删除成功",
		"item": map[string]any{
			"id":   id,
			"name": name,
			"url":  storage.PublicURL(r, path),
		},
	})
}

func (a *app) storageConfigAPI(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg, err := a.loadStorageConfig()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取存储配置失败"})
			return
		}
		writeJSON(w, http.StatusOK, cfg)
	case http.MethodPost:
		var cfg storageConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "参数错误"})
			return
		}
		cfg = normalizeStorageConfig(cfg)
		storage, err := newStorageFromConfig(cfg)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := a.saveStorageConfig(cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存存储配置失败"})
			return
		}
		a.setStorage(storage)
		a.recordOperationLog(clientIP(r), "更新存储配置", formatStorageTypeLabel(cfg.Type), "存储配置已保存")
		writeJSON(w, http.StatusOK, map[string]any{
			"message": "存储配置已更新",
			"item":    cfg,
		})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "请求方式错误"})
	}
}

func normalizeAIConfig(cfg aiConfig) aiConfig {
	cfg.BaseURL = strings.TrimSpace(cfg.BaseURL)
	cfg.WireAPI = "responses"
	cfg.Model = strings.TrimSpace(cfg.Model)
	cfg.Key = strings.TrimSpace(cfg.Key)
	cfg.TagPrompt = strings.TrimSpace(cfg.TagPrompt)
	cfg.ReviewPrompt = strings.TrimSpace(cfg.ReviewPrompt)
	return cfg
}

func aiTagPrompt(cfg aiConfig) string {
	if strings.TrimSpace(cfg.TagPrompt) != "" {
		return cfg.TagPrompt
	}
	return "你是图片标签助手。根据图片返回 3-12 个中文标签。只能输出 JSON 字符串数组，如 [\"人物\",\"夜景\"]，不要输出其它文本。"
}

func aiReviewPrompt(cfg aiConfig) string {
	if strings.TrimSpace(cfg.ReviewPrompt) != "" {
		return cfg.ReviewPrompt
	}
	return "你是内容安全审核助手。判断图片是否属于：1)政治人物/政治敏感内容；2)色情露骨内容。严格只输出 JSON：{\"political\":布尔,\"porn\":布尔,\"reason\":\"简短中文原因\"}。"
}

func (a *app) aiConfigAPI(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg, err := a.loadAIConfig()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取 AI 配置失败"})
			return
		}
		writeJSON(w, http.StatusOK, cfg)
	case http.MethodPost:
		var cfg aiConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "参数错误"})
			return
		}
		cfg = normalizeAIConfig(cfg)
		if err := a.saveAIConfig(cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存 AI 配置失败"})
			return
		}
		target := cfg.Model
		if target == "" {
			target = "未设置模型"
		}
		a.recordOperationLog(clientIP(r), "更新 AI 配置", target, fmt.Sprintf("接口 %s", cfg.WireAPI))
		writeJSON(w, http.StatusOK, map[string]any{
			"message": "AI 配置已更新",
			"item":    cfg,
		})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "请求方式错误"})
	}
}

func normalizeNotifyConfig(cfg notifyConfig) notifyConfig {
	cfg.Server.SendKey = strings.TrimSpace(cfg.Server.SendKey)

	cfg.Email.Host = strings.TrimSpace(cfg.Email.Host)
	if cfg.Email.Port <= 0 || cfg.Email.Port > 65535 {
		cfg.Email.Port = 587
	}
	cfg.Email.Username = strings.TrimSpace(cfg.Email.Username)
	cfg.Email.Password = strings.TrimSpace(cfg.Email.Password)
	cfg.Email.From = strings.TrimSpace(cfg.Email.From)
	cfg.Email.To = strings.TrimSpace(cfg.Email.To)

	cfg.Telegram.BotToken = strings.TrimSpace(cfg.Telegram.BotToken)
	cfg.Telegram.ChatID = strings.TrimSpace(cfg.Telegram.ChatID)

	// 新增事件开关，老配置默认启用新图上传通知。
	if !cfg.Events.NewUpload && !cfg.Events.SensitiveReview {
		cfg.Events.NewUpload = true
	}
	return cfg
}

func (a *app) notifyConfigAPI(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg, err := a.loadNotifyConfig()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取通知配置失败"})
			return
		}
		writeJSON(w, http.StatusOK, cfg)
	case http.MethodPost:
		var cfg notifyConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "参数错误"})
			return
		}
		cfg = normalizeNotifyConfig(cfg)
		if err := a.saveNotifyConfig(cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存通知配置失败"})
			return
		}
		a.recordOperationLog(clientIP(r), "更新通知配置", "notify", "通知配置已保存")
		writeJSON(w, http.StatusOK, map[string]any{
			"message": "通知配置已更新",
			"item":    cfg,
		})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "请求方式错误"})
	}
}

func (a *app) notifyTestAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "请求方式错误"})
		return
	}
	cfg, err := a.loadNotifyConfig()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取通知配置失败"})
		return
	}
	if r.Body != nil {
		var body notifyConfig
		if decodeErr := json.NewDecoder(r.Body).Decode(&body); decodeErr == nil {
			cfg = normalizeNotifyConfig(body)
		} else if decodeErr != io.EOF {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "参数错误"})
			return
		}
	}
	if !cfg.Enabled {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请先启用通知总开关"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	results := a.sendNotification(ctx, "Himg 测试通知", "这是一条来自 Himg 的测试消息。", cfg)
	if results["server_chan"] == "skip" && results["email"] == "skip" && results["telegram"] == "skip" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请至少启用一个通知渠道并补全配置"})
		return
	}
	a.recordOperationLog(clientIP(r), "发送测试通知", "notify", "已提交测试通知")
	if hasNotifyError(results) {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":  "部分通知渠道发送失败",
			"result": results,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message": "测试通知已发送",
		"result":  results,
	})
}

func hasNotifyError(results map[string]string) bool {
	for _, status := range results {
		if strings.HasPrefix(status, "error:") {
			return true
		}
	}
	return false
}

func normalizeGuestUploadConfig(cfg guestUploadConfig) guestUploadConfig {
	cfg.BlockedIPs = normalizeIPRuleList(cfg.BlockedIPs)
	if cfg.MaxUploadsPerMinute < 0 {
		cfg.MaxUploadsPerMinute = 0
	}
	if cfg.UploadSpeedKBPerSecond < 0 {
		cfg.UploadSpeedKBPerSecond = 0
	}
	if cfg.UploadSpeedKBPerSecond > 102400 {
		cfg.UploadSpeedKBPerSecond = 102400
	}
	return cfg
}

func normalizeSecurityConfig(cfg securityConfig) securityConfig {
	cfg.BlockedIPs = normalizeIPRuleList(cfg.BlockedIPs)
	if cfg.RequestsPerMinute <= 0 {
		cfg.RequestsPerMinute = 120
	}
	if cfg.RequestsPerMinute > 5000 {
		cfg.RequestsPerMinute = 5000
	}
	if cfg.MaxBodyMB <= 0 {
		cfg.MaxBodyMB = 64
	}
	if cfg.MaxBodyMB > 512 {
		cfg.MaxBodyMB = 512
	}
	return cfg
}

func defaultSecurityConfig() securityConfig {
	return normalizeSecurityConfig(securityConfig{
		Enabled:                true,
		SecurityHeadersEnabled: true,
		RateLimitEnabled:       true,
		RequestsPerMinute:      120,
		MaxBodyMB:              64,
		InjectionFilterEnabled: true,
	})
}

func (a *app) guestUploadConfigAPI(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg, err := a.loadGuestUploadConfig()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取游客上传配置失败"})
			return
		}
		writeJSON(w, http.StatusOK, cfg)
	case http.MethodPost:
		var cfg guestUploadConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "参数错误"})
			return
		}
		cfg = normalizeGuestUploadConfig(cfg)
		if err := a.saveGuestUploadConfig(cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存游客上传配置失败"})
			return
		}
		detail := fmt.Sprintf("启用=%t，限额=%d 次/分钟", cfg.Enabled, cfg.MaxUploadsPerMinute)
		a.recordOperationLog(clientIP(r), "更新游客上传配置", "guest-upload", detail)
		writeJSON(w, http.StatusOK, map[string]any{
			"message": "游客上传配置已更新",
			"item":    cfg,
		})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "请求方式错误"})
	}
}

func (a *app) securityConfigAPI(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, a.currentSecurityConfig())
	case http.MethodPost:
		var cfg securityConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "参数错误"})
			return
		}
		cfg = normalizeSecurityConfig(cfg)
		if err := a.saveSecurityConfig(cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存安全配置失败"})
			return
		}
		a.setSecurityConfig(cfg)
		a.recordOperationLog(clientIP(r), "更新安全配置", "security", fmt.Sprintf("启用=%t，限额=%d 次/分钟", cfg.Enabled, cfg.RequestsPerMinute))
		writeJSON(w, http.StatusOK, map[string]any{
			"message": "安全配置已更新",
			"item":    cfg,
		})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "请求方式错误"})
	}
}

func (a *app) themesAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "请求方式错误"})
		return
	}
	items, err := listThemes()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取主题失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"active": a.currentTheme(),
		"items":  items,
	})
}

func (a *app) uploadTheme(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "请求方式错误"})
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "表单错误或文件过大"})
		return
	}
	file, header, err := r.FormFile("theme")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请选择主题压缩包"})
		return
	}
	defer file.Close()
	if !strings.EqualFold(filepath.Ext(header.Filename), ".zip") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "仅支持 zip 压缩包"})
		return
	}
	data, err := io.ReadAll(file)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取主题失败"})
		return
	}
	name := sanitizeThemeName(r.FormValue("name"))
	if name == "theme" {
		name = sanitizeThemeName(header.Filename)
	}
	if err := installThemeZip(name, data); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "主题安装失败，请确认 zip 根目录包含 theme.css"})
		return
	}
	a.recordOperationLog(clientIP(r), "上传主题", name, "主题压缩包已安装")
	writeJSON(w, http.StatusOK, map[string]any{
		"message": "主题上传成功",
		"item": map[string]string{
			"name": name,
		},
	})
}

func (a *app) activateTheme(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "请求方式错误"})
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "参数错误"})
		return
	}
	name := sanitizeThemeName(body.Name)
	if name != "" && name != "theme" && !themeExists(name) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "主题不存在"})
		return
	}
	if body.Name == "" {
		name = ""
	}
	if err := a.saveThemeName(name); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存主题失败"})
		return
	}
	a.setTheme(name)
	target := name
	if target == "" {
		target = defaultThemeName
	}
	a.recordOperationLog(clientIP(r), "切换主题", target, "主题已切换")
	writeJSON(w, http.StatusOK, map[string]any{
		"message": "主题已切换",
		"active":  name,
	})
}

func (a *app) deleteTheme(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "请求方式错误"})
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "参数错误"})
		return
	}
	name := sanitizeThemeName(body.Name)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "主题名不能为空"})
		return
	}
	if name == defaultThemeName {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "默认主题不能删除"})
		return
	}
	if a.currentTheme() == name {
		if err := a.saveThemeName(""); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "切回默认主题失败"})
			return
		}
		a.setTheme("")
	}
	if err := os.RemoveAll(filepath.Join(themesDir(), name)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "删除主题失败"})
		return
	}
	a.recordOperationLog(clientIP(r), "删除主题", name, "主题目录已删除")
	writeJSON(w, http.StatusOK, map[string]string{"message": "主题已删除"})
}

// 读取或更新公告。
func (a *app) notice(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		item, err := a.getNotice()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取公告失败"})
			return
		}
		writeJSON(w, http.StatusOK, item)
	case http.MethodPost:
		if !a.isAuthed(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "请先登录"})
			return
		}
		var body struct {
			Content string `json:"content"`
			Enabled *bool  `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "参数错误"})
			return
		}
		content := strings.TrimSpace(body.Content)
		enabled := content != ""
		if body.Enabled != nil {
			enabled = *body.Enabled
		}
		if _, err := a.db.Exec(`UPDATE settings SET notice = ?, notice_enabled = ?, updated_at = CURRENT_TIMESTAMP WHERE id = 1`, content, boolToInt(enabled)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存公告失败"})
			return
		}
		item, err := a.getNotice()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取公告失败"})
			return
		}
		statusText := "关闭"
		if item.Enabled {
			statusText = "启用"
		}
		a.recordOperationLog(clientIP(r), "更新公告", statusText, fmt.Sprintf("公告长度 %d 字", len([]rune(item.Content))))
		writeJSON(w, http.StatusOK, map[string]any{"message": "公告已更新", "item": item})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "请求方式错误"})
	}
}

func (a *app) loginLogsAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "请求方式错误"})
		return
	}
	page, pageSize, keyword, ok := parseLogListParams(w, r)
	if !ok {
		return
	}
	where := ""
	args := make([]any, 0, 4)
	if keyword != "" {
		like := "%" + keyword + "%"
		where = "WHERE ip LIKE ? OR status LIKE ? OR message LIKE ?"
		args = append(args, like, like, like)
	}
	var total int64
	countQuery := "SELECT COUNT(*) FROM login_logs " + where
	if err := a.db.QueryRow(countQuery, args...).Scan(&total); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取登录日志总数失败"})
		return
	}
	totalPages := calcTotalPages(total, pageSize)
	if page > totalPages {
		page = totalPages
	}
	offset := (page - 1) * pageSize
	queryArgs := append(append([]any{}, args...), pageSize, offset)
	rows, err := a.db.Query("SELECT id, ip, status, message, created_at FROM login_logs "+where+" ORDER BY id DESC LIMIT ? OFFSET ?", queryArgs...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取登录日志失败"})
		return
	}
	defer rows.Close()
	items := make([]loginLogItem, 0, pageSize)
	for rows.Next() {
		var item loginLogItem
		if err := rows.Scan(&item.ID, &item.IP, &item.Status, &item.Message, &item.CreatedAt); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "解析登录日志失败"})
			return
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":       items,
		"total":       total,
		"page":        page,
		"page_size":   pageSize,
		"total_pages": totalPages,
		"has_prev":    page > 1,
		"has_next":    page < totalPages,
		"keyword":     keyword,
	})
}

func (a *app) operationLogsAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "请求方式错误"})
		return
	}
	page, pageSize, keyword, ok := parseLogListParams(w, r)
	if !ok {
		return
	}
	where := ""
	args := make([]any, 0, 4)
	if keyword != "" {
		like := "%" + keyword + "%"
		where = "WHERE ip LIKE ? OR action LIKE ? OR target LIKE ? OR detail LIKE ?"
		args = append(args, like, like, like, like)
	}
	var total int64
	countQuery := "SELECT COUNT(*) FROM operation_logs " + where
	if err := a.db.QueryRow(countQuery, args...).Scan(&total); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取操作日志总数失败"})
		return
	}
	totalPages := calcTotalPages(total, pageSize)
	if page > totalPages {
		page = totalPages
	}
	offset := (page - 1) * pageSize
	queryArgs := append(append([]any{}, args...), pageSize, offset)
	rows, err := a.db.Query("SELECT id, ip, action, target, detail, created_at FROM operation_logs "+where+" ORDER BY id DESC LIMIT ? OFFSET ?", queryArgs...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取操作日志失败"})
		return
	}
	defer rows.Close()
	items := make([]operationLogItem, 0, pageSize)
	for rows.Next() {
		var item operationLogItem
		if err := rows.Scan(&item.ID, &item.IP, &item.Action, &item.Target, &item.Detail, &item.CreatedAt); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "解析操作日志失败"})
			return
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":       items,
		"total":       total,
		"page":        page,
		"page_size":   pageSize,
		"total_pages": totalPages,
		"has_prev":    page > 1,
		"has_next":    page < totalPages,
		"keyword":     keyword,
	})
}

func (a *app) getNotice() (noticeItem, error) {
	var item noticeItem
	var enabled int
	err := a.db.QueryRow(`SELECT notice, notice_enabled, updated_at FROM settings WHERE id = 1`).Scan(&item.Content, &enabled, &item.UpdatedAt)
	item.Enabled = enabled == 1
	return item, err
}

func (a *app) buildOverview(r *http.Request) (overviewItem, error) {
	var out overviewItem

	cfg, err := a.loadStorageConfig()
	if err != nil {
		return out, err
	}
	ai, err := a.loadAIConfig()
	if err != nil {
		return out, err
	}
	notice, err := a.getNotice()
	if err != nil {
		return out, err
	}
	themes, err := listThemes()
	if err != nil {
		return out, err
	}

	out.GeneratedAt = time.Now().Format("2006-01-02 15:04:05")
	out.Storage = cfg
	out.AI = ai
	out.Themes = overviewThemeStats{
		Active: a.currentTheme(),
		Items:  themes,
		Count:  len(themes),
	}
	out.Notice = overviewNoticeStats{
		Content:    notice.Content,
		Enabled:    notice.Enabled,
		UpdatedAt:  notice.UpdatedAt,
		HasContent: strings.TrimSpace(notice.Content) != "",
	}

	if err := a.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM images`).Scan(&out.Images.Total, &out.Images.TotalSize); err != nil {
		return out, err
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM image_views`).Scan(&out.Images.TotalViews); err != nil {
		return out, err
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM homepage_views`).Scan(&out.Images.HomepageTotalViews); err != nil {
		return out, err
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM images WHERE date(created_at, 'localtime') = date('now', 'localtime')`).Scan(&out.Images.TodayTotal); err != nil {
		return out, err
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM image_views WHERE date(created_at, 'localtime') = date('now', 'localtime')`).Scan(&out.Images.TodayViews); err != nil {
		return out, err
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM homepage_views WHERE date(created_at, 'localtime') = date('now', 'localtime')`).Scan(&out.Images.HomepageTodayViews); err != nil {
		return out, err
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM images WHERE date(created_at, 'localtime') >= date('now', 'localtime', '-6 day')`).Scan(&out.Images.Last7dTotal); err != nil {
		return out, err
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM image_views WHERE date(created_at, 'localtime') >= date('now', 'localtime', '-6 day')`).Scan(&out.Images.Last7dViews); err != nil {
		return out, err
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM homepage_views WHERE date(created_at, 'localtime') >= date('now', 'localtime', '-6 day')`).Scan(&out.Images.HomepageLast7dViews); err != nil {
		return out, err
	}

	out.Images.DailyBuckets = make([]overviewDailyBucket, 0, 7)
	bucketMap := make(map[string]int64, 7)
	rows, err := a.db.Query(`
		SELECT date(created_at, 'localtime') AS day, COUNT(*)
		FROM images
		WHERE date(created_at, 'localtime') >= date('now', 'localtime', '-6 day')
		GROUP BY day
		ORDER BY day ASC
	`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var bucket overviewDailyBucket
		if err := rows.Scan(&bucket.Date, &bucket.Count); err != nil {
			return out, err
		}
		bucketMap[bucket.Date] = bucket.Count
	}
	for i := 6; i >= 0; i-- {
		day := time.Now().AddDate(0, 0, -i).Format("2006-01-02")
		out.Images.DailyBuckets = append(out.Images.DailyBuckets, overviewDailyBucket{
			Date:  day,
			Count: bucketMap[day],
		})
	}

	var latest overviewImageItem
	var latestPath, storageType, rawStorageConfig string
	err = a.db.QueryRow(`SELECT id, name, path, size, storage_type, storage_config, created_at FROM images ORDER BY id DESC LIMIT 1`).Scan(&latest.ID, &latest.Name, &latestPath, &latest.Size, &storageType, &rawStorageConfig, &latest.CreatedAt)
	switch err {
	case nil:
		latest.URL = imagePublicURL(r, storageType, rawStorageConfig, latestPath)
		out.Images.Latest = &latest
	case sql.ErrNoRows:
		out.Images.Latest = nil
	default:
		return out, err
	}

	return out, nil
}

func (a *app) loadStorageConfig() (storageConfig, error) {
	var cfg storageConfig
	var storageType, raw string
	err := a.db.QueryRow(`SELECT storage_type, storage_config FROM settings WHERE id = 1`).Scan(&storageType, &raw)
	if err != nil {
		return cfg, err
	}
	cfg = parseStorageConfig(raw)
	if strings.TrimSpace(cfg.Type) == "" {
		cfg.Type = storageType
	}
	return normalizeStorageConfig(cfg), nil
}

func (a *app) loadAIConfig() (aiConfig, error) {
	var cfg aiConfig
	err := a.db.QueryRow(`SELECT ai_base_url, ai_wire_api, ai_model, ai_key, ai_tag_prompt, ai_review_prompt FROM settings WHERE id = 1`).Scan(&cfg.BaseURL, &cfg.WireAPI, &cfg.Model, &cfg.Key, &cfg.TagPrompt, &cfg.ReviewPrompt)
	if err != nil {
		return cfg, err
	}
	return normalizeAIConfig(cfg), nil
}

func (a *app) loadNotifyConfig() (notifyConfig, error) {
	var cfg notifyConfig
	var raw string
	err := a.db.QueryRow(`SELECT notify_config FROM settings WHERE id = 1`).Scan(&raw)
	if err != nil {
		return cfg, err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return normalizeNotifyConfig(notifyConfig{
			Email: notifyEmailConfig{
				Port: 587,
			},
		}), nil
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return notifyConfig{
			Email: notifyEmailConfig{
				Port: 587,
			},
		}, nil
	}
	return normalizeNotifyConfig(cfg), nil
}

func (a *app) loadGuestUploadConfig() (guestUploadConfig, error) {
	var cfg guestUploadConfig
	var raw string
	err := a.db.QueryRow(`SELECT guest_upload_config FROM settings WHERE id = 1`).Scan(&raw)
	if err != nil {
		return cfg, err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return normalizeGuestUploadConfig(guestUploadConfig{Enabled: true}), nil
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return normalizeGuestUploadConfig(guestUploadConfig{Enabled: true}), nil
	}
	return normalizeGuestUploadConfig(cfg), nil
}

func (a *app) loadSecurityConfig() (securityConfig, error) {
	var cfg securityConfig
	var raw string
	err := a.db.QueryRow(`SELECT security_config FROM settings WHERE id = 1`).Scan(&raw)
	if err != nil {
		return cfg, err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultSecurityConfig(), nil
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return defaultSecurityConfig(), nil
	}
	return normalizeSecurityConfig(cfg), nil
}

func (a *app) loadSiteConfig() (siteConfig, error) {
	var cfg siteConfig
	err := a.db.QueryRow(`SELECT home_tagline, browser_title FROM settings WHERE id = 1`).Scan(&cfg.HomeTagline, &cfg.BrowserTitle)
	if err != nil {
		return cfg, err
	}
	return normalizeSiteConfig(cfg), nil
}

func (a *app) saveSiteConfig(cfg siteConfig) error {
	cfg = normalizeSiteConfig(cfg)
	_, err := a.db.Exec(`UPDATE settings SET home_tagline = ?, browser_title = ?, updated_at = CURRENT_TIMESTAMP WHERE id = 1`, cfg.HomeTagline, cfg.BrowserTitle)
	return err
}

func (a *app) loadAdminPassword() (string, error) {
	var password string
	err := a.db.QueryRow(`SELECT admin_password FROM settings WHERE id = 1`).Scan(&password)
	return strings.TrimSpace(password), err
}

func (a *app) saveAdminPassword(password string) error {
	_, err := a.db.Exec(`UPDATE settings SET admin_password = ?, updated_at = CURRENT_TIMESTAMP WHERE id = 1`, strings.TrimSpace(password))
	return err
}

func ensureColumn(db *sql.DB, table, column, definition string) error {
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	_, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition))
	return err
}

func cleanupLegacyDailyNotifyTasks(db *sql.DB) error {
	var rawConfig, rawState string
	if err := db.QueryRow(`SELECT notify_config, notify_state FROM settings WHERE id = 1`).Scan(&rawConfig, &rawState); err != nil {
		return err
	}

	if updatedConfig, changed := removeNestedJSONKeys(rawConfig, "events", "daily_upload_summary", "daily_view_summary"); changed {
		if _, err := db.Exec(`UPDATE settings SET notify_config = ?, updated_at = CURRENT_TIMESTAMP WHERE id = 1`, updatedConfig); err != nil {
			return err
		}
	}
	if updatedState, changed := removeJSONKeys(rawState, "daily_upload_summary_date", "daily_view_summary_date"); changed {
		if _, err := db.Exec(`UPDATE settings SET notify_state = ?, updated_at = CURRENT_TIMESTAMP WHERE id = 1`, updatedState); err != nil {
			return err
		}
	}
	return nil
}

func removeNestedJSONKeys(raw, parent string, keys ...string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw, false
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return raw, false
	}
	nested, ok := payload[parent].(map[string]any)
	if !ok {
		return raw, false
	}
	changed := false
	for _, key := range keys {
		if _, exists := nested[key]; exists {
			delete(nested, key)
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return raw, false
	}
	return string(data), true
}

func removeJSONKeys(raw string, keys ...string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw, false
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return raw, false
	}
	changed := false
	for _, key := range keys {
		if _, exists := payload[key]; exists {
			delete(payload, key)
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return raw, false
	}
	return string(data), true
}

func (a *app) saveStorageConfig(cfg storageConfig) error {
	cfg = normalizeStorageConfig(cfg)
	_, err := a.db.Exec(`UPDATE settings SET storage_type = ?, storage_config = ?, updated_at = CURRENT_TIMESTAMP WHERE id = 1`, cfg.Type, marshalStorageConfig(cfg))
	return err
}

func (a *app) saveAIConfig(cfg aiConfig) error {
	cfg = normalizeAIConfig(cfg)
	_, err := a.db.Exec(`UPDATE settings SET ai_base_url = ?, ai_wire_api = ?, ai_model = ?, ai_key = ?, ai_tag_prompt = ?, ai_review_prompt = ?, updated_at = CURRENT_TIMESTAMP WHERE id = 1`, cfg.BaseURL, cfg.WireAPI, cfg.Model, cfg.Key, cfg.TagPrompt, cfg.ReviewPrompt)
	return err
}

func (a *app) saveNotifyConfig(cfg notifyConfig) error {
	cfg = normalizeNotifyConfig(cfg)
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = a.db.Exec(`UPDATE settings SET notify_config = ?, updated_at = CURRENT_TIMESTAMP WHERE id = 1`, string(data))
	return err
}

func (a *app) saveGuestUploadConfig(cfg guestUploadConfig) error {
	cfg = normalizeGuestUploadConfig(cfg)
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = a.db.Exec(`UPDATE settings SET guest_upload_config = ?, updated_at = CURRENT_TIMESTAMP WHERE id = 1`, string(data))
	return err
}

func (a *app) saveSecurityConfig(cfg securityConfig) error {
	cfg = normalizeSecurityConfig(cfg)
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = a.db.Exec(`UPDATE settings SET security_config = ?, updated_at = CURRENT_TIMESTAMP WHERE id = 1`, string(data))
	return err
}

func (a *app) notifyUploadEvents(name, publicURL string, size int64, width, height int, uploadIP string, sensitive bool, sensitiveReason string, tags []string) {
	cfg, err := a.loadNotifyConfig()
	if err != nil {
		log.Printf("读取通知配置失败: %v", err)
		return
	}
	if !cfg.Enabled {
		return
	}
	tagText := "无"
	if len(tags) > 0 {
		tagText = strings.Join(tags, ", ")
	}
	baseDetail := fmt.Sprintf(
		"文件: %s\n大小: %s\n尺寸: %dx%d\n上传IP: %s\n标签: %s\n链接: %s",
		name,
		formatBytesSimple(size),
		width,
		height,
		uploadIP,
		tagText,
		publicURL,
	)
	if cfg.Events.NewUpload {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		result := a.sendNotification(ctx, "Himg 新图片上传通知", "检测到新图片上传。\n"+baseDetail, cfg)
		cancel()
		logNotificationResult("new_upload", result)
	}
	if cfg.Events.SensitiveReview && sensitive {
		reason := strings.TrimSpace(sensitiveReason)
		if reason == "" {
			reason = "命中敏感规则"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		result := a.sendNotification(ctx, "Himg 敏感图片审核通知", "检测到疑似敏感图片，已标记为隐藏。\n原因: "+reason+"\n"+baseDetail, cfg)
		cancel()
		logNotificationResult("sensitive_review", result)
	}
}

func logNotificationResult(scene string, results map[string]string) {
	for channel, result := range results {
		if strings.HasPrefix(result, "error:") {
			log.Printf("通知发送失败 scene=%s channel=%s err=%s", scene, channel, strings.TrimPrefix(result, "error:"))
		}
	}
}

func (a *app) recordHomepageView(ip string) {
	if _, err := a.db.Exec(`INSERT INTO homepage_views(view_ip) VALUES (?)`, ip); err != nil {
		log.Printf("记录首页访问日志失败: %v", err)
	}
}

func (a *app) sendNotification(ctx context.Context, title, content string, cfg notifyConfig) map[string]string {
	result := map[string]string{
		"server_chan": "skip",
		"email":       "skip",
		"telegram":    "skip",
	}
	cfg = normalizeNotifyConfig(cfg)
	if !cfg.Enabled {
		return result
	}

	if cfg.Server.Enabled {
		if err := sendServerChan(ctx, title, content, cfg.Server); err != nil {
			result["server_chan"] = "error: " + err.Error()
		} else {
			result["server_chan"] = "ok"
		}
	}
	if cfg.Email.Enabled {
		if err := sendEmail(ctx, title, content, cfg.Email); err != nil {
			result["email"] = "error: " + err.Error()
		} else {
			result["email"] = "ok"
		}
	}
	if cfg.Telegram.Enabled {
		if err := sendTelegram(ctx, title, content, cfg.Telegram); err != nil {
			result["telegram"] = "error: " + err.Error()
		} else {
			result["telegram"] = "ok"
		}
	}
	return result
}

func (a *app) checkGuestUploadAccess(ip string) (guestUploadConfig, error) {
	cfg, err := a.loadGuestUploadConfig()
	if err != nil {
		log.Printf("读取游客上传配置失败: %v", err)
		return normalizeGuestUploadConfig(guestUploadConfig{Enabled: true}), nil
	}
	if !cfg.Enabled {
		return cfg, fmt.Errorf("游客上传已关闭")
	}
	if matchIPRule(ip, cfg.BlockedIPs) {
		return cfg, fmt.Errorf("当前 IP 已被禁止访问")
	}
	if cfg.MaxUploadsPerMinute <= 0 {
		return cfg, nil
	}
	var count int
	err = a.db.QueryRow(
		`SELECT COUNT(*) FROM images WHERE upload_ip = ? AND created_at >= datetime('now', '-1 minute')`,
		ip,
	).Scan(&count)
	if err != nil {
		return cfg, nil
	}
	if count >= cfg.MaxUploadsPerMinute {
		return cfg, fmt.Errorf("上传过于频繁，请稍后再试")
	}
	return cfg, nil
}

func (a *app) checkGuestViewAccess(ip string) error {
	cfg, err := a.loadGuestUploadConfig()
	if err != nil {
		log.Printf("读取游客上传配置失败: %v", err)
		return nil
	}
	if matchIPRule(ip, cfg.BlockedIPs) {
		return fmt.Errorf("当前 IP 已被禁止访问")
	}
	return nil
}

func sendServerChan(ctx context.Context, title, content string, cfg notifyServerChanConfig) error {
	sendKey := strings.TrimSpace(cfg.SendKey)
	if sendKey == "" {
		return fmt.Errorf("未填写 Server酱 SendKey")
	}
	form := url.Values{
		"text":  {title},
		"title": {title},
		"desp":  {content},
	}
	endpoint := fmt.Sprintf("https://sctapi.ftqq.com/%s.send", sendKey)
	if strings.HasPrefix(strings.ToUpper(sendKey), "SCK") {
		endpoint = fmt.Sprintf("https://sc.ftqq.com/%s.send", sendKey)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Code int    `json:"code"`
		Msg  string `json:"message"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.Code != 0 {
		return fmt.Errorf("code=%d msg=%s", payload.Code, payload.Msg)
	}
	return nil
}

func sendEmail(ctx context.Context, title, content string, cfg notifyEmailConfig) error {
	host := strings.TrimSpace(cfg.Host)
	from := strings.TrimSpace(cfg.From)
	to := parseEmailRecipients(cfg.To)
	if host == "" || from == "" || len(to) == 0 {
		return fmt.Errorf("邮箱配置不完整")
	}
	port := cfg.Port
	if port <= 0 || port > 65535 {
		port = 587
	}
	addr := fmt.Sprintf("%s:%d", host, port)

	subject := mime.QEncoding.Encode("UTF-8", title)
	message := strings.Join([]string{
		fmt.Sprintf("From: %s", from),
		fmt.Sprintf("To: %s", strings.Join(to, ", ")),
		fmt.Sprintf("Subject: %s", subject),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"",
		content,
	}, "\r\n")

	var auth smtp.Auth
	if strings.TrimSpace(cfg.Username) != "" {
		auth = smtp.PlainAuth("", cfg.Username, cfg.Password, host)
	}

	dialer := &net.Dialer{Timeout: 8 * time.Second}
	if cfg.UseSSL || port == 465 {
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: host})
		if err != nil {
			return err
		}
		return sendSMTPMessage(ctx, conn, host, auth, from, to, message)
	}

	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	if port == 587 || auth != nil {
		return sendSMTPMessage(ctx, conn, host, auth, from, to, message)
	}
	return sendSMTPMessage(ctx, conn, host, nil, from, to, message)
}

func sendSMTPMessage(ctx context.Context, conn net.Conn, host string, auth smtp.Auth, from string, to []string, message string) error {
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	}

	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer client.Close()

	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return err
		}
	} else if auth != nil {
		if tlsConn, ok := conn.(*tls.Conn); !ok || tlsConn.ConnectionState().HandshakeComplete == false {
			return fmt.Errorf("SMTP 服务器不支持 STARTTLS，无法安全认证")
		}
	}

	if auth != nil {
		if err := client.Auth(auth); err != nil {
			return err
		}
	}
	if err := client.Mail(from); err != nil {
		return err
	}
	for _, recipient := range to {
		if err := client.Rcpt(recipient); err != nil {
			return err
		}
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(message)); err != nil {
		_ = w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return client.Quit()
}

func sendTelegram(ctx context.Context, title, content string, cfg notifyTelegramConfig) error {
	token := strings.TrimSpace(cfg.BotToken)
	chatID := strings.TrimSpace(cfg.ChatID)
	if token == "" || chatID == "" {
		return fmt.Errorf("Telegram 配置不完整")
	}
	endpoint := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)
	payload := map[string]any{
		"chat_id":                  chatID,
		"text":                     title + "\n\n" + content,
		"disable_web_page_preview": true,
	}
	data, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(body, &result); err == nil && !result.OK {
		return fmt.Errorf(result.Description)
	}
	return nil
}

func parseEmailRecipients(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '，' || r == ';' || r == '；' || r == '\n' || r == '\r' || r == '\t'
	})
	out := make([]string, 0, len(fields))
	for _, item := range fields {
		email := strings.TrimSpace(item)
		if email == "" {
			continue
		}
		out = append(out, email)
	}
	return out
}

func formatBytesSimple(bytes int64) string {
	value := float64(bytes)
	if value <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	index := 0
	for value >= 1024 && index < len(units)-1 {
		value /= 1024
		index++
	}
	if index == 0 {
		return fmt.Sprintf("%.0f %s", value, units[index])
	}
	if value >= 100 {
		return fmt.Sprintf("%.0f %s", value, units[index])
	}
	if value >= 10 {
		return fmt.Sprintf("%.1f %s", value, units[index])
	}
	return fmt.Sprintf("%.2f %s", value, units[index])
}

func parseTags(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []string{}
	}
	var tags []string
	if err := json.Unmarshal([]byte(raw), &tags); err != nil {
		parts := strings.FieldsFunc(raw, func(r rune) bool {
			return r == ',' || r == '，' || r == '|' || r == '\n' || r == '\r' || r == '\t'
		})
		return sanitizeTags(parts)
	}
	return sanitizeTags(tags)
}

func marshalTags(tags []string) string {
	clean := sanitizeTags(tags)
	if len(clean) == 0 {
		return "[]"
	}
	data, err := json.Marshal(clean)
	if err != nil {
		return "[]"
	}
	return string(data)
}

func sanitizeTags(tags []string) []string {
	out := make([]string, 0, 12)
	seen := make(map[string]struct{}, 12)
	for _, tag := range tags {
		item := strings.TrimSpace(tag)
		if item == "" {
			continue
		}
		if len([]rune(item)) > 24 {
			continue
		}
		key := strings.ToLower(item)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item)
		if len(out) >= 12 {
			break
		}
	}
	return out
}

func containsKeyword(text string, keywords []string) bool {
	content := strings.ToLower(strings.TrimSpace(text))
	if content == "" {
		return false
	}
	for _, keyword := range keywords {
		word := strings.ToLower(strings.TrimSpace(keyword))
		if word != "" && strings.Contains(content, word) {
			return true
		}
	}
	return false
}

func shouldAutoHideByPolicy(filename string, tags []string) (bool, string) {
	politicalKeywords := []string{
		"政治", "政党", "政府", "官员", "国家领导人", "领导人", "政治人物", "公众人物", "国家主席", "总书记", "总理", "总统一", "总统", "副总统",
		"人大", "政协", "党代会", "两会", "选举", "示威", "游行", "抗议", "维权", "敏感人物", "意识形态", "国旗焚烧", "反政府", "政治宣传",
		"习近平", "李强", "蔡奇", "丁薛祥", "李希", "王沪宁", "赵乐际", "韩正", "胡锦涛", "江泽民",
		"毛泽东", "邓小平", "蒋介石", "拜登", "特朗普", "普京", "泽连斯基", "金正恩",
	}
	pornKeywords := []string{
		"色情", "裸露", "裸体", "成人视频", "性暗示", "性行为", "私密部位", "乳房", "阴部", "胸部特写",
		"成人用品", "情趣内衣", "挑逗姿势", "成人视频截图", "露点", "porn", "nsfw", "adult",
	}

	joinedTags := strings.Join(tags, " ")
	if containsKeyword(joinedTags, pornKeywords) || containsKeyword(filename, pornKeywords) {
		return true, "色情内容默认隐藏"
	}
	if containsKeyword(joinedTags, politicalKeywords) || containsKeyword(filename, politicalKeywords) {
		return true, "政治敏感内容默认隐藏"
	}
	return false, ""
}

func (a *app) shouldAutoHideByAI(ctx context.Context, imageData []byte, filename string, tags []string) (bool, string, error) {
	cfg, err := a.loadAIConfig()
	if err != nil {
		return false, "", err
	}
	if strings.TrimSpace(cfg.BaseURL) == "" || strings.TrimSpace(cfg.Model) == "" || strings.TrimSpace(cfg.Key) == "" {
		return false, "", nil
	}

	checkCtx, cancel := context.WithTimeout(ctx, 16*time.Second)
	defer cancel()

	dataURL := "data:image/webp;base64," + base64.StdEncoding.EncodeToString(imageData)
	instruction := aiReviewPrompt(cfg)
	userMeta := fmt.Sprintf("文件名: %s\n标签: %s", filename, strings.Join(tags, ","))

	apiPath := "responses"
	var body map[string]any
	if cfg.WireAPI == "chat/completions" {
		apiPath = "chat/completions"
		body = map[string]any{
			"model": cfg.Model,
			"messages": []map[string]any{
				{
					"role":    "system",
					"content": instruction,
				},
				{
					"role": "user",
					"content": []map[string]any{
						{"type": "text", "text": userMeta},
						{"type": "image_url", "image_url": map[string]string{"url": dataURL}},
					},
				},
			},
			"temperature": 0.0,
			"max_tokens":  120,
		}
	} else {
		body = map[string]any{
			"model": cfg.Model,
			"input": []map[string]any{
				{
					"role": "system",
					"content": []map[string]any{
						{"type": "input_text", "text": instruction},
					},
				},
				{
					"role": "user",
					"content": []map[string]any{
						{"type": "input_text", "text": userMeta},
						{"type": "input_image", "image_url": dataURL},
					},
				},
			},
			"temperature": 0.0,
		}
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return false, "", err
	}

	req, err := http.NewRequestWithContext(checkCtx, http.MethodPost, aiJoinURL(cfg.BaseURL, apiPath), bytes.NewReader(buf))
	if err != nil {
		return false, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Key)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, "", fmt.Errorf("AI 审核请求失败: %d", resp.StatusCode)
	}

	var text string
	if apiPath == "chat/completions" {
		text = parseAITextFromChatResponse(data)
	} else {
		text = parseAITextFromResponsesResponse(data)
	}
	text = cleanAIJSONText(text)
	if strings.TrimSpace(text) == "" {
		return false, "", nil
	}

	var result struct {
		Political bool   `json:"political"`
		Porn      bool   `json:"porn"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		return false, "", nil
	}
	if result.Porn {
		reason := strings.TrimSpace(result.Reason)
		if reason == "" {
			reason = "AI 审核判定为色情风险"
		}
		return true, reason, nil
	}
	if result.Political {
		reason := strings.TrimSpace(result.Reason)
		if reason == "" {
			reason = "AI 审核判定为政治人物或政治敏感内容"
		}
		return true, reason, nil
	}
	return false, "", nil
}

func textVector(text string, dim int) []float64 {
	if dim <= 0 {
		dim = 256
	}
	vec := make([]float64, dim)
	runes := []rune(strings.ToLower(strings.TrimSpace(text)))
	if len(runes) == 0 {
		return vec
	}

	addToken := func(token string, weight float64) {
		if strings.TrimSpace(token) == "" {
			return
		}
		h := fnv.New32a()
		_, _ = h.Write([]byte(token))
		idx := int(h.Sum32() % uint32(dim))
		vec[idx] += weight
	}

	for i := 0; i < len(runes); i++ {
		addToken(string(runes[i]), 1.0)
		if i+1 < len(runes) {
			addToken(string(runes[i:i+2]), 1.8)
		}
		if i+2 < len(runes) {
			addToken(string(runes[i:i+3]), 2.2)
		}
	}

	var norm float64
	for _, v := range vec {
		norm += v * v
	}
	if norm <= 0 {
		return vec
	}
	norm = math.Sqrt(norm)
	for i := range vec {
		vec[i] /= norm
	}
	return vec
}

func cosineSimilarity(a, b []float64) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot float64
	for i := range a {
		dot += a[i] * b[i]
	}
	return dot
}

func clampScore(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func lexicalMatchScore(corpus string, terms []string) float64 {
	seen := make(map[string]struct{}, len(terms))
	score := 0.0
	for _, term := range terms {
		t := strings.ToLower(strings.TrimSpace(term))
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		if strings.Contains(corpus, t) {
			score += 0.12
		}
	}
	return clampScore(score)
}

func limitRunes(input string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(strings.TrimSpace(input))
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max])
}

func cleanAIJSONText(text string) string {
	s := strings.TrimSpace(text)
	if s == "" {
		return s
	}
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	if idx := strings.IndexAny(s, "{["); idx > 0 {
		s = s[idx:]
	}
	return strings.TrimSpace(s)
}

func parseAITextFromChatResponse(data []byte) string {
	var payload struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || len(payload.Choices) == 0 {
		return ""
	}
	content := payload.Choices[0].Message.Content
	switch v := content.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		parts := make([]string, 0, len(v))
		for _, block := range v {
			item, ok := block.(map[string]any)
			if !ok {
				continue
			}
			text, _ := item["text"].(string)
			if strings.TrimSpace(text) != "" {
				parts = append(parts, text)
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	default:
		return ""
	}
}

func parseAITextFromResponsesResponse(data []byte) string {
	var payload struct {
		OutputText string `json:"output_text"`
		Output     []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return ""
	}
	if strings.TrimSpace(payload.OutputText) != "" {
		return strings.TrimSpace(payload.OutputText)
	}
	parts := make([]string, 0, 8)
	for _, item := range payload.Output {
		for _, block := range item.Content {
			if (block.Type == "output_text" || block.Type == "text") && strings.TrimSpace(block.Text) != "" {
				parts = append(parts, block.Text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func (a *app) generateAIText(ctx context.Context, instruction, prompt string, maxOutTokens int) (string, error) {
	cfg, err := a.loadAIConfig()
	if err != nil {
		return "", err
	}
	if cfg.BaseURL == "" || cfg.Model == "" || cfg.Key == "" {
		return "", nil
	}
	if maxOutTokens <= 0 {
		maxOutTokens = 300
	}

	apiPath := "responses"
	var body map[string]any
	if cfg.WireAPI == "chat/completions" {
		apiPath = "chat/completions"
		body = map[string]any{
			"model": cfg.Model,
			"messages": []map[string]any{
				{"role": "system", "content": instruction},
				{"role": "user", "content": prompt},
			},
			"temperature": 0.1,
			"max_tokens":  maxOutTokens,
		}
	} else {
		body = map[string]any{
			"model": cfg.Model,
			"input": []map[string]any{
				{
					"role": "system",
					"content": []map[string]any{
						{"type": "input_text", "text": instruction},
					},
				},
				{
					"role": "user",
					"content": []map[string]any{
						{"type": "input_text", "text": prompt},
					},
				},
			},
			"temperature": 0.1,
		}
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, aiJoinURL(cfg.BaseURL, apiPath), bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Key)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("AI 接口请求失败: %d", resp.StatusCode)
	}
	if apiPath == "chat/completions" {
		return parseAITextFromChatResponse(data), nil
	}
	return parseAITextFromResponsesResponse(data), nil
}

func (a *app) expandSearchTerms(ctx context.Context, query string) []string {
	base := strings.TrimSpace(query)
	if base == "" {
		return []string{}
	}
	terms := sanitizeTags([]string{base})
	instruction := "你是搜索词扩展助手。请输出 JSON 字符串数组，只返回 3-10 个与查询强相关的中文关键词和同义词，不要解释。"
	prompt := fmt.Sprintf("查询词：%s", base)
	text, err := a.generateAIText(ctx, instruction, prompt, 180)
	if err != nil || strings.TrimSpace(text) == "" {
		return terms
	}
	expanded := parseTags(cleanAIJSONText(text))
	if len(expanded) == 0 {
		return terms
	}
	return sanitizeTags(append([]string{base}, expanded...))
}

func (a *app) aiRerankSearch(ctx context.Context, query string, candidates []imageItem) map[int64]float64 {
	if len(candidates) == 0 {
		return map[int64]float64{}
	}
	type row struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Tags string `json:"tags"`
	}
	rows := make([]row, 0, len(candidates))
	for _, item := range candidates {
		rows = append(rows, row{
			ID:   item.ID,
			Name: limitRunes(item.Name, 60),
			Tags: limitRunes(strings.Join(item.Tags, ","), 80),
		})
	}
	rowsJSON, _ := json.Marshal(rows)
	instruction := "你是图片搜索重排助手。根据查询词给候选图片打相关性分数。严格返回 JSON 对象：{\"items\":[{\"id\":数字,\"score\":0到1小数}]}，不要输出其它内容。"
	prompt := fmt.Sprintf("查询：%s\n候选：%s", query, string(rowsJSON))
	text, err := a.generateAIText(ctx, instruction, prompt, 360)
	if err != nil || strings.TrimSpace(text) == "" {
		return map[int64]float64{}
	}
	text = cleanAIJSONText(text)

	out := make(map[int64]float64, len(candidates))
	var parsed struct {
		Items []struct {
			ID    int64   `json:"id"`
			Score float64 `json:"score"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(text), &parsed); err == nil && len(parsed.Items) > 0 {
		for _, item := range parsed.Items {
			if item.ID > 0 {
				out[item.ID] = clampScore(item.Score)
			}
		}
		return out
	}
	var obj map[string]float64
	if err := json.Unmarshal([]byte(text), &obj); err == nil {
		for k, v := range obj {
			id, convErr := strconv.ParseInt(strings.TrimSpace(k), 10, 64)
			if convErr == nil && id > 0 {
				out[id] = clampScore(v)
			}
		}
	}
	return out
}

func aiJoinURL(base, path string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	path = strings.TrimLeft(strings.TrimSpace(path), "/")
	if base == "" {
		return "/" + path
	}
	return base + "/" + path
}

func extractTagsFromChatResponse(data []byte) []string {
	var payload struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return []string{}
	}
	if len(payload.Choices) == 0 {
		return []string{}
	}
	content := payload.Choices[0].Message.Content
	switch v := content.(type) {
	case string:
		return parseTags(v)
	case []any:
		parts := make([]string, 0, len(v))
		for _, block := range v {
			item, ok := block.(map[string]any)
			if !ok {
				continue
			}
			text, _ := item["text"].(string)
			if strings.TrimSpace(text) != "" {
				parts = append(parts, text)
			}
		}
		return parseTags(strings.Join(parts, ","))
	default:
		return []string{}
	}
}

func extractTagsFromResponsesResponse(data []byte) []string {
	var payload struct {
		OutputText string `json:"output_text"`
		Output     []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return []string{}
	}
	if strings.TrimSpace(payload.OutputText) != "" {
		return parseTags(payload.OutputText)
	}
	parts := make([]string, 0, 8)
	for _, item := range payload.Output {
		for _, block := range item.Content {
			if block.Type == "output_text" || block.Type == "text" {
				if strings.TrimSpace(block.Text) != "" {
					parts = append(parts, block.Text)
				}
			}
		}
	}
	return parseTags(strings.Join(parts, ","))
}

func (a *app) generateImageTags(ctx context.Context, imageData []byte) ([]string, error) {
	cfg, err := a.loadAIConfig()
	if err != nil {
		return nil, err
	}
	if cfg.BaseURL == "" || cfg.Model == "" || cfg.Key == "" {
		return []string{}, nil
	}

	tagCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	dataURL := "data:image/webp;base64," + base64.StdEncoding.EncodeToString(imageData)
	instruction := aiTagPrompt(cfg)
	userPrompt := "请为这张图生成精准标签，标签尽量具体。"
	apiPath := "responses"
	var body map[string]any
	if cfg.WireAPI == "chat/completions" {
		apiPath = "chat/completions"
		body = map[string]any{
			"model": cfg.Model,
			"messages": []map[string]any{
				{
					"role":    "system",
					"content": instruction,
				},
				{
					"role": "user",
					"content": []map[string]any{
						{
							"type": "text",
							"text": userPrompt,
						},
						{
							"type": "image_url",
							"image_url": map[string]string{
								"url": dataURL,
							},
						},
					},
				},
			},
			"temperature": 0.2,
			"max_tokens":  200,
		}
	} else {
		body = map[string]any{
			"model": cfg.Model,
			"input": []map[string]any{
				{
					"role": "system",
					"content": []map[string]any{
						{
							"type": "input_text",
							"text": instruction,
						},
					},
				},
				{
					"role": "user",
					"content": []map[string]any{
						{
							"type": "input_text",
							"text": userPrompt,
						},
						{
							"type":      "input_image",
							"image_url": dataURL,
						},
					},
				},
			},
			"temperature": 0.2,
		}
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(tagCtx, http.MethodPost, aiJoinURL(cfg.BaseURL, apiPath), bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Key)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("AI 接口请求失败: %d", resp.StatusCode)
	}
	if apiPath == "chat/completions" {
		return sanitizeTags(extractTagsFromChatResponse(data)), nil
	}
	return sanitizeTags(extractTagsFromResponsesResponse(data)), nil
}

func (a *app) currentPassword() string {
	a.passwordMu.RLock()
	defer a.passwordMu.RUnlock()
	return a.password
}

func (a *app) setPassword(password string) {
	a.passwordMu.Lock()
	defer a.passwordMu.Unlock()
	a.password = password
}

func isImage(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp":
		return true
	default:
		return false
	}
}

func (a *app) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.isAuthed(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "请先登录"})
			return
		}
		next(w, r)
	}
}

func (a *app) currentStorage() storageBackend {
	a.storageMu.RLock()
	defer a.storageMu.RUnlock()
	return a.storage
}

func (a *app) setStorage(storage storageBackend) {
	a.storageMu.Lock()
	defer a.storageMu.Unlock()
	a.storage = storage
}

func (a *app) currentSecurityConfig() securityConfig {
	a.securityMu.RLock()
	defer a.securityMu.RUnlock()
	return a.security
}

func (a *app) setSecurityConfig(cfg securityConfig) {
	a.securityMu.Lock()
	defer a.securityMu.Unlock()
	a.security = normalizeSecurityConfig(cfg)
}

func normalizeImageStorageType(storageType string) string {
	switch strings.ToLower(strings.TrimSpace(storageType)) {
	case "webdav":
		return "webdav"
	case "s3":
		return "s3"
	default:
		return "local"
	}
}

func storageTypeLabel(storageType string) string {
	switch normalizeImageStorageType(storageType) {
	case "webdav":
		return "WebDAV 存储"
	case "s3":
		return "S3 存储"
	default:
		return "本地存储"
	}
}

func imageStorageBackend(storageType, rawConfig string) (storageBackend, error) {
	cfg := parseStorageConfig(rawConfig)
	cfg.Type = normalizeImageStorageType(storageType)
	return newStorageFromConfig(cfg)
}

func imagePublicURL(r *http.Request, storageType, rawConfig, key string) string {
	cfg := parseStorageConfig(rawConfig)
	cfg.Type = normalizeImageStorageType(storageType)
	return appUploadURL(r, cfg.PublicBaseURL, key)
}

func (a *app) serveUpload(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/uploads/")
	if key == "" {
		http.NotFound(w, r)
		return
	}
	if err := a.checkGuestViewAccess(clientIP(r)); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	var hidden int
	var storageType, rawStorageConfig string
	err := a.db.QueryRow(`SELECT hidden, storage_type, storage_config FROM images WHERE path = ? LIMIT 1`, key).Scan(&hidden, &storageType, &rawStorageConfig)
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取图片状态失败"})
		return
	}
	if hidden == 1 {
		a.serveBanImage(w, r)
		return
	}
	storage, err := imageStorageBackend(storageType, rawStorageConfig)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "解析图片存储失败"})
		return
	}
	if !isAdminReferrer(r) {
		if _, err = a.db.Exec(`UPDATE images SET view_count = view_count + 1 WHERE path = ?`, key); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "更新访问量失败"})
			return
		}
		viewIP := clientIP(r)
		if _, err = a.db.Exec(`INSERT INTO image_views(image_path, view_ip) VALUES (?, ?)`, key, viewIP); err != nil {
			log.Printf("记录访问日志失败: %v", err)
		}
	}
	applyUploadNoCacheHeaders(w)
	if fs := storage.FileServer(); fs != nil {
		http.StripPrefix("/uploads/", fs).ServeHTTP(w, r)
		return
	}
	if _, ok := storage.(*webdavStorage); ok {
		raw, readErr := readImageBytesFromStorage(r.Context(), r, storage, key)
		if readErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取图片失败"})
			return
		}
		contentType := "application/octet-stream"
		if len(raw) > 0 {
			detectLen := len(raw)
			if detectLen > 512 {
				detectLen = 512
			}
			contentType = http.DetectContentType(raw[:detectLen])
		}
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(raw)
		return
	}
	if _, ok := storage.(*s3Storage); ok {
		raw, readErr := readImageBytesFromStorage(r.Context(), r, storage, key)
		if readErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取图片失败"})
			return
		}
		contentType := "application/octet-stream"
		if len(raw) > 0 {
			detectLen := len(raw)
			if detectLen > 512 {
				detectLen = 512
			}
			contentType = http.DetectContentType(raw[:detectLen])
		}
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(raw)
		return
	}
	http.Redirect(w, r, storage.PublicURL(r, key), http.StatusFound)
}

func (a *app) serveBanImage(w http.ResponseWriter, r *http.Request) {
	data, err := loadBanImageBytes()
	if err != nil {
		log.Printf("读取 ban.webp 失败: %v", err)
		http.NotFound(w, r)
		return
	}
	applyUploadNoCacheHeaders(w)
	w.Header().Set("Content-Type", "image/webp")
	_, _ = w.Write(data)
}

func applyUploadNoCacheHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, proxy-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("Surrogate-Control", "no-store")
}

func loadBanImageBytes() ([]byte, error) {
	paths := []string{
		dataPath("ban.webp"),
		filepath.Join(filepath.Dir(os.Args[0]), "ban.webp"),
		"/opt/himg-defaults/ban.webp",
	}
	var lastErr error
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err == nil {
			return data, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = os.ErrNotExist
	}
	return nil, lastErr
}

func readImageBytesFromStorage(ctx context.Context, r *http.Request, storage storageBackend, key string) ([]byte, error) {
	switch s := storage.(type) {
	case *localStorage:
		return os.ReadFile(filepath.Join(s.dir, key))
	case *webdavStorage:
		return s.client.Read(s.objectPath(key))
	case *s3Storage:
		obj, err := s.client.GetObject(ctx, s.bucket, s.objectKey(key), minio.GetObjectOptions{})
		if err != nil {
			return nil, err
		}
		defer obj.Close()
		return io.ReadAll(obj)
	default:
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, storage.PublicURL(r, key), nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("读取远程图片失败: %d", resp.StatusCode)
		}
		return io.ReadAll(resp.Body)
	}
}

type throttledReadCloser struct {
	reader       io.ReadCloser
	bytesPerSec  int64
	startedAt    time.Time
	totalRead    int64
	startedClock bool
}

func newThrottledReadCloser(reader io.ReadCloser, kbPerSecond int) io.ReadCloser {
	return &throttledReadCloser{
		reader:      reader,
		bytesPerSec: int64(kbPerSecond) * 1024,
	}
}

func (r *throttledReadCloser) Read(p []byte) (int, error) {
	if !r.startedClock {
		r.startedAt = time.Now()
		r.startedClock = true
	}
	n, err := r.reader.Read(p)
	if n > 0 && r.bytesPerSec > 0 {
		r.totalRead += int64(n)
		expected := time.Duration(r.totalRead*int64(time.Second)) / time.Duration(r.bytesPerSec)
		if sleepFor := r.startedAt.Add(expected).Sub(time.Now()); sleepFor > 0 {
			time.Sleep(sleepFor)
		}
	}
	return n, err
}

func (r *throttledReadCloser) Close() error {
	return r.reader.Close()
}

func downscaleIfNeeded(src image.Image, maxW int) image.Image {
	if maxW <= 0 {
		return src
	}
	b := src.Bounds()
	w := b.Dx()
	h := b.Dy()
	if w <= maxW || w <= 0 || h <= 0 {
		return src
	}
	newW := maxW
	newH := h * maxW / w
	if newH <= 0 {
		newH = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	for y := 0; y < newH; y++ {
		srcY := b.Min.Y + y*h/newH
		for x := 0; x < newW; x++ {
			srcX := b.Min.X + x*w/newW
			dst.Set(x, y, color.RGBAModel.Convert(src.At(srcX, srcY)))
		}
	}
	return dst
}

func calcTotalPages(total int64, pageSize int) int {
	if pageSize <= 0 {
		pageSize = 10
	}
	totalPages := int((total + int64(pageSize) - 1) / int64(pageSize))
	if totalPages == 0 {
		return 1
	}
	return totalPages
}

func parseLogListParams(w http.ResponseWriter, r *http.Request) (int, int, string, bool) {
	page := 1
	pageSize := 10
	var err error
	if raw := strings.TrimSpace(r.URL.Query().Get("page")); raw != "" {
		page, err = strconv.Atoi(raw)
		if err != nil || page < 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "page 参数错误"})
			return 0, 0, "", false
		}
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("page_size")); raw != "" {
		pageSize, err = strconv.Atoi(raw)
		if err != nil || pageSize < 1 || pageSize > 100 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "page_size 参数错误，范围 1-100"})
			return 0, 0, "", false
		}
	}
	keyword := strings.TrimSpace(r.URL.Query().Get("q"))
	if keyword == "" {
		keyword = strings.TrimSpace(r.URL.Query().Get("keyword"))
	}
	return page, pageSize, keyword, true
}

func formatStorageTypeLabel(storageType string) string {
	switch strings.ToLower(strings.TrimSpace(storageType)) {
	case "webdav":
		return "WebDAV"
	case "s3":
		return "S3"
	default:
		return "本地"
	}
}

func (a *app) recordLoginLog(ip, status, message string) {
	if _, err := a.db.Exec(`INSERT INTO login_logs(ip, status, message) VALUES (?, ?, ?)`, strings.TrimSpace(ip), strings.TrimSpace(status), strings.TrimSpace(message)); err != nil {
		log.Printf("记录登录日志失败: %v", err)
	}
}

func (a *app) blockIPAfterRepeatedLoginFailures(ip string) (bool, error) {
	ip = normalizeIPText(ip)
	if net.ParseIP(ip) == nil {
		return false, nil
	}

	var count int
	err := a.db.QueryRow(
		`SELECT COUNT(*) FROM login_logs WHERE ip = ? AND status = 'failed' AND created_at >= datetime('now', ?)`,
		ip,
		fmt.Sprintf("-%d minutes", int(loginFailureBlockWindow.Minutes())),
	).Scan(&count)
	if err != nil {
		return false, err
	}
	if count < loginFailureBlockLimit {
		return false, nil
	}

	cfg, err := a.loadSecurityConfig()
	if err != nil {
		return false, err
	}
	if matchIPRule(ip, cfg.BlockedIPs) {
		return false, nil
	}
	cfg.BlockedIPs = append(cfg.BlockedIPs, ip)
	cfg = normalizeSecurityConfig(cfg)
	if err := a.saveSecurityConfig(cfg); err != nil {
		return false, err
	}
	a.setSecurityConfig(cfg)
	a.recordOperationLog(ip, "自动封禁 IP", ip, fmt.Sprintf("10 分钟内登录失败 %d 次", count))
	return true, nil
}

func (a *app) recordOperationLog(ip, action, target, detail string) {
	if _, err := a.db.Exec(`INSERT INTO operation_logs(ip, action, target, detail) VALUES (?, ?, ?, ?)`, strings.TrimSpace(ip), strings.TrimSpace(action), strings.TrimSpace(target), strings.TrimSpace(detail)); err != nil {
		log.Printf("记录操作日志失败: %v", err)
	}
}

func clientIP(r *http.Request) string {
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); forwarded != "" {
		first := strings.TrimSpace(strings.Split(forwarded, ",")[0])
		if first != "" {
			return normalizeIPText(first)
		}
	}
	if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
		return normalizeIPText(realIP)
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil && host != "" {
		return normalizeIPText(host)
	}
	return normalizeIPText(r.RemoteAddr)
}

func normalizeIPText(value string) string {
	text := strings.TrimSpace(value)
	if ip := net.ParseIP(text); ip != nil {
		return ip.String()
	}
	return text
}

func normalizeIPRuleList(items []string) []string {
	out := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		rule := strings.TrimSpace(item)
		if rule == "" {
			continue
		}
		if strings.Contains(rule, "/") {
			if _, _, err := net.ParseCIDR(rule); err == nil {
				if _, ok := seen[rule]; !ok {
					seen[rule] = struct{}{}
					out = append(out, rule)
				}
			}
			continue
		}
		rule = normalizeIPText(rule)
		if rule == "" {
			continue
		}
		if _, ok := seen[rule]; ok {
			continue
		}
		seen[rule] = struct{}{}
		out = append(out, rule)
	}
	sort.Strings(out)
	return out
}

func matchIPRule(ip string, rules []string) bool {
	if len(rules) == 0 {
		return false
	}
	target := net.ParseIP(strings.TrimSpace(ip))
	plain := normalizeIPText(ip)
	for _, rule := range rules {
		rule = strings.TrimSpace(rule)
		if rule == "" {
			continue
		}
		if strings.Contains(rule, "/") {
			if target == nil {
				continue
			}
			if _, network, err := net.ParseCIDR(rule); err == nil && network.Contains(target) {
				return true
			}
			continue
		}
		if plain == normalizeIPText(rule) {
			return true
		}
	}
	return false
}

func isAdminReferrer(r *http.Request) bool {
	ref := strings.TrimSpace(r.Referer())
	if ref == "" {
		return false
	}
	u, err := url.Parse(ref)
	if err != nil {
		return false
	}
	path := strings.TrimSpace(u.Path)
	return path == "/admin" || strings.HasPrefix(path, "/admin/")
}

func (a *app) isAuthed(r *http.Request) bool {
	if a.isAPIAuthed(r) {
		return true
	}
	cookie, err := r.Cookie(cookieName)
	return err == nil && cookie.Value == a.token
}

// API 调用允许直接在请求头里带管理密码。
func (a *app) isAPIAuthed(r *http.Request) bool {
	password := strings.TrimSpace(r.Header.Get("X-API-Password"))
	if password != "" && password == a.currentPassword() {
		return true
	}

	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(auth, "Bearer ") && strings.TrimSpace(strings.TrimPrefix(auth, "Bearer ")) == a.currentPassword() {
		return true
	}
	return false
}

// 按格式解码上传图片，便于后续统一编码成 WebP。
func decodeImage(data []byte, filename string) (image.Image, error) {
	if strings.EqualFold(filepath.Ext(filename), ".webp") {
		return webp.Decode(bytes.NewReader(data))
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	return img, err
}

func writeJSON(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
}

func (a *app) securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := a.currentSecurityConfig()
		if cfg.SecurityHeadersEnabled {
			applySecurityHeaders(w)
		}
		if !cfg.Enabled {
			next.ServeHTTP(w, r)
			return
		}
		ip := clientIP(r)
		if matchIPRule(ip, cfg.BlockedIPs) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "当前 IP 已被安全策略拦截"})
			return
		}
		if cfg.InjectionFilterEnabled && hasSuspiciousRequestPattern(r) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求命中安全过滤规则"})
			return
		}
		if cfg.RateLimitEnabled && !a.allowSecurityRate(ip, cfg.RequestsPerMinute) {
			w.Header().Set("Retry-After", "60")
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "请求过于频繁，请稍后再试"})
			return
		}
		if cfg.MaxBodyMB > 0 && r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, int64(cfg.MaxBodyMB)<<20)
		}
		next.ServeHTTP(w, r)
	})
}

func applySecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob: http: https:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'")
}

func (a *app) allowSecurityRate(ip string, limit int) bool {
	if limit <= 0 {
		return true
	}
	now := time.Now()
	cutoff := now.Add(-time.Minute)
	a.rateMu.Lock()
	defer a.rateMu.Unlock()
	hits := a.rateHits[ip]
	start := 0
	for start < len(hits) && hits[start].Before(cutoff) {
		start++
	}
	if start > 0 {
		hits = append([]time.Time(nil), hits[start:]...)
	}
	if len(hits) >= limit {
		a.rateHits[ip] = hits
		return false
	}
	hits = append(hits, now)
	a.rateHits[ip] = hits
	if len(a.rateHits) > 2048 {
		for key, values := range a.rateHits {
			if len(values) == 0 || values[len(values)-1].Before(cutoff) {
				delete(a.rateHits, key)
			}
		}
	}
	return true
}

func hasSuspiciousRequestPattern(r *http.Request) bool {
	target := strings.ToLower(r.URL.Path + "?" + r.URL.RawQuery)
	patterns := []string{
		"../",
		"..%2f",
		"%2e%2e",
		"<script",
		"%3cscript",
		"union%20select",
		"union+select",
		" union select ",
		"information_schema",
		"benchmark(",
		"sleep(",
		" or 1=1",
		"' or '1'='1",
		"\" or \"1\"=\"1",
	}
	for _, pattern := range patterns {
		if strings.Contains(target, pattern) {
			return true
		}
	}
	return false
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func loadRootEnvFile() {
	for _, path := range configFileCandidates() {
		if err := loadEnvFile(path); err == nil {
			return
		}
	}
}

func configFileCandidates() []string {
	candidates := make([]string, 0, 3)
	if path := strings.TrimSpace(os.Getenv("HIMG_CONFIG_FILE")); path != "" {
		candidates = append(candidates, path)
	}
	candidates = append(candidates, ".env")
	if isCodeWorkdir() {
		candidates = append(candidates, "../.env")
	}
	return candidates
}

func loadEnvFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || os.Getenv(key) != "" {
			continue
		}
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		}
		_ = os.Setenv(key, value)
	}
	return nil
}

func dataPath(name string) string {
	return filepath.Join(env("HIMG_DATA_DIR", defaultDataDir()), name)
}

func databaseFile() string {
	return env("HIMG_DB_FILE", dataPath("himg.db"))
}

func defaultDataDir() string {
	if isCodeWorkdir() {
		if info, err := os.Stat("../data"); err == nil && info.IsDir() {
			return "../data"
		}
	}
	return "data"
}

func defaultThemesDir() string {
	if isCodeWorkdir() {
		if info, err := os.Stat("../themes"); err == nil && info.IsDir() {
			return "../themes"
		}
	}
	return "themes"
}

func isCodeWorkdir() bool {
	cwd, err := os.Getwd()
	return err == nil && filepath.Base(cwd) == "code"
}

func mustToken() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		log.Fatal(err)
	}
	return hex.EncodeToString(buf)
}

func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// 记录请求，便于排查问题。
func logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

// 统一处理跨域请求，便于前端直接调用 API。
func cors(next http.Handler) http.Handler {
	allowOrigins := splitAndTrim(env("HIMG_CORS_ALLOW_ORIGIN", "*"))
	allowMethods := env("HIMG_CORS_ALLOW_METHODS", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
	allowHeaders := env("HIMG_CORS_ALLOW_HEADERS", "Content-Type,Authorization,X-API-Password")
	allowCredentials := envBool("HIMG_CORS_ALLOW_CREDENTIALS", false)
	allowAnyOrigin := len(allowOrigins) == 0 || (len(allowOrigins) == 1 && allowOrigins[0] == "*")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		if origin != "" {
			switch {
			case allowAnyOrigin && !allowCredentials:
				w.Header().Set("Access-Control-Allow-Origin", "*")
			case allowAnyOrigin && allowCredentials:
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
			case containsString(allowOrigins, origin):
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
			}
			w.Header().Set("Access-Control-Allow-Methods", allowMethods)
			w.Header().Set("Access-Control-Allow-Headers", allowHeaders)
			w.Header().Set("Access-Control-Max-Age", "86400")
			if allowCredentials {
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func splitAndTrim(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		item := strings.TrimSpace(part)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}
