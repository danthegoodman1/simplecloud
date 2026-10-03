// Package agentbin carries the agent binary that the CLI uploads into sandboxes.
//
// Embedding it means there is one binary to install, and the agent can never be a
// different version from the CLI that deployed it.
package agentbin

import (
	"bytes"
	"compress/gzip"
	"embed"
	"fmt"
	"io"
	"os"
)

//go:embed scagent.gz
var packed embed.FS

// Bytes returns the agent binary, built for linux/amd64.
//
// SC_AGENT_BINARY overrides it, which is what makes iterating on the agent
// possible without rebuilding the CLI.
func Bytes() ([]byte, error) {
	if path := os.Getenv("SC_AGENT_BINARY"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading SC_AGENT_BINARY %s: %w", path, err)
		}
		return raw, nil
	}
	gz, err := packed.Open("scagent.gz")
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	zr, err := gzip.NewReader(gz)
	if err != nil {
		return nil, fmt.Errorf("the embedded agent is unreadable: %w", err)
	}
	defer zr.Close()
	var out bytes.Buffer
	if _, err := io.Copy(&out, zr); err != nil {
		return nil, err
	}
	if out.Len() < 1<<20 {
		return nil, fmt.Errorf("the embedded agent is only %d bytes; run scripts/build.sh to rebuild it", out.Len())
	}
	return out.Bytes(), nil
}
