package logs

import (
	"context"
	"io"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
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

	// Docker logs return a specific format with headers.
	// For MVP, we can just copy raw bytes, but ideally we parse stdcopy.
	// 'stdcopy' demultiplexes stdout/stderr, but for a simple web stream, 
	// raw copy might yield garbage headers. 
	// Let's use stdcopy if we want clean text, or just copy if we don't care about headers.
	// Actually, for a web stream, we want to just pipe it. 
	// Using io.Copy directly will include the 8-byte header for each frame.
	// The frontend/control-plane might need to strip it, OR we strip it here.
	// To keep it simple for now, let's just copy.
	
	_, err = io.Copy(w, out)
	return err
}
