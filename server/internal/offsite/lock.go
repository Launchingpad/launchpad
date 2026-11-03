package offsite

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// destLock is the advisory lock of one destination (decisions-T16 #31 step 1). It is
// held on a DEDICATED connection that is not part of the pool: pg_try_advisory_lock
// is session scoped, so a pooled connection would go back to the pool with the lock
// still held, or be closed and lose it. Closing the connection frees the lock, so a
// crashed worker frees it too.
type destLock struct{ conn *pgx.Conn }

// lockKey maps a destination id to the bigint key of its advisory lock.
func lockKey(id uuid.UUID) int64 {
	sum := sha256.Sum256([]byte("launchpad-backup-destination:" + id.String()))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

// tryLock takes the lock of a destination. ok is false when another session holds it.
func (w *Worker) tryLock(ctx context.Context, id uuid.UUID) (*destLock, bool, error) {
	conn, err := pgx.Connect(ctx, w.o.DSN)
	if err != nil {
		return nil, false, err
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lockKey(id)).Scan(&got); err != nil {
		closeConn(conn)
		return nil, false, err
	}
	if !got {
		closeConn(conn)
		return nil, false, nil
	}
	return &destLock{conn: conn}, true, nil
}

// release frees the lock by closing its connection.
func (l *destLock) release() {
	if l == nil || l.conn == nil {
		return
	}
	closeConn(l.conn)
	l.conn = nil
}

func closeConn(c *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.Close(ctx)
}

// listen keeps one connection LISTENing on backup_work and wakes the scan loop
// (decisions-T16 #30). A lost connection is made again after a pause; the 30
// second scan is the safety net, so a missed notification only costs time.
func (w *Worker) listen(ctx context.Context, wake chan<- struct{}) {
	for ctx.Err() == nil {
		if err := w.listenOnce(ctx, wake); err != nil && ctx.Err() == nil {
			w.log.Warn("offsite: the LISTEN connection was lost; the scan continues", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (w *Worker) listenOnce(ctx context.Context, wake chan<- struct{}) error {
	conn, err := pgx.Connect(ctx, w.o.DSN)
	if err != nil {
		return err
	}
	defer closeConn(conn)
	if _, err := conn.Exec(ctx, `LISTEN backup_work`); err != nil {
		return err
	}
	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return err
		}
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}
