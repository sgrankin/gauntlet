package workers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

type Peer struct {
	Online  bool      `json:"online"`
	Worker  Worker    `json:"worker"`
	Seen    time.Time `json:"seen"`
	Running int       `json:"running"`
}
type Demand struct {
	Profile      string       `json:"profile"`
	Requirements Requirements `json:"requirements"`
	Pending      int          `json:"pending"`
	Running      int          `json:"running"`
}
type Status struct {
	Workers []Peer   `json:"workers"`
	Demand  []Demand `json:"demand"`
}

func (b *Broker) Status(ctx context.Context) (Status, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var status Status
	rows, err := b.db.QueryContext(ctx, `SELECT data,seen,draining,(SELECT COUNT(*) FROM worker_jobs WHERE worker=worker_peers.id AND state='leased') FROM worker_peers ORDER BY id`)
	if err != nil {
		return status, err
	}
	for rows.Next() {
		var data []byte
		var seen int64
		var drain bool
		var peer Peer
		if err = rows.Scan(&data, &seen, &drain, &peer.Running); err != nil {
			break
		}
		if err = json.Unmarshal(data, &peer.Worker); err != nil {
			break
		}
		peer.Worker.Draining = peer.Worker.Draining || drain
		peer.Worker.Session = ""
		peer.Seen = time.UnixMilli(seen)
		peer.Online = b.now().Sub(peer.Seen) <= 2*b.lease
		status.Workers = append(status.Workers, peer)
	}
	readErr := rows.Err()
	rows.Close()
	if err != nil {
		return status, err
	}
	if readErr != nil {
		return status, readErr
	}
	rows, err = b.db.QueryContext(ctx, `SELECT json_extract(job,'$.Executor'),needs,state,COUNT(*) FROM worker_jobs WHERE state IN ('pending','leased') AND owner_epoch=? GROUP BY json_extract(job,'$.Executor'),needs,state ORDER BY 1,2,3`, b.epoch)
	if err != nil {
		return status, err
	}
	for rows.Next() {
		var demand Demand
		var data []byte
		var state string
		var count int
		if err = rows.Scan(&demand.Profile, &data, &state, &count); err != nil {
			break
		}
		if err = json.Unmarshal(data, &demand.Requirements); err != nil {
			break
		}
		if state == "pending" {
			demand.Pending = count
		} else {
			demand.Running = count
		}
		status.Demand = append(status.Demand, demand)
	}
	readErr = rows.Err()
	rows.Close()
	if err != nil {
		return status, err
	}
	return status, readErr
}

func (b *Broker) Drain(ctx context.Context, worker string, draining bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	result, err := b.db.ExecContext(ctx, `UPDATE worker_peers SET draining=? WHERE id=?`, draining, worker)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("unknown worker %q", worker)
	}
	b.notify()
	return nil
}

// Maintain retires jobs no reconstructed run adopted. A scheduler shutdown
// never invokes cancellation; the restart grace leaves workers time to return.
func (b *Broker) Maintain(ctx context.Context, orphanGrace, retention time.Duration) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if now.Sub(b.born) < b.grace {
		return nil
	}
	rows, err := b.db.QueryContext(ctx, `SELECT id,state,wanted FROM worker_jobs`)
	if err != nil {
		return err
	}
	var cancel, remove []string
	for rows.Next() {
		var id, state string
		var wanted int64
		if err = rows.Scan(&id, &state, &wanted); err != nil {
			break
		}
		if b.waiters[id] > 0 {
			continue
		}
		age := now.Sub(time.UnixMilli(wanted))
		if (state == "pending" || state == "leased") && age > orphanGrace {
			cancel = append(cancel, id)
		} else if age > retention {
			remove = append(remove, id)
		}
	}
	readErr := rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if readErr != nil {
		return readErr
	}
	for _, id := range cancel {
		if _, err = b.db.ExecContext(ctx, `UPDATE worker_jobs SET state='cancelled' WHERE id=?`, id); err != nil {
			return err
		}
	}
	for _, id := range remove {
		if _, err = b.db.ExecContext(ctx, `DELETE FROM worker_jobs WHERE id=?`, id); err != nil {
			return err
		}
	}
	_, err = b.db.ExecContext(ctx, `DELETE FROM worker_peers WHERE seen<? AND NOT EXISTS(SELECT 1 FROM worker_jobs WHERE worker=worker_peers.id AND state='leased')`, now.Add(-retention).UnixMilli())
	if len(cancel) > 0 {
		b.notify()
	}
	return err
}

func (b *Broker) cancelUnwatched(ctx context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.waiters[id] > 0 {
		return nil
	}
	_, err := b.db.ExecContext(ctx, `UPDATE worker_jobs SET state='cancelled' WHERE id=? AND state IN ('pending','leased')`, id)
	if err == nil {
		b.notify()
	}
	return err
}

// KeepRun retains already-authorized child work while an exact prepared graph
// restores its prerequisite results. No verdict or lease authority is changed.
func (b *Broker) KeepRun(ctx context.Context, runID, sha string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, err := b.db.ExecContext(ctx, `UPDATE worker_jobs SET wanted=?,owner_epoch=? WHERE json_extract(job,'$.RunID')=? AND json_extract(job,'$.MergeSHA')=? AND (owner_epoch!=? OR wanted<?)`, b.now().UnixMilli(), b.epoch, runID, sha, b.epoch, b.now().Add(-30*time.Second).UnixMilli())
	if err == nil {
		b.notify()
	}
	return err
}
