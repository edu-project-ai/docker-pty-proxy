package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/gorilla/websocket"
)

const (
	readBufSize = 4096
	writeDeadline = 10 * time.Second
)

type resizeMsg struct {
	Type string `json:"type"`
	Cols uint   `json:"cols"`
	Rows uint   `json:"rows"`
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  readBufSize,
	WriteBufferSize: readBufSize,
	CheckOrigin: func(r *http.Request) bool { return true },
}

func Register(mux *http.ServeMux, cli *client.Client) {
	mux.HandleFunc("/attach", attachHandler(cli))
	mux.HandleFunc("/resize", resizeHandler(cli))
	mux.HandleFunc("/healthz", healthHandler(cli))
}

func attachHandler(cli *client.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		containerID := r.URL.Query().Get("id")
		if containerID == "" {
			http.Error(w, `missing "id" query parameter`, http.StatusBadRequest)
			return
		}

		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("[attach] websocket upgrade failed: %v", err)
			return
		}
		defer ws.Close()

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		log.Printf("[attach] connecting to container %s", containerID)

		hijack, err := cli.ContainerAttach(ctx, containerID, container.AttachOptions{
			Stream: true,
			Stdin:  true,
			Stdout: true,
			Stderr: true,
		})
		if err != nil {
			log.Printf("[attach] container attach error: %v", err)
			_ = ws.WriteMessage(websocket.TextMessage, []byte("attach error: "+err.Error()))
			return
		}
		defer hijack.Close()

		log.Printf("[attach] attached to container %s, starting bidirectional pipe", containerID)

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			defer cancel()
			buf := make([]byte, readBufSize)
			for {
				n, readErr := hijack.Reader.Read(buf)
				if n > 0 {
					_ = ws.SetWriteDeadline(time.Now().Add(writeDeadline))
					if writeErr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); writeErr != nil {
						log.Printf("[attach] write to websocket failed: %v", writeErr)
						return
					}
				}
				if readErr != nil {
					if readErr != io.EOF {
						log.Printf("[attach] read from docker failed: %v", readErr)
					}
					return
				}
			}
		}()

		go func() {
			defer wg.Done()
			defer cancel()
			for {
				mt, payload, readErr := ws.ReadMessage()
				if readErr != nil {
					if websocket.IsUnexpectedCloseError(readErr, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
						log.Printf("[attach] read from websocket failed: %v", readErr)
					}
					return
				}
				if mt == websocket.CloseMessage {
					return
				}
				if len(payload) == 0 {
					continue
				}

				if payload[0] == '{' {
					var msg resizeMsg
					if json.Unmarshal(payload, &msg) == nil && msg.Type == "resize" {
						if err := cli.ContainerResize(ctx, containerID, container.ResizeOptions{
							Height: msg.Rows,
							Width:  msg.Cols,
						}); err != nil {
							log.Printf("[attach] resize failed: %v", err)
						}
						continue
					}
				}

				if _, writeErr := hijack.Conn.Write(payload); writeErr != nil {
					log.Printf("[attach] write to docker failed: %v", writeErr)
					return
				}
			}
		}()

		wg.Wait()
		log.Printf("[attach] session ended for container %s", containerID)
	}
}

func resizeHandler(cli *client.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		containerID := r.URL.Query().Get("id")
		if containerID == "" {
			http.Error(w, `missing "id" query parameter`, http.StatusBadRequest)
			return
		}

		cols, err := strconv.ParseUint(r.URL.Query().Get("w"), 10, 32)
		if err != nil || cols == 0 {
			http.Error(w, `invalid "w" (cols) parameter`, http.StatusBadRequest)
			return
		}

		rows, err := strconv.ParseUint(r.URL.Query().Get("h"), 10, 32)
		if err != nil || rows == 0 {
			http.Error(w, `invalid "h" (rows) parameter`, http.StatusBadRequest)
			return
		}

		if err := cli.ContainerResize(r.Context(), containerID, container.ResizeOptions{
			Height: uint(rows),
			Width:  uint(cols),
		}); err != nil {
			http.Error(w, fmt.Sprintf("resize failed: %v", err), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}

func healthHandler(cli *client.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		ping, err := cli.Ping(ctx)
		if err != nil {
			http.Error(w, fmt.Sprintf("docker unreachable: %v", err), http.StatusServiceUnavailable)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":     "ok",
			"docker_api": ping.APIVersion,
		})
	}
}
