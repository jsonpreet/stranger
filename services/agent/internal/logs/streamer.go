package logs

import (
	"context"
	"io"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

type Streamer struct {
	cli *client.Client
}

func NewStreamer(cli *client.Client) *Streamer {
	return &Streamer{cli: cli}
}

func (s *Streamer) Stream(ctx context.Context, containerID string, w io.Writer) error {
	options := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Timestamps: true,
		Tail:       "100", // Start with last 100 lines
	}

	out, err := s.cli.ContainerLogs(ctx, containerID, options)
	if err != nil {
		return err
	}
	defer out.Close()

	// Use stdcopy to demultiplex Docker's multiplexed stdout/stderr stream.
	// io.Copy would include raw 8-byte frame headers causing garbage output.
	_, err = stdcopy.StdCopy(w, w, out)
	return err
}
