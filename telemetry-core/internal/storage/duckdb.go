package storage

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"

	_ "github.com/marcboeker/go-duckdb"
	"github.com/rs/zerolog/log"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/state"
)

// Storage manages a single DuckDB *sql.DB pool, the batch buffer system,
// and the in-memory LiveCache for the latest RaceState snapshot.
//
// Why a single pool instead of writer+reader connections:
//
//	DuckDB's `?access_mode=READ_ONLY` mode opens a catalog+data snapshot
//	at connect time and never sees subsequent writer commits from another
//	connection in the same process. We tried it — it silently froze every
//	SQL-backed feature (analyst, lap-summary competitor blurb, /api/query,
//	/api/state/*) at whatever data existed when the reader was opened.
//
//	With one *sql.DB pool whose connections all hit the same DuckDB file,
//	DuckDB's MVCC handles read/write concurrency at the engine level —
//	long-running analyst queries don't block lap_data INSERTs, and reads
//	always see the latest committed state.
//
// "Reader()" is kept as a method so callers don't have to migrate; it
// returns the same *sql.DB. Treat it as read-intent by convention.
type Storage struct {
	writer  *sql.DB
	buffers *BufferSet
	cache   *LiveCache
	roster  *state.Roster
	dbPath  string
}

// NewStorage opens the DuckDB pool at dbPath, initialises the schema (all
// 10 tables), and creates the batch buffer set with the requested batchSize.
func NewStorage(dbPath string, batchSize int) (*Storage, error) {
	writer, err := sql.Open("duckdb", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// Pool size 4: enough headroom for the slow analyst query, the
	// lap-summary engine tick, the /api/state/* HTTP handlers, and the
	// background buffer flusher to all run without queuing on each other.
	// DuckDB serialises actual writes internally; the pool size only
	// caps Go-side concurrency.
	writer.SetMaxOpenConns(4)

	if err := InitSchema(writer); err != nil {
		writer.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}

	s := &Storage{
		writer:  writer,
		buffers: NewBufferSet(writer, batchSize),
		cache:   NewLiveCache(),
		roster:  state.NewRoster(),
		dbPath:  dbPath,
	}

	log.Info().Str("path", dbPath).Int("batch", batchSize).Msg("storage initialised")
	return s, nil
}

// Writer returns the DuckDB connection used for INSERTs.
func (s *Storage) Writer() *sql.DB { return s.writer }

// Reader returns the same DuckDB connection used for writes — see the
// Storage type doc for why we don't run a separate read-only connection.
// Callers should still treat this as read-intent (SELECT only).
func (s *Storage) Reader() *sql.DB { return s.writer }

// Buffers returns the set of table batch buffers.
func (s *Storage) Buffers() *BufferSet { return s.buffers }

// Cache returns the in-memory LiveCache holding the latest RaceState.
func (s *Storage) Cache() *LiveCache { return s.cache }

// Roster returns the live driver roster (car_index → name) populated from
// Participants packets. Always non-nil; an empty roster simply has no
// entries until F1 sends the first Participants packet.
func (s *Storage) Roster() *state.Roster { return s.roster }

// Query executes a read-only SQL statement and returns the rows as a slice
// of column-name-keyed maps. This powers /api/query plus the topic-clustered
// state endpoints (/api/state/competitors, /api/state/pace, /api/state/events).
//
// Flushes buffers first so the query observes the latest writes (the buffer
// holds rows in memory until BATCH_SIZE or the 1s periodic flush ticker).
func (s *Storage) Query(ctx context.Context, query string) ([]map[string]interface{}, error) {
	if err := s.buffers.FlushAll(ctx); err != nil {
		log.Warn().Err(err).Msg("flush before query failed, results may be stale")
	}

	rows, err := s.writer.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("columns: %w", err)
	}

	var results []map[string]interface{}
	for rows.Next() {
		// Create a slice of interface{} pointers for Scan.
		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		row := make(map[string]interface{}, len(cols))
		for i, col := range cols {
			row[col] = vals[i]
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration: %w", err)
	}
	return results, nil
}

// FlushAll forces all batch buffers to write their pending rows to DuckDB.
func (s *Storage) FlushAll(ctx context.Context) error {
	return s.buffers.FlushAll(ctx)
}

// UpsertSessionHistory writes lap-history rows keyed on
// (session_uid, car_index, lap_num). The SessionHistory packet rebroadcasts
// the full per-car history every few seconds; the unique index lets us
// resend the same rows without growing the table — newer sector splits
// overwrite older partials so the freshest numbers always win.
func (s *Storage) UpsertSessionHistory(rows []SessionHistoryRow) error {
	if len(rows) == 0 {
		return nil
	}
	const stmt = `INSERT INTO session_history
		(session_uid, car_index, lap_num, lap_time_in_ms,
		 sector1_time_in_ms, sector2_time_in_ms, sector3_time_in_ms, lap_valid)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (session_uid, car_index, lap_num) DO UPDATE SET
		  lap_time_in_ms = EXCLUDED.lap_time_in_ms,
		  sector1_time_in_ms = EXCLUDED.sector1_time_in_ms,
		  sector2_time_in_ms = EXCLUDED.sector2_time_in_ms,
		  sector3_time_in_ms = EXCLUDED.sector3_time_in_ms,
		  lap_valid = EXCLUDED.lap_valid`
	tx, err := s.writer.Begin()
	if err != nil {
		return fmt.Errorf("session_history begin: %w", err)
	}
	defer tx.Rollback()
	p, err := tx.Prepare(stmt)
	if err != nil {
		return fmt.Errorf("session_history prepare: %w", err)
	}
	defer p.Close()
	uid := new(big.Int)
	for _, r := range rows {
		// F1 session UIDs are random 64-bit; values with the high bit set
		// trip database/sql's "uint64 high bit not supported" check, and
		// bit-casting to int64 fails DuckDB's UBIGINT range check. The
		// go-duckdb driver whitelists *big.Int via CheckNamedValue and
		// binds it as HUGEINT, which DuckDB casts to UBIGINT cleanly for
		// the full uint64 range.
		uid.SetUint64(r.SessionUID)
		if _, err := p.Exec(uid, r.CarIndex, r.LapNum, r.LapTimeMs,
			r.S1Ms, r.S2Ms, r.S3Ms, r.LapValid); err != nil {
			return fmt.Errorf("session_history upsert: %w", err)
		}
	}
	return tx.Commit()
}

// Close flushes all pending data and closes both database connections.
func (s *Storage) Close() error {
	// Best-effort flush before closing.
	if err := s.buffers.FlushAll(context.Background()); err != nil {
		log.Error().Err(err).Msg("flush on close failed")
	}

	if err := s.writer.Close(); err != nil {
		return fmt.Errorf("close writer: %w", err)
	}
	return nil
}
