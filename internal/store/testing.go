package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestDB gives a test its own schema in the Postgres POSTGRES_DSN names, and
// skips when there is none.
//
// Its own schema rather than its own database: creating a database needs a
// connection to another one and a moment of waiting, and search_path buys
// the same isolation for the price of a SET. It is dropped on cleanup, so a
// failed run leaves nothing behind unless the process was killed.
//
// A skip rather than a fake. A fake Postgres would test the fake, and these
// tests exist because the interesting failures are constraint violations and
// the exact behaviour of an UPDATE that matches no rows.
func TestDB(t *testing.T) *DB {
	t.Helper()
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN is not set; run `mise run pg` and export what it prints")
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	schema := "tasksd_test_" + hex.EncodeToString(b[:])

	// `?` when the DSN carries no query string yet and `&` when it does.
	// Deciding this rather than trying both matters: a Postgres that is down
	// must fail as unreachable on the first attempt, not as an invalid
	// parameter on a second one made against a doubled separator.
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := Open(t.Context(), dsn+sep+"search_path="+schema)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	if _, err := db.pool.Exec(t.Context(),
		fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", schema)); err != nil {
		t.Fatalf("creating the schema: %v", err)
	}
	if _, err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.pool.Exec(context.Background(),
			fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", schema))
		db.Close()
	})
	return db
}
