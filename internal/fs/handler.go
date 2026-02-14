package fs

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/docker/docker/client"
)

func Register(mux *http.ServeMux, cli *client.Client) {
	svc := New(cli)
	mux.HandleFunc("/fs/tree", treeHandler(svc))
	mux.HandleFunc("/fs/file", fileHandler(svc))
}

func treeHandler(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		containerID := r.URL.Query().Get("id")
		if containerID == "" {
			http.Error(w, `missing "id" query parameter`, http.StatusBadRequest)
			return
		}

		tree, err := svc.GetFileTree(r.Context(), containerID)
		if err != nil {
			log.Printf("[fs/tree] error for container %s: %v", containerID, err)
			http.Error(w, "failed to get file tree", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(tree); err != nil {
			log.Printf("[fs/tree] encode error: %v", err)
		}
	}
}

func fileHandler(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		containerID := r.URL.Query().Get("id")
		if containerID == "" {
			http.Error(w, `missing "id" query parameter`, http.StatusBadRequest)
			return
		}

		filePath := r.URL.Query().Get("path")
		if filePath == "" {
			http.Error(w, `missing "path" query parameter`, http.StatusBadRequest)
			return
		}

		switch r.Method {
		case http.MethodGet:
			content, err := svc.ReadFile(r.Context(), containerID, filePath)
			if err != nil {
				log.Printf("[fs/file] read error for %s in %s: %v", filePath, containerID, err)
				if strings.Contains(err.Error(), "No such container") || strings.Contains(err.Error(), "not found") {
					http.Error(w, "file not found", http.StatusNotFound)
				} else {
					http.Error(w, "failed to read file", http.StatusInternalServerError)
				}
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = io.WriteString(w, content)

		case http.MethodPost:
			body, err := io.ReadAll(io.LimitReader(r.Body, maxFileSize))
			if err != nil {
				http.Error(w, "failed to read request body", http.StatusBadRequest)
				return
			}
			if err := svc.WriteFile(r.Context(), containerID, filePath, string(body)); err != nil {
				log.Printf("[fs/file] write error for %s in %s: %v", filePath, containerID, err)
				http.Error(w, "failed to write file", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}
