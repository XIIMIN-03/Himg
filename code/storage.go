package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/studio-b12/gowebdav"
)

type storageConfig struct {
	Type          string `json:"type"`
	PublicBaseURL string `json:"public_base_url"`
	LocalDir      string `json:"local_dir"`
	WebDAVURL     string `json:"webdav_url"`
	WebDAVUser    string `json:"webdav_user"`
	WebDAVPass    string `json:"webdav_password"`
	WebDAVBase    string `json:"webdav_base_path"`
	S3Endpoint    string `json:"s3_endpoint"`
	S3AccessKey   string `json:"s3_access_key"`
	S3SecretKey   string `json:"s3_secret_key"`
	S3Bucket      string `json:"s3_bucket"`
	S3Region      string `json:"s3_region"`
	S3UseSSL      bool   `json:"s3_use_ssl"`
	S3Prefix      string `json:"s3_prefix"`
}

type storageBackend interface {
	Name() string
	Save(ctx context.Context, key string, data []byte, contentType string) error
	Delete(ctx context.Context, key string) error
	PublicURL(r *http.Request, key string) string
	FileServer() http.Handler
}

func newStorageFromEnv() (storageBackend, error) {
	return newStorageFromConfig(defaultStorageConfig())
}

func newStorageFromConfig(cfg storageConfig) (storageBackend, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Type)) {
	case "", "local":
		return newLocalStorage(cfg)
	case "webdav":
		return newWebDAVStorage(cfg)
	case "s3":
		return newS3Storage(cfg)
	default:
		return nil, fmt.Errorf("不支持的存储类型: %s", cfg.Type)
	}
}

func defaultStorageConfig() storageConfig {
	return storageConfig{
		Type:          env("HIMG_STORAGE", "local"),
		PublicBaseURL: strings.TrimRight(env("HIMG_PUBLIC_BASE_URL", ""), "/"),
		LocalDir:      env("LOCAL_UPLOAD_DIR", defaultLocalUploadDir()),
		WebDAVURL:     env("WEBDAV_URL", ""),
		WebDAVUser:    env("WEBDAV_USER", ""),
		WebDAVPass:    env("WEBDAV_PASSWORD", ""),
		WebDAVBase:    env("WEBDAV_BASE_PATH", ""),
		S3Endpoint:    env("S3_ENDPOINT", ""),
		S3AccessKey:   env("S3_ACCESS_KEY", ""),
		S3SecretKey:   env("S3_SECRET_KEY", ""),
		S3Bucket:      env("S3_BUCKET", ""),
		S3Region:      env("S3_REGION", ""),
		S3UseSSL:      envBool("S3_USE_SSL", true),
		S3Prefix:      env("S3_PREFIX", ""),
	}
}

func normalizeStorageConfig(cfg storageConfig) storageConfig {
	cfg.Type = strings.ToLower(strings.TrimSpace(cfg.Type))
	if cfg.Type == "" {
		cfg.Type = "local"
	}
	cfg.PublicBaseURL = strings.TrimRight(strings.TrimSpace(cfg.PublicBaseURL), "/")
	cfg.LocalDir = strings.TrimSpace(cfg.LocalDir)
	if cfg.LocalDir == "" {
		cfg.LocalDir = defaultLocalUploadDir()
	}
	cfg.WebDAVURL = strings.TrimRight(strings.TrimSpace(cfg.WebDAVURL), "/")
	cfg.WebDAVBase = strings.Trim(strings.TrimSpace(cfg.WebDAVBase), "/")
	cfg.S3Endpoint = stripScheme(cfg.S3Endpoint)
	cfg.S3Bucket = strings.TrimSpace(cfg.S3Bucket)
	cfg.S3Region = strings.TrimSpace(cfg.S3Region)
	cfg.S3Prefix = strings.Trim(strings.TrimSpace(cfg.S3Prefix), "/")
	return cfg
}

func defaultLocalUploadDir() string {
	return filepath.Join(env("HIMG_DATA_DIR", defaultDataDir()), "uploads")
}

func marshalStorageConfig(cfg storageConfig) string {
	data, _ := json.Marshal(normalizeStorageConfig(cfg))
	return string(data)
}

func parseStorageConfig(raw string) storageConfig {
	cfg := defaultStorageConfig()
	if strings.TrimSpace(raw) == "" {
		return normalizeStorageConfig(cfg)
	}
	_ = json.Unmarshal([]byte(raw), &cfg)
	return normalizeStorageConfig(cfg)
}

type localStorage struct {
	dir           string
	publicBaseURL string
}

func newLocalStorage(cfg storageConfig) (storageBackend, error) {
	cfg = normalizeStorageConfig(cfg)
	dir := cfg.LocalDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &localStorage{
		dir:           dir,
		publicBaseURL: cfg.PublicBaseURL,
	}, nil
}

func (s *localStorage) Name() string { return "local" }

func (s *localStorage) Save(_ context.Context, key string, data []byte, _ string) error {
	return os.WriteFile(filepath.Join(s.dir, key), data, 0o644)
}

func (s *localStorage) Delete(_ context.Context, key string) error {
	err := os.Remove(filepath.Join(s.dir, key))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *localStorage) PublicURL(r *http.Request, key string) string {
	return appUploadURL(r, s.publicBaseURL, key)
}

func (s *localStorage) FileServer() http.Handler {
	return http.FileServer(http.Dir(s.dir))
}

type webdavStorage struct {
	client        *gowebdav.Client
	rootURL       string
	basePath      string
	publicBaseURL string
}

func newWebDAVStorage(cfg storageConfig) (storageBackend, error) {
	cfg = normalizeStorageConfig(cfg)
	rootURL := cfg.WebDAVURL
	if rootURL == "" {
		return nil, fmt.Errorf("WEBDAV_URL 未配置")
	}
	client := gowebdav.NewClient(rootURL, cfg.WebDAVUser, cfg.WebDAVPass)
	basePath := cfg.WebDAVBase
	if basePath != "" {
		if err := client.MkdirAll(basePath, 0o755); err != nil {
			return nil, err
		}
	}
	return &webdavStorage{
		client:        client,
		rootURL:       rootURL,
		basePath:      basePath,
		publicBaseURL: cfg.PublicBaseURL,
	}, nil
}

func (s *webdavStorage) Name() string { return "webdav" }

func (s *webdavStorage) Save(_ context.Context, key string, data []byte, _ string) error {
	remotePath := s.objectPath(key)
	dir := path.Dir(remotePath)
	if dir != "." && dir != "/" {
		if err := s.client.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return s.client.Write(remotePath, data, 0o644)
}

func (s *webdavStorage) Delete(_ context.Context, key string) error {
	err := s.client.Remove(s.objectPath(key))
	if gowebdav.IsErrNotFound(err) {
		return nil
	}
	return err
}

func (s *webdavStorage) PublicURL(r *http.Request, key string) string {
	return appUploadURL(r, s.publicBaseURL, key)
}

func (s *webdavStorage) FileServer() http.Handler { return nil }

func (s *webdavStorage) objectPath(key string) string {
	if s.basePath == "" {
		return strings.TrimLeft(key, "/")
	}
	return path.Join(s.basePath, key)
}

type s3Storage struct {
	client        *minio.Client
	endpoint      string
	bucket        string
	prefix        string
	secure        bool
	publicBaseURL string
}

func newS3Storage(cfg storageConfig) (storageBackend, error) {
	cfg = normalizeStorageConfig(cfg)
	endpoint := cfg.S3Endpoint
	if endpoint == "" {
		return nil, fmt.Errorf("S3_ENDPOINT 未配置")
	}
	client, err := minio.New(stripScheme(endpoint), &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.S3AccessKey, cfg.S3SecretKey, ""),
		Secure: cfg.S3UseSSL,
		Region: cfg.S3Region,
	})
	if err != nil {
		return nil, err
	}
	bucket := cfg.S3Bucket
	if bucket == "" {
		return nil, fmt.Errorf("S3_BUCKET 未配置")
	}
	return &s3Storage{
		client:        client,
		endpoint:      stripScheme(endpoint),
		bucket:        bucket,
		prefix:        cfg.S3Prefix,
		secure:        cfg.S3UseSSL,
		publicBaseURL: cfg.PublicBaseURL,
	}, nil
}

func (s *s3Storage) Name() string { return "s3" }

func (s *s3Storage) Save(ctx context.Context, key string, data []byte, contentType string) error {
	_, err := s.client.PutObject(ctx, s.bucket, s.objectKey(key), bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{
		ContentType: contentType,
	})
	return err
}

func (s *s3Storage) Delete(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, s.objectKey(key), minio.RemoveObjectOptions{})
}

func (s *s3Storage) PublicURL(r *http.Request, key string) string {
	return appUploadURL(r, s.publicBaseURL, key)
}

func (s *s3Storage) FileServer() http.Handler { return nil }

func (s *s3Storage) objectKey(key string) string {
	if s.prefix == "" {
		return strings.TrimLeft(key, "/")
	}
	return path.Join(s.prefix, key)
}

func joinURL(baseURL, key string) string {
	return strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(key, "/")
}

func appUploadURL(r *http.Request, publicBaseURL, key string) string {
	baseURL := requestBaseURL(r)
	if strings.TrimSpace(publicBaseURL) != "" {
		baseURL = strings.TrimRight(strings.TrimSpace(publicBaseURL), "/")
	}
	return joinURL(baseURL, "uploads/"+strings.TrimLeft(key, "/"))
}

func stripScheme(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	endpoint = strings.TrimPrefix(endpoint, "https://")
	endpoint = strings.TrimPrefix(endpoint, "http://")
	return strings.TrimRight(endpoint, "/")
}

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if value == "" {
		return fallback
	}
	switch value {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}
