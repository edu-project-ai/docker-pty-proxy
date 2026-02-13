package docker

import (
	"context"
	"fmt"

	"github.com/docker/docker/client"
)

func New() (*client.Client, error) {
	cli, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("docker client init: %w", err)
	}
	return cli, nil
}

func Ping(ctx context.Context, cli *client.Client) error {
	_, err := cli.Ping(ctx)
	if err != nil {
		return fmt.Errorf("docker ping: %w", err)
	}
	return nil
}
