package main

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/gorilla/websocket"
)

const defaultDockerHost = "npipe:////./pipe/docker_engine"

func main() {
	dockerHost := os.Getenv("DOCKER_HOST")
	if dockerHost == "" {
		dockerHost = defaultDockerHost
	}

	cli, err := newDockerClient(dockerHost)
	if err != nil {
		log.Fatalf("failed to create docker client: %v", err)
	}
	defer cli.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/attach", attachHandler(cli))

	srv := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("listening on %s, Docker host=%s", srv.Addr, dockerHost)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-stop
	log.Printf("shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func newDockerClient(dockerHost string) (*client.Client, error) {
	if strings.HasPrefix(dockerHost, "npipe:") {
		raw := strings.TrimPrefix(dockerHost, "npipe:")
		raw = strings.TrimPrefix(raw, "////")
		raw = strings.TrimLeft(raw, "/")
		pipePath := `\\.\` + strings.ReplaceAll(raw, "/", `\\`)

		transport := &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return winio.DialPipeContext(ctx, pipePath)
			},
		}

		httpClient := &http.Client{Transport: transport}
		return client.NewClientWithOpts(
			client.WithHost(dockerHost),
			client.WithHTTPClient(httpClient),
			client.WithAPIVersionNegotiation(),
		)
	}

	return client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
}

func attachHandler(cli *client.Client) http.HandlerFunc {
	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     func(r *http.Request) bool { return true },
	}

	return func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "missing id query parameter", http.StatusBadRequest)
			return
		}

		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("websocket upgrade failed: %v", err)
			return
		}
		defer ws.Close()

		ctx := r.Context()

		attachOpts := types.ContainerAttachOptions{Stream: true, Stdin: true, Stdout: true, Stderr: true, Tty: true}
		hijackResp, err := cli.ContainerAttach(ctx, id, attachOpts)
		if err != nil {
			log.Printf("container attach error: %v", err)
			ws.WriteMessage(websocket.TextMessage, []byte("attach error: "+err.Error()))
			return
		}
		defer func() { _ = hijackResp.Close() }()

		done := make(chan struct{})

		go func() {
			buf := make([]byte, 1024)
			for {
				n, err := hijackResp.Reader.Read(buf)
				if err != nil {
					if err != io.EOF {
						log.Printf("read from docker failed: %v", err)
					}
					break
				}
				if n > 0 {
					ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
					if err := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
						log.Printf("write to websocket failed: %v", err)
						break
					}
				}
			}
			close(done)
		}()

		for {
			mt, p, err := ws.ReadMessage()
			if err != nil {
				log.Printf("read from websocket failed: %v", err)
				break
			}
			if mt == websocket.CloseMessage {
				break
			}
			if len(p) == 0 {
				continue
			}
			if _, err := hijackResp.Conn.Write(p); err != nil {
				log.Printf("write to docker failed: %v", err)
				break
			}
		}

		<-done
	}
}
