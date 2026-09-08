package introspect

import (
	"context"
	"fmt"
)

// Capacity is how much connection headroom an instance has.
//
// The number that matters is Free, not Max. A server configured for five
// hundred connections with four hundred and eighty in use has twenty spare, and
// consuming a hundred of them would break the application it exists to serve
// rather than anything of ours.
type Capacity struct {
	Max      int `json:"max"`
	Reserved int `json:"reserved"`
	Used     int `json:"used"`
}

// Free reports the connections available to us, never below zero.
func (c Capacity) Free() int {
	free := c.Max - c.Reserved - c.Used
	if free < 0 {
		return 0
	}
	return free
}

// SampleCapacity reads an instance's connection limits and current usage.
//
// Counting pg_stat_activity needs no special privilege: an unprivileged role
// sees a row for every backend, with the details of other users' sessions
// hidden. It cannot see what they are doing, which is all we need — only how
// many there are.
func SampleCapacity(ctx context.Context, q Querier) (*Capacity, error) {
	rows, err := q.Query(ctx, `
		SELECT current_setting('max_connections')::int,
		       current_setting('superuser_reserved_connections')::int,
		       (SELECT count(*) FROM pg_stat_activity)::int`)
	if err != nil {
		return nil, fmt.Errorf("sample connection capacity: %w", err)
	}
	defer rows.Close()

	var c Capacity
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("sample connection capacity: no row returned")
	}
	if err := rows.Scan(&c.Max, &c.Reserved, &c.Used); err != nil {
		return nil, fmt.Errorf("scan connection capacity: %w", err)
	}
	return &c, rows.Err()
}
