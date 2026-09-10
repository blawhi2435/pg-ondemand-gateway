package pgwire

import (
	"strings"
	"testing"
)

func TestParseStartupMessage_Normal(t *testing.T) {
	payload := []byte("user\x00app_rw\x00database\x00orders\x00application_name\x00metabase\x00\x00")

	sm, err := ParseStartupMessage(payload)
	if err != nil {
		t.Fatalf("ParseStartupMessage: %v", err)
	}
	if sm.User != "app_rw" {
		t.Errorf("User = %q, want app_rw", sm.User)
	}
	if sm.Database != "orders" {
		t.Errorf("Database = %q, want orders", sm.Database)
	}
	if sm.ApplicationName != "metabase" {
		t.Errorf("ApplicationName = %q, want metabase", sm.ApplicationName)
	}
}

func TestParseStartupMessage_MissingDatabaseDefaultsToUser(t *testing.T) {
	payload := []byte("user\x00analyst_ro\x00\x00")

	sm, err := ParseStartupMessage(payload)
	if err != nil {
		t.Fatalf("ParseStartupMessage: %v", err)
	}
	if sm.Database != "analyst_ro" {
		t.Errorf("Database = %q, want it to default to user %q", sm.Database, sm.User)
	}
}

func TestParseStartupMessage_MissingUser(t *testing.T) {
	payload := []byte("database\x00orders\x00\x00")

	_, err := ParseStartupMessage(payload)
	if err == nil {
		t.Fatal("ParseStartupMessage: want error when user is missing")
	}
}

func TestParseStartupMessage_NotDoubleNullTerminated(t *testing.T) {
	payload := []byte("user\x00app_rw\x00") // missing the trailing extra \0

	_, err := ParseStartupMessage(payload)
	if err == nil {
		t.Fatal("ParseStartupMessage: want error for missing terminator")
	}
}

func TestParseStartupMessage_OddNumberOfStrings(t *testing.T) {
	// A key without a matching value before the terminator.
	payload := []byte("user\x00app_rw\x00database\x00\x00")

	_, err := ParseStartupMessage(payload)
	if err == nil {
		t.Fatal("ParseStartupMessage: want error for unpaired key/value")
	}
}

func TestParseStartupMessage_TooLong(t *testing.T) {
	huge := strings.Repeat("a", MaxStartupMessageLength+1)
	payload := []byte("user\x00" + huge + "\x00\x00")

	_, err := ParseStartupMessage(payload)
	if err == nil {
		t.Fatal("ParseStartupMessage: want error for oversized payload")
	}
}

func TestParseStartupMessage_DoesNotPanicOnGarbage(t *testing.T) {
	garbage := []byte{0xff, 0x00, 0x01, 0x02, 0xff, 0xff}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ParseStartupMessage panicked: %v", r)
		}
	}()
	_, _ = ParseStartupMessage(garbage)
}
