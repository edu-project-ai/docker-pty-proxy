package proxy

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"

	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

// Regex to match: {port}-{container_id}.{baseDomain}
// Example: 8000-abcd123.localhost
// The host might include the port like 8000-abcd123.localhost:8080
var hostRegex = regexp.MustCompile(`^(\d+)-([a-f0-9]+)\.`)

// Regex to match path-based proxy: /proxy/{containerId}/{port}/{path...}
// Example: /proxy/abc123def/8000/api/users
var pathProxyRegex = regexp.MustCompile(`^/proxy/([a-f0-9]+)/(\d+)(/.*)?$`)

type ProxyHandler struct {
	cli *client.Client
}

func NewHandler(cli *client.Client) *ProxyHandler {
	return &ProxyHandler{cli: cli}
}

// Middleware intercepts requests that are meant for a sub-domain proxy or path-based proxy
func (p *ProxyHandler) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check for path-based proxy: /proxy/{containerId}/{port}/{path...}
		pathMatches := pathProxyRegex.FindStringSubmatch(r.URL.Path)
		if len(pathMatches) >= 3 {
			containerID := pathMatches[1]
			port := pathMatches[2]
			// Rewrite the path to strip the /proxy/{containerId}/{port} prefix
			targetPath := "/"
			if len(pathMatches) >= 4 && pathMatches[3] != "" {
				targetPath = pathMatches[3]
			}
			r.URL.Path = targetPath
			r.URL.RawPath = ""
			p.handleProxy(w, r, containerID, port)
			return
		}

		// Check for subdomain-based proxy: {port}-{containerId}.domain
		host := r.Host // e.g. "8000-c7b4f.localhost:8080"
		hostMatches := hostRegex.FindStringSubmatch(host)
		if len(hostMatches) == 3 {
			port := hostMatches[1]
			containerID := hostMatches[2]
			p.handleProxy(w, r, containerID, port)
			return
		}

		// Fallback to normal routes
		next.ServeHTTP(w, r)
	})
}

func (p *ProxyHandler) handleProxy(w http.ResponseWriter, r *http.Request, containerID, port string) {
	targetURL, err := p.resolveTarget(r.Context(), containerID, port)
	if err != nil {
		log.Printf("[proxy] cannot resolve target for container %s port %s: %v", containerID, port, err)
		http.Error(w, fmt.Sprintf("Cannot reach container %s on port %s: %v", containerID, port, err), http.StatusBadGateway)
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(targetURL)

	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = targetURL.Scheme
		req.URL.Host = targetURL.Host
		req.Host = targetURL.Host
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
		log.Printf("[proxy] error proxying to %s: %v", targetURL.String(), err)
		http.Error(w, "Error proxying to container. Is the server running inside the container on port "+port+"?", http.StatusBadGateway)
	}

	proxy.ServeHTTP(w, r)
}

// resolveTarget picks the best address to reach the container port.
// On Windows / Docker Desktop the bridge IPs are not routable from the host,
// so we prefer the published (mapped) host port when available.
func (p *ProxyHandler) resolveTarget(ctx context.Context, containerID, port string) (*url.URL, error) {
	info, err := p.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return nil, fmt.Errorf("container inspect: %w", err)
	}
	if !info.State.Running {
		return nil, fmt.Errorf("container is not running")
	}

	// Prefer mapped host port (works when proxy runs on the Docker host / Windows)
	portKey := nat.Port(port + "/tcp")
	if bindings, ok := info.NetworkSettings.Ports[portKey]; ok && len(bindings) > 0 {
		hostPort := bindings[0].HostPort
		if hostPort != "" && hostPort != "0" {
			log.Printf("[proxy] container %s port %s → host localhost:%s (mapped)", containerID[:12], port, hostPort)
			return url.Parse(fmt.Sprintf("http://localhost:%s", hostPort))
		}
	}

	// Fallback: direct container IP (works inside Docker network / Linux)
	for _, net := range info.NetworkSettings.Networks {
		if net.IPAddress != "" {
			log.Printf("[proxy] container %s port %s → direct %s:%s (no mapped port)", containerID[:12], port, net.IPAddress, port)
			return url.Parse(fmt.Sprintf("http://%s:%s", net.IPAddress, port))
		}
	}

	return nil, fmt.Errorf("no reachable address found for container")
}

