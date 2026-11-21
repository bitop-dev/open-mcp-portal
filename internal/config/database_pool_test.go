package config

import (
	"strings"
	"testing"
)

// The serving pool's size (issue #107): pgx's default, max(4, CPUs), let a
// few slow provider token refreshes (each holding a connection for up to
// 10 s) take every connection. DATABASE_MAX_CONNS sets it; unset, a
// pool_max_conns in DATABASE_URL is kept, else DefaultDatabaseMaxConns.
func TestDatabasePoolSize(t *testing.T) {
	c, err := LoadFrom("", env(map[string]string{"DATABASE_URL": "postgres://u:p@db:5432/omp?sslmode=require"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.DatabasePoolSize(); got != DefaultDatabaseMaxConns || got <= 4 {
		t.Fatalf("default pool size = %d, want %d (above pgx's floor of 4)", got, DefaultDatabaseMaxConns)
	}
	for value, want := range map[string]int{"4": 4, " 50 ": 50, "1000": 1000} {
		c, err := LoadFrom("", env(map[string]string{"DATABASE_MAX_CONNS": value, "DATABASE_URL": "postgres://u:p@db/omp?pool_max_conns=7"}))
		if err != nil || c.DatabaseMaxConns != want || c.DatabasePoolSize() != want {
			t.Errorf("%q = %d (pool %d) %v, want %d over the URL's pool_max_conns", value, c.DatabaseMaxConns, c.DatabasePoolSize(), err, want)
		}
	}
	for _, bad := range []string{"0", "3", "1001", "-1", "many", ""} {
		_, err := LoadFrom("", env(map[string]string{"DATABASE_MAX_CONNS": bad}))
		if err == nil || !strings.Contains(err.Error(), "DATABASE_MAX_CONNS") {
			t.Errorf("%q: err = %v, want one naming the setting", bad, err)
		}
	}
	for url, keep := range map[string]bool{
		"postgres://u:p@db/omp?sslmode=require&pool_max_conns=7": true,
		"postgresql://u:p@db/omp?pool_max_conns=7":               true,
		"host=db dbname=omp pool_max_conns=7":                    true,
		"pool_max_conns = 7 host=db":                             true,
		"postgres://u:pool_max_conns@db/omp?sslmode=require":     false,
		"host=db password=xpool_max_conns=1 dbname=omp":          false,
		"postgres://u:p@db/omp?sslmode=require&pool_min_conns=2": false,
	} {
		c := Config{DatabaseURL: url}
		if got := c.DatabasePoolSize(); (got == 0) != keep {
			t.Errorf("%q: pool size %d, keep the URL's own = %v", url, got, keep)
		}
	}
}
