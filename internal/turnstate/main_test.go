package turnstate

import (
	"os"
	"testing"
)

// Fixtures predate the 240-second shipped lifetime and assume one hour.
func TestMain(m *testing.M) {
	shippedTTLSeconds, shippedRenewBeforeMinutes = 3600, 0
	os.Exit(m.Run())
}
