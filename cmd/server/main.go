package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/edu-project-ai/docker-pty-proxy/internal/docker"
	"github.com/edu-project-ai/docker-pty-proxy/internal/handler"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	cli, err := docker.New()
	if err != nil {
		log.Fatalf("FATAL: %v", err)
	}
	defer cli.Close()

	if err := docker.Ping(context.Background(), cli); err != nil {
		log.Printf("WARNING: %v (service will start but may not work)", err)
	} else {
		log.Printf("Docker daemon connected (DOCKER_HOST=%s)", os.Getenv("DOCKER_HOST"))
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	handler.Register(mux, cli)

	corsHandler := corsMiddleware(mux)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           corsHandler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("docker-pty-proxy listening on :%s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-stop
	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	log.Println("server stopped")
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}
