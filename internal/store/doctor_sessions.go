package store

import (
	"context"
	"database/sql"

	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/sqliteutil"
)

// ReadDoctorSessions supplies both the store owner and an offline CLI holding
// the startup lock. The shared query keeps their session output identical.
func ReadDoctorSessions(ctx context.Context, db *sql.DB) ([]protocol.DoctorSession, error) {
	var rows *sql.Rows
	err := sqliteutil.Retry(func() error {
		var queryErr error
		rows, queryErr = db.QueryContext(ctx, `SELECT a.id,a.desired,a.observed,a.tool,a.variant,COALESCE(a.launcher_id,''),a.generation,c.canonical_path,machine.is_host=0,COALESCE(l.containerized,0),
			COALESCE((SELECT m.room_id FROM room_member m WHERE m.member_kind='agent' AND m.member_id=a.id AND m.left_at IS NULL ORDER BY m.joined_at,m.room_id LIMIT 1),''),
			COALESCE((SELECT t.id FROM turn t WHERE t.agent_id=a.id ORDER BY COALESCE(t.started_at,'' ) DESC,t.id DESC LIMIT 1),''),
			COALESCE((SELECT t.started_at FROM turn t WHERE t.agent_id=a.id ORDER BY COALESCE(t.started_at,'' ) DESC,t.id DESC LIMIT 1),''),
			COALESCE(s.native_session_id,'')
			FROM agent a JOIN checkout c ON c.id=a.checkout_id JOIN machine ON machine.id=a.machine_id
			LEFT JOIN machine_launcher l ON l.id=a.launcher_id AND l.machine_id=a.machine_id
			LEFT JOIN session s ON s.agent_id=a.id AND s.generation=a.generation AND s.ended_at IS NULL AND s.native_session_started_at IS NOT NULL
			ORDER BY a.id`)
		return queryErr
	})
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []protocol.DoctorSession{}
	for rows.Next() {
		var row protocol.DoctorSession
		if err := rows.Scan(&row.ID, &row.Desired, &row.Observed, &row.Tool, &row.Variant, &row.Launcher, &row.Generation, &row.Checkout, &row.Remote, &row.Containerized, &row.Room, &row.LastTurn, &row.When, &row.NativeSessionID); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
