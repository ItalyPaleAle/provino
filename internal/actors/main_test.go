//go:build unit

package actors

import (
	"os"
	"testing"
	"time"
)

// Run the tests in a local zone other than UTC, so a time that reaches a manifest, an event, or a status without being converted to UTC fails them, even on a machine set to UTC
func TestMain(m *testing.M) {
	time.Local = time.FixedZone("UTC+05:30", 5*60*60+30*60) //nolint:gosmopolitan // Changing the local zone is the point, and nothing runs yet that could read it
	os.Exit(m.Run())
}
