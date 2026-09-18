package payment

import (
	"crypto/sha256"
	"crypto/subtle"
	"strings"
)

// BetaCode verifies the shared access code used instead of a payment provider
// during beta testing. Only the hash is retained by the adapter.
type BetaCode struct {
	digest [sha256.Size]byte
}

func NewBetaCode(code string) *BetaCode {
	return &BetaCode{digest: sha256.Sum256([]byte(strings.TrimSpace(code)))}
}

func (p *BetaCode) Verify(code string) bool {
	candidate := sha256.Sum256([]byte(strings.TrimSpace(code)))
	return subtle.ConstantTimeCompare(p.digest[:], candidate[:]) == 1
}
