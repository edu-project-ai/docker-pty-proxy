package fs

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

const maxFileSize = 5 * 1024 * 1024 // 5 MB

type FileNode struct {
	Name     string      `json:"name"`
	Path     string      `json:"path"`
	Type     string      `json:"type"` // "file" or "folder"
	Children []*FileNode `json:"children,omitempty"`
}

type SearchResult struct {
	File   string `json:"file"`   // relative path
	Line   int    `json:"line"`   // line number (1-based)
	Column int    `json:"column"` // column (1-based)
	Text   string `json:"text"`   // matching line content
}

type Service struct {
	cli *client.Client
}

func New(cli *client.Client) *Service {
	return &Service{cli: cli}
}

func validatePath(p string) error {
	if p == "" {
		return fmt.Errorf("path must not be empty")
	}
	if strings.ContainsRune(p, 0) {
		return fmt.Errorf("path must not contain null bytes")
	}
	cleaned := path.Clean(p)
	if strings.HasPrefix(cleaned, "/") {
		return fmt.Errorf("path must be relative")
	}
	if strings.HasPrefix(cleaned, "..") {
		return fmt.Errorf("path must not traverse above workspace root")
	}
	return nil
}

func (s *Service) GetFileTree(ctx context.Context, containerID string) ([]*FileNode, error) {
	// FIX: Alpine Linux (BusyBox) не підтримує -printf.
	// Використовуємо -exec stat, щоб отримати шлях і тип файлу.
	// %n = ім'я файлу, %F = тип (regular file / directory)
	cmd := []string{
		"find", ".", "-maxdepth", "4",
		"-not", "-path", "*/.*",
		"-not", "-path", "./node_modules*",
		"-exec", "stat", "-c", "%n:%F", "{}", "+",
	}

	execResp, err := s.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          false,
		WorkingDir:   "/workspace",
	})
	if err != nil {
		return nil, fmt.Errorf("exec create: %w", err)
	}

	hijack, err := s.cli.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{
		Tty: false,
	})
	if err != nil {
		return nil, fmt.Errorf("exec attach: %w", err)
	}
	defer hijack.Close()

	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, hijack.Reader); err != nil {
		return nil, fmt.Errorf("read exec output: %w", err)
	}

	// Логуємо помилки, якщо find впав
	if stderr.Len() > 0 {
		log.Printf("[FileTree] stderr: %s", stderr.String())
	}

	return buildTree(stdout.String())
}

func buildTree(output string) ([]*FileNode, error) {
	dirs := make(map[string]*FileNode)
	topLevel := make([]*FileNode, 0)

	lines := strings.Split(strings.TrimSpace(output), "\n")
	sort.Strings(lines)

	for _, line := range lines {
		if line == "" {
			continue
		}

		// Формат лінії: ./src/main.ts:regular file  АБО  ./src:directory
		parts := strings.Split(line, ":")
		if len(parts) < 2 {
			continue
		}

		rawPath := parts[0]
		rawType := parts[1]

		// Очищуємо шлях від "./" на початку
		relPath := strings.TrimPrefix(rawPath, "./")
		if relPath == "." || relPath == "" {
			continue
		}

		node := &FileNode{
			Name: path.Base(relPath),
			Path: relPath,
		}

		// Визначаємо тип
		if strings.Contains(rawType, "directory") {
			node.Type = "folder"
			node.Children = make([]*FileNode, 0)
			dirs[relPath] = node
		} else {
			node.Type = "file"
		}

		// Будуємо дерево
		parentPath := path.Dir(relPath)
		if parentPath == "." {
			topLevel = append(topLevel, node)
		} else if parent, ok := dirs[parentPath]; ok {
			parent.Children = append(parent.Children, node)
		} else {
			// Якщо батька не знайдено (наприклад, через maxdepth), кидаємо в корінь
			topLevel = append(topLevel, node)
		}
	}

	return topLevel, nil
}

func (s *Service) ReadFile(ctx context.Context, containerID, filePath string) (string, error) {
	if err := validatePath(filePath); err != nil {
		return "", err
	}

	absPath := "/workspace/" + filePath

	tarStream, _, err := s.cli.CopyFromContainer(ctx, containerID, absPath)
	if err != nil {
		return "", fmt.Errorf("copy from container: %w", err)
	}
	defer tarStream.Close()

	tr := tar.NewReader(tarStream)
	if _, err := tr.Next(); err != nil {
		return "", fmt.Errorf("read tar header: %w", err)
	}

	content, err := io.ReadAll(io.LimitReader(tr, maxFileSize))
	if err != nil {
		return "", fmt.Errorf("read file content: %w", err)
	}

	return string(content), nil
}

func (s *Service) WriteFile(ctx context.Context, containerID, filePath, content string) error {
	if err := validatePath(filePath); err != nil {
		return err
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	hdr := &tar.Header{
		Name:    path.Base(filePath),
		Mode:    0644,
		Size:    int64(len(content)),
		ModTime: time.Now(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write tar header: %w", err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		return fmt.Errorf("write tar content: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar writer: %w", err)
	}

	destDir := "/workspace/" + path.Dir(filePath)
	if destDir == "/workspace/." {
		destDir = "/workspace"
	}

	if err := s.cli.CopyToContainer(ctx, containerID, destDir, &buf, container.CopyToContainerOptions{}); err != nil {
		return fmt.Errorf("copy to container: %w", err)
	}

	return nil
}

func (s *Service) SearchFiles(ctx context.Context, containerID, query string) ([]*SearchResult, error) {
	if query == "" {
		return []*SearchResult{}, nil
	}

	// Use grep with line numbers and case-insensitive search
	// Exclude hidden directories and node_modules
	cmd := []string{
		"sh", "-c",
		fmt.Sprintf("grep -rn -i --exclude-dir='.*' --exclude-dir='node_modules' '%s' . 2>/dev/null || true",
			escapeForShell(query)),
	}

	execResp, err := s.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          false,
		WorkingDir:   "/workspace",
	})
	if err != nil {
		return nil, fmt.Errorf("exec create: %w", err)
	}

	hijack, err := s.cli.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{
		Tty: false,
	})
	if err != nil {
		return nil, fmt.Errorf("exec attach: %w", err)
	}
	defer hijack.Close()

	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, hijack.Reader); err != nil {
		return nil, fmt.Errorf("read exec output: %w", err)
	}

	if stderr.Len() > 0 {
		log.Printf("[Search] stderr: %s", stderr.String())
	}

	return parseGrepOutput(stdout.String()), nil
}

func escapeForShell(s string) string {
	// Simple shell escaping - replace single quotes with '\''
	return strings.ReplaceAll(s, "'", "'\\''")
}

func parseGrepOutput(output string) []*SearchResult {
	results := make([]*SearchResult, 0)
	lines := strings.Split(output, "\n")

	for _, line := range lines {
		if line == "" {
			continue
		}

		// Format: ./path/to/file.txt:42:matching line content
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 3 {
			continue
		}

		filePath := strings.TrimPrefix(parts[0], "./")
		lineNumber := 0
		if n, err := fmt.Sscanf(parts[1], "%d", &lineNumber); err == nil && n == 1 {
			results = append(results, &SearchResult{
				File:   filePath,
				Line:   lineNumber,
				Column: 1, // grep doesn't provide column, default to 1
				Text:   strings.TrimSpace(parts[2]),
			})
		}
	}

	return results
}
