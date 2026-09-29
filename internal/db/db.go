// Package db opens a gallery's SQLite pools and owns its schema.
package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	_ "embed"
	"fmt"
	"math/bits"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
)

// Both separators: a Windows library's canonical_path has backslashes.
// Indexes store the result, so changing it needs a REINDEX migration.
func basenameSQL(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	if len(args) != 1 || args[0] == nil {
		return nil, nil
	}
	s, ok := args[0].(string)
	if !ok {
		return nil, nil
	}
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		return s[i+1:], nil
	}
	return s, nil
}

func hammingDistSQL(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	if len(args) != 2 || args[0] == nil || args[1] == nil {
		return nil, nil
	}
	a, aOk := args[0].(int64)
	b, bOk := args[1].(int64)
	if !aOk || !bOk {
		return nil, nil
	}
	return int64(bits.OnesCount64(uint64(a) ^ uint64(b))), nil
}

func randomKeySQL(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	if len(args) != 2 || args[0] == nil || args[1] == nil {
		return nil, nil
	}
	id, aOk := args[0].(int64)
	seed, bOk := args[1].(int64)
	if !aOk || !bOk {
		return nil, nil
	}
	return int64(RandomSortKey(id, seed)), nil
}

// RandomSortKey is random_key() in Go: a SplitMix64 mix of (id, seed).
func RandomSortKey(id, seed int64) uint64 {
	x := uint64(id)*0x9E3779B97F4A7C15 ^ uint64(seed)*0xBF58476D1CE4E5B9
	x ^= x >> 30
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 27
	x *= 0x94D049BB133111EB
	x ^= x >> 31
	return x & 0x7FFFFFFFFFFFFFFF
}

func init() {
	sqlite.MustRegisterDeterministicScalarFunction("basename", 1, basenameSQL)
	sqlite.MustRegisterDeterministicScalarFunction("hammingdist", 2, hammingDistSQL)
	sqlite.MustRegisterDeterministicScalarFunction("random_key", 2, randomKeySQL)
}

//go:embed schema.sql
var schemaSQL string

// NormalizeWindowsFolderPathSQL skips any row whose canonical_path has a
// "/": on POSIX a backslash in a name is literal.
const NormalizeWindowsFolderPathSQL = `UPDATE images SET folder_path = ltrim(replace(folder_path, '\', '/'), '/')
	WHERE instr(folder_path, '\') > 0
	  AND instr(canonical_path, '\') > 0
	  AND instr(canonical_path, '/') = 0`

// Bump bootstrapSchemaVersion for a new one-shot or an index the planner
// needs stats for: ANALYZE runs once per bump.
const bootstrapSchemaVersion = 14

// WAL serialises writers, so Write holds one connection and Read a pool.
type DB struct {
	Read  *sql.DB
	Write *sql.DB
}

func Open(path string) (*DB, error) {
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(on)" +
		"&_pragma=journal_mode(wal)" +
		"&_pragma=synchronous(normal)" +
		"&_pragma=cache_size(-1024)" +
		"&_pragma=temp_store(memory)" +
		"&_pragma=mmap_size(67108864)"

	rd, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening read pool: %w", err)
	}
	rd.SetMaxOpenConns(8)
	rd.SetMaxIdleConns(8)
	rd.SetConnMaxIdleTime(5 * time.Minute)

	wr, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = rd.Close()
		return nil, fmt.Errorf("opening write pool: %w", err)
	}
	wr.SetMaxOpenConns(1)
	wr.SetMaxIdleConns(1)
	wr.SetConnMaxIdleTime(5 * time.Minute)

	db := &DB{Read: rd, Write: wr}

	if err := rd.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pinging read pool: %w", err)
	}
	if err := wr.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pinging write pool: %w", err)
	}

	return db, nil
}

// Each connection has its own page cache, so ShrinkMemory holds them all
// at once to reach every one.
func (db *DB) ShrinkMemory(ctx context.Context) error {
	if err := shrinkPool(ctx, db.Read); err != nil {
		return err
	}
	return shrinkPool(ctx, db.Write)
}

func shrinkPool(ctx context.Context, pool *sql.DB) error {
	n := pool.Stats().MaxOpenConnections
	if n <= 0 {
		n = 1
	}
	conns := make([]*sql.Conn, 0, n)
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for i := 0; i < n; i++ {
		c, err := pool.Conn(ctx)
		if err != nil {
			return err
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		if _, err := c.ExecContext(ctx, `PRAGMA shrink_memory`); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) Close() error {
	var firstErr error
	if err := db.Read.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := db.Write.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}
