package framework

import (
	"errors"
	"fmt"
)

// Blocked is a condition that stops a case running here without being a defect
// in the thing under test: a tool the image does not carry, a mount option that
// makes the thing being measured local, a binary nobody built for this node's
// architecture.
//
// It is neither a pass nor a failure. The distinction has to survive the trip
// back to the case, because a helper that returned an ordinary error for it
// would be turned into a failure by every caller, and a suite that reports "no
// locktool for arm64" as a storage defect wastes the time of whoever triages it.
//
// Every Blocked names the probe that failed and the flag, option or command
// that would fix it.
type Blocked struct{ Reason string }

func (b *Blocked) Error() string { return b.Reason }

// Blockedf builds a blocked condition.
func Blockedf(format string, args ...any) error {
	return &Blocked{Reason: fmt.Sprintf(format, args...)}
}

// IsBlocked reports whether err is a blocked condition anywhere in its chain,
// so a helper that wrapped one on the way back still reads as blocked.
func IsBlocked(err error) bool {
	var b *Blocked
	return errors.As(err, &b)
}
