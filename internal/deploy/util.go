package deploy

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/danthegoodman1/simplecloud/internal/archil"
)

type archilRunOpts = archil.RunOptions

func sha256Sum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
