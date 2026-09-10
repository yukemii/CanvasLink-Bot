// Package testsupport provisions isolated database schemas for integration tests.
package testsupport

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/markadodo/canvaslink/internal/store"
	"net/url"
	"os"
	"testing"
	"time"
)

func Store(t *testing.T) *store.Store {
	t.Helper()
	base := os.Getenv("CANVASLINK_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("requires PostgreSQL")
	}
	db, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("planner_test_%d", time.Now().UnixNano())
	if _, err = db.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); db.Close() })
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	s, err := store.Connect(u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.InitSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}
