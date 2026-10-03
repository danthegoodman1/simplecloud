package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// runIfPresent runs a command only if it exists, so an optional tool missing from
// an application image is reported rather than fatal.
func runIfPresent(ctx context.Context, name string, args ...string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s is not in this image", name)
	}
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(out), nil
}
