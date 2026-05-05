package main

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const defaultThemeName = "default"

func ensureThemesDir() error {
	return os.MkdirAll(themesDir(), 0o755)
}

func sanitizeThemeName(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	name = strings.TrimSuffix(name, filepath.Ext(name))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-_")
	if s == "" {
		return "theme"
	}
	return s
}

func (a *app) currentTheme() string {
	a.themeMu.RLock()
	defer a.themeMu.RUnlock()
	return a.theme
}

func (a *app) setTheme(name string) {
	a.themeMu.Lock()
	defer a.themeMu.Unlock()
	a.theme = name
}

func (a *app) loadThemeName() (string, error) {
	var name string
	err := a.db.QueryRow(`SELECT active_theme FROM settings WHERE id = 1`).Scan(&name)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return strings.TrimSpace(name), err
}

func (a *app) saveThemeName(name string) error {
	_, err := a.db.Exec(`UPDATE settings SET active_theme = ?, updated_at = CURRENT_TIMESTAMP WHERE id = 1`, strings.TrimSpace(name))
	return err
}

func themeExists(name string) bool {
	if strings.TrimSpace(name) == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(themesDir(), name))
	return err == nil && info.IsDir()
}

func listThemes() ([]string, error) {
	if err := ensureThemesDir(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(themesDir())
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != defaultThemeName {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func installThemeZip(themeName string, data []byte) error {
	if err := ensureThemesDir(); err != nil {
		return err
	}
	themeName = sanitizeThemeName(themeName)
	targetDir := filepath.Join(themesDir(), themeName)
	if err := os.RemoveAll(targetDir); err != nil {
		return err
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return err
	}

	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}

	hasThemeCSS := false
	for _, file := range reader.File {
		name := filepath.Clean(file.Name)
		if name == "." || strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			return os.ErrPermission
		}
		destPath := filepath.Join(targetDir, name)
		if !strings.HasPrefix(destPath, targetDir+string(os.PathSeparator)) && destPath != targetDir {
			return os.ErrPermission
		}

		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(destPath, 0o755); err != nil {
				return err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			return err
		}
		rc, err := file.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(destPath)
		if err != nil {
			rc.Close()
			return err
		}
		_, copyErr := io.Copy(out, rc)
		closeErr := out.Close()
		rc.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if name == "theme.css" {
			hasThemeCSS = true
		}
	}

	if !hasThemeCSS {
		return os.ErrInvalid
	}
	return nil
}

func (a *app) themePagePath(name string) string {
	pageName := filepath.Base(name)
	for _, themeName := range []string{strings.TrimSpace(a.currentTheme()), defaultThemeName} {
		if themeName == "" {
			continue
		}
		fullPath := filepath.Join(themesDir(), themeName, pageName)
		info, err := os.Stat(fullPath)
		if err == nil && !info.IsDir() {
			return fullPath
		}
	}
	return ""
}

func (a *app) servePage(w http.ResponseWriter, r *http.Request, fallback string) {
	if themed := a.themePagePath(fallback); themed != "" {
		// Theme can switch while URL stays the same (/admin, /).
		// Disable page cache to avoid stale 304 responses serving wrong theme.
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		http.ServeFile(w, r, themed)
		return
	}
	http.NotFound(w, r)
}

func (a *app) serveTheme(w http.ResponseWriter, r *http.Request) {
	relPath := filepath.Clean(strings.TrimPrefix(r.URL.Path, "/theme/"))
	if relPath == "." || strings.HasPrefix(relPath, "..") || filepath.IsAbs(relPath) {
		http.NotFound(w, r)
		return
	}

	for _, themeName := range []string{strings.TrimSpace(a.currentTheme()), defaultThemeName} {
		if themeName == "" {
			continue
		}
		themeRoot := filepath.Join(themesDir(), themeName)
		fullPath := filepath.Join(themeRoot, relPath)
		if !strings.HasPrefix(fullPath, themeRoot+string(os.PathSeparator)) && fullPath != themeRoot {
			continue
		}
		data, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}

		contentType := mime.TypeByExtension(filepath.Ext(fullPath))
		if contentType == "" {
			contentType = http.DetectContentType(data)
		}
		w.Header().Set("Content-Type", contentType)
		w.Write(data)
		return
	}
	http.NotFound(w, r)
}

func themesDir() string {
	return env("HIMG_THEMES_DIR", defaultThemesDir())
}
