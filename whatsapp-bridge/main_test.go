package main

import (
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
)

// ── path sanitization (traversal impossible) ────────────────────────────────

func TestSanitizeMediaFilename(t *testing.T) {
	cases := map[string]string{
		"invoice.pdf":               "invoice.pdf",
		"../../../../etc/passwd":    "passwd",
		"..\\..\\windows\\system32": "system32",
		"a/b/c.jpg":                 "c.jpg",
		"/absolute/path/x.ogg":      "x.ogg",
		"..":                        "", // -> generated
		".":                         "", // -> generated
		"":                          "", // -> generated
		"MD-Robiul islam 28-08.pdf": "MD-Robiul islam 28-08.pdf",
	}
	for in, want := range cases {
		got := sanitizeMediaFilename(in)
		if strings.ContainsAny(got, "/\\") || got == ".." || got == "." {
			t.Fatalf("sanitizeMediaFilename(%q) = %q — still contains a path component", in, got)
		}
		if want != "" && got != want {
			t.Fatalf("sanitizeMediaFilename(%q) = %q, want %q", in, got, want)
		}
		if want == "" && !strings.HasPrefix(got, "media_") {
			t.Fatalf("sanitizeMediaFilename(%q) = %q, want a generated media_* name", in, got)
		}
	}
}

// ── legacy URL fallback still parses ────────────────────────────────────────

func TestExtractDirectPathFromURL(t *testing.T) {
	got := extractDirectPathFromURL("https://mmg.whatsapp.net/v/t62.7118-24/13812002_698_n.enc?ccb=11-4&oh=abc")
	if got != "/v/t62.7118-24/13812002_698_n.enc" {
		t.Fatalf("got %q", got)
	}
	// no ".net/" -> returned unchanged (whatsmeow will then reject it, which is fine for legacy rows)
	if extractDirectPathFromURL("garbage") != "garbage" {
		t.Fatal("unexpected parse of non-URL")
	}
}

// ── MediaDownloader prefers direct_path ─────────────────────────────────────

func TestMediaDownloaderInterface(t *testing.T) {
	d := &MediaDownloader{URL: "https://x/y.enc", DirectPath: "/v/real/path.enc", MediaType: whatsmeow.MediaAudio}
	if d.GetDirectPath() != "/v/real/path.enc" {
		t.Fatalf("GetDirectPath = %q", d.GetDirectPath())
	}
	if d.GetURL() != "https://x/y.enc" {
		t.Fatal("GetURL mismatch")
	}
}

// ── migration: idempotent, additive; store/read round-trip ─────────────────

func withStore(t *testing.T) *MessageStore {
	t.Helper()
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	s, err := NewMessageStore()
	if err != nil {
		t.Fatalf("NewMessageStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func hasCol(t *testing.T, db *sql.DB, col string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = ?", col).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func TestMigration_FreshStoreHasDirectPath(t *testing.T) {
	s := withStore(t)
	if !hasCol(t, s.db, "direct_path") {
		t.Fatal("fresh store: messages.direct_path missing")
	}
}

func TestMigration_IdempotentAndAdditiveOnLegacyStore(t *testing.T) {
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	os.Chdir(dir)
	os.MkdirAll("store", 0755)

	// Build a pre-migration (stock 13-column) messages.db with a real row.
	db, err := sql.Open("sqlite3", "file:store/messages.db?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP);
		CREATE TABLE messages (
			id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP,
			is_from_me BOOLEAN, media_type TEXT, filename TEXT, url TEXT,
			media_key BLOB, file_sha256 BLOB, file_enc_sha256 BLOB, file_length INTEGER,
			PRIMARY KEY (id, chat_jid), FOREIGN KEY (chat_jid) REFERENCES chats(jid));
		INSERT INTO chats VALUES ('c@s.whatsapp.net','C',CURRENT_TIMESTAMP);
		INSERT INTO messages (id,chat_jid,sender,content,media_type) VALUES ('OLD1','c@s.whatsapp.net','c','legacy text','');
	`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	// First open: migration runs.
	s1, err := NewMessageStore()
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	if !hasCol(t, s1.db, "direct_path") {
		t.Fatal("legacy store: direct_path not added")
	}
	var content string
	if err := s1.db.QueryRow("SELECT content FROM messages WHERE id='OLD1'").Scan(&content); err != nil || content != "legacy text" {
		t.Fatalf("legacy row disturbed: %q %v", content, err)
	}
	var dp sql.NullString
	if err := s1.db.QueryRow("SELECT direct_path FROM messages WHERE id='OLD1'").Scan(&dp); err != nil {
		t.Fatal(err)
	}
	if dp.Valid {
		t.Fatal("legacy row direct_path should be NULL")
	}
	s1.Close()

	// Second open: must be a no-op (idempotent), no error.
	s2, err := NewMessageStore()
	if err != nil {
		t.Fatalf("open 2 (idempotent): %v", err)
	}
	if !hasCol(t, s2.db, "direct_path") {
		t.Fatal("direct_path missing after 2nd open")
	}
	s2.Close()
}

func TestStoreAndGetMediaInfo_DirectPathRoundTrip(t *testing.T) {
	s := withStore(t)
	if err := s.StoreChat("c@s.whatsapp.net", "C", time.Now()); err != nil {
		t.Fatal(err)
	}
	key := []byte("k")
	err := s.StoreMessage("M1", "c@s.whatsapp.net", "c", "", time.Now(), false,
		"audio", "a.ogg", "https://legacy/url.enc", "/v/t62.real/path.enc",
		key, []byte("sha"), []byte("enc"), 1234, false)
	if err != nil {
		t.Fatal(err)
	}
	mt, fn, url, dp, mk, _, _, fl, err := s.GetMediaInfo("M1", "c@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	if mt != "audio" || fn != "a.ogg" || url != "https://legacy/url.enc" || dp != "/v/t62.real/path.enc" || string(mk) != "k" || fl != 1234 {
		t.Fatalf("round-trip mismatch: mt=%q fn=%q url=%q dp=%q mk=%q fl=%d", mt, fn, url, dp, mk, fl)
	}

	// Row with NO direct_path (legacy) -> GetMediaInfo returns "" for it, no scan error.
	err = s.StoreMessage("M2", "c@s.whatsapp.net", "c", "", time.Now(), false,
		"image", "i.jpg", "https://legacy/only-url.enc", "",
		key, []byte("sha"), []byte("enc"), 55, false)
	if err != nil {
		t.Fatal(err)
	}
	_, _, url2, dp2, _, _, _, _, err := s.GetMediaInfo("M2", "c@s.whatsapp.net")
	if err != nil {
		t.Fatalf("legacy-shaped row GetMediaInfo error: %v", err)
	}
	if dp2 != "" || url2 != "https://legacy/only-url.enc" {
		t.Fatalf("legacy row: dp=%q url=%q", dp2, url2)
	}
}

// ── from_history_sync flag (BRIDGE3_FIRST_REPAIR_INCIDENT_2026-09-02) ────────

func TestMigration_FreshStoreHasHistoryFlag(t *testing.T) {
	s := withStore(t)
	defer s.Close()
	if !hasCol(t, s.db, "from_history_sync") {
		t.Fatal("fresh store: messages.from_history_sync missing")
	}
	// default must be 0 for a live-path insert
	if err := s.StoreChat("c@s.whatsapp.net", "C", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreMessage("L1", "c@s.whatsapp.net", "c", "hi", time.Now(), false,
		"", "", "", "", nil, nil, nil, 0, false); err != nil {
		t.Fatal(err)
	}
	var flag int
	if err := s.db.QueryRow("SELECT from_history_sync FROM messages WHERE id='L1'").Scan(&flag); err != nil {
		t.Fatal(err)
	}
	if flag != 0 {
		t.Fatalf("live message from_history_sync = %d, want 0", flag)
	}
}

func TestStoreMessage_HistorySyncFlagAndNoClobber(t *testing.T) {
	s := withStore(t)
	defer s.Close()
	if err := s.StoreChat("c@s.whatsapp.net", "C", time.Now()); err != nil {
		t.Fatal(err)
	}
	ts := time.Now()

	// history-sync insert -> flag 1
	if err := s.StoreMessage("H1", "c@s.whatsapp.net", "c", "old history text", ts, false,
		"", "", "", "", nil, nil, nil, 0, true); err != nil {
		t.Fatal(err)
	}
	var flag int
	var content string
	if err := s.db.QueryRow("SELECT from_history_sync, content FROM messages WHERE id='H1'").Scan(&flag, &content); err != nil {
		t.Fatal(err)
	}
	if flag != 1 || content != "old history text" {
		t.Fatalf("history row: flag=%d content=%q", flag, content)
	}

	// a LIVE message arrives, same id/chat -> REPLACE, becomes flag 0
	if err := s.StoreMessage("H1", "c@s.whatsapp.net", "c", "live version", ts, false,
		"", "", "", "", nil, nil, nil, 0, false); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT from_history_sync, content FROM messages WHERE id='H1'").Scan(&flag, &content); err != nil {
		t.Fatal(err)
	}
	if flag != 0 || content != "live version" {
		t.Fatalf("after live replace: flag=%d content=%q, want flag=0 content=\"live version\"", flag, content)
	}

	// history sync re-delivers the same id -> must NOT clobber the live row back to flag 1
	if err := s.StoreMessage("H1", "c@s.whatsapp.net", "c", "history again", ts, false,
		"", "", "", "", nil, nil, nil, 0, true); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT from_history_sync, content FROM messages WHERE id='H1'").Scan(&flag, &content); err != nil {
		t.Fatal(err)
	}
	if flag != 0 || content != "live version" {
		t.Fatalf("history re-delivery clobbered live row: flag=%d content=%q", flag, content)
	}
}
