package main

import (
	"errors"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestSecurityCLIMapsAMissingItem checks the one assumption about security(1)
// the fake cannot: that a missing item exits 44 and reads as errItemMissing,
// not as a failure that would hide the "add it with" remedy. It asks for a
// service nobody has, so no secret is read.
func TestSecurityCLIMapsAMissingItem(t *testing.T) {
	if _, err := os.Stat("/usr/bin/security"); err != nil {
		t.Skip("no /usr/bin/security")
	}
	kc := securityCLI{user: "anthropic-account-test", timeout: 30 * time.Second}
	service := "anthropic-account-test-absent-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := kc.read(service); !errors.Is(err, errItemMissing) {
		t.Fatalf("read of an absent item: %v, want errItemMissing", err)
	}
	ok, err := kc.has(service)
	if err != nil || ok {
		t.Fatalf("has of an absent item: %v, %v", ok, err)
	}
}
