package proxy

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// handleAdminUI serves the admin dashboard from web/ directory
func (s *Server) handleAdminUI(w http.ResponseWriter, r *http.Request) {
	// Determine the web directory path
	webDir := "web"
	if exe, err := os.Executable(); err == nil {
		webDir = filepath.Join(filepath.Dir(exe), "web")
		// If not found next to exe, try current working directory
		if _, err := os.Stat(webDir); os.IsNotExist(err) {
			webDir = "web"
		}
	}

	// Map URL path to file path
	urlPath := strings.TrimPrefix(r.URL.Path, "/admin")
	if urlPath == "" || urlPath == "/" {
		urlPath = "/index.html"
	}

	filePath := filepath.Join(webDir, filepath.Clean(urlPath))

	// Security: prevent directory traversal
	absWeb, _ := filepath.Abs(webDir)
	absFile, _ := filepath.Abs(filePath)
	if !strings.HasPrefix(absFile, absWeb) {
		http.NotFound(w, r)
		return
	}

	// Check file exists
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		http.NotFound(w, r)
		return
	}

	// Set content type based on extension
	ext := filepath.Ext(filePath)
	switch ext {
	case ".html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".js":
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	case ".json":
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	case ".png":
		w.Header().Set("Content-Type", "image/png")
	case ".svg":
		w.Header().Set("Content-Type", "image/svg+xml")
	case ".ico":
		w.Header().Set("Content-Type", "image/x-icon")
	}

	// Disable caching during development
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")

	http.ServeFile(w, r, filePath)
}
