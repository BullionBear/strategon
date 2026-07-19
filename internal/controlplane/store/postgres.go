package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"sync"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/artifacturi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Postgres is a Store backed by PostgreSQL. It mirrors Memory's semantics
// (per-machine monotonic generation, spec/status write ownership, empty-target
// rollback via previous_artifacts) with durable storage, so the control plane
// can restart without losing desired state, status, catalog, audit, or leases.
type Postgres struct {
	pool        *pgxpool.Pool
	hub         *Hub
	leaseMu     sync.Mutex // guards leaseMargin only
	leaseMargin time.Duration
	now         func() time.Time
}

// querier is satisfied by both *pgxpool.Pool and pgx.Tx, so read helpers work
// inside or outside a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// NewPostgres connects to dsn, runs pending migrations, and returns a Store.
// hub may be nil (no WatchMachine fan-out). Call Close to release the pool.
func NewPostgres(ctx context.Context, dsn string, hub *Hub) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	p := &Postgres{
		pool:        pool,
		hub:         hub,
		leaseMargin: DefaultLeaseMarginCP,
		now:         time.Now,
	}
	if err := p.migrate(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: migrate: %w", err)
	}
	return p, nil
}

// Close releases the connection pool.
func (p *Postgres) Close() { p.pool.Close() }

// migrate applies every embedded migration not yet recorded in
// schema_migrations, each in its own transaction, in filename order.
func (p *Postgres) migrate(ctx context.Context) error {
	if _, err := p.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // 0001_, 0002_, ...
	for _, name := range names {
		var exists bool
		if err := p.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		sqlBytes, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if err := p.inTx(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
				return fmt.Errorf("apply %s: %w", name, err)
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, name)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

// inTx runs fn in a transaction, committing on success and rolling back on error.
func (p *Postgres) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func opCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

func (p *Postgres) notify(machineID string) {
	if p.hub != nil && machineID != "" {
		p.hub.Notify(machineID)
	}
}

// --- machine reads ---

// loadMachine assembles a full MachineRecord (machine row + assignments +
// statuses + previous artifacts). ok=false when the machine row is absent.
func loadMachine(ctx context.Context, q querier, id string) (*MachineRecord, bool, error) {
	rec := &MachineRecord{
		MachineID:         id,
		Assignments:       map[string]*pb.StrategyAssignmentSpec{},
		Status:            map[string]*pb.StrategyAssignmentStatus{},
		PreviousArtifacts: map[string]*pb.ArtifactRef{},
	}
	var register, resources []byte
	err := q.QueryRow(ctx, `SELECT register, reachable, agent_version, agent_build_version,
		last_resources, last_heartbeat, generation, observed_gen FROM machines WHERE machine_id=$1`, id).
		Scan(&register, &rec.Reachable, &rec.AgentVersion, &rec.AgentBuildVersion, &resources,
			&rec.LastHeartbeat, &rec.Generation, &rec.ObservedGen)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if register != nil {
		rec.Register = &pb.Register{}
		if err := proto.Unmarshal(register, rec.Register); err != nil {
			return nil, false, err
		}
	}
	if resources != nil {
		rec.LastResources = &pb.MachineResources{}
		if err := proto.Unmarshal(resources, rec.LastResources); err != nil {
			return nil, false, err
		}
	}
	if err := loadInto(ctx, q, `SELECT strategy, spec FROM assignments WHERE machine_id=$1`, id,
		func(k string, b []byte) error {
			m := &pb.StrategyAssignmentSpec{}
			if err := proto.Unmarshal(b, m); err != nil {
				return err
			}
			rec.Assignments[k] = m
			return nil
		}); err != nil {
		return nil, false, err
	}
	if err := loadInto(ctx, q, `SELECT strategy, status FROM statuses WHERE machine_id=$1`, id,
		func(k string, b []byte) error {
			m := &pb.StrategyAssignmentStatus{}
			if err := proto.Unmarshal(b, m); err != nil {
				return err
			}
			rec.Status[k] = m
			return nil
		}); err != nil {
		return nil, false, err
	}
	if err := loadInto(ctx, q, `SELECT strategy, artifact FROM previous_artifacts WHERE machine_id=$1`, id,
		func(k string, b []byte) error {
			m := &pb.ArtifactRef{}
			if err := proto.Unmarshal(b, m); err != nil {
				return err
			}
			rec.PreviousArtifacts[k] = m
			return nil
		}); err != nil {
		return nil, false, err
	}
	return rec, true, nil
}

// loadInto runs a (key, bytes) query and calls fn for each row.
func loadInto(ctx context.Context, q querier, sql, id string, fn func(key string, b []byte) error) error {
	rows, err := q.Query(ctx, sql, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var b []byte
		if err := rows.Scan(&key, &b); err != nil {
			return err
		}
		if err := fn(key, b); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (p *Postgres) GetMachine(machineID string) (*MachineRecord, bool) {
	ctx, cancel := opCtx()
	defer cancel()
	rec, ok, err := loadMachine(ctx, p.pool, machineID)
	if err != nil || !ok {
		return nil, false
	}
	return rec, true
}

func (p *Postgres) ListMachines() []*MachineRecord {
	ctx, cancel := opCtx()
	defer cancel()
	rows, err := p.pool.Query(ctx, `SELECT machine_id FROM machines ORDER BY machine_id`)
	if err != nil {
		return nil
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := make([]*MachineRecord, 0, len(ids))
	for _, id := range ids {
		if rec, ok, err := loadMachine(ctx, p.pool, id); err == nil && ok {
			out = append(out, rec)
		}
	}
	return out
}

func (p *Postgres) DesiredState(machineID string) (*pb.DesiredState, bool) {
	ctx, cancel := opCtx()
	defer cancel()
	rec, ok, err := loadMachine(ctx, p.pool, machineID)
	if err != nil || !ok {
		return nil, false
	}
	return buildDesiredState(rec), true
}

// --- machine writes ---

func (p *Postgres) UpsertMachine(reg *pb.Register) (*MachineRecord, error) {
	if reg.GetMachineId() == "" {
		return nil, fmt.Errorf("upsert: empty machine_id")
	}
	regBytes, err := proto.Marshal(reg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := opCtx()
	defer cancel()
	if _, err := p.pool.Exec(ctx, `INSERT INTO machines (machine_id, register, agent_version, agent_build_version, reachable)
		VALUES ($1, $2, $3, $4, TRUE)
		ON CONFLICT (machine_id) DO UPDATE
		SET register = EXCLUDED.register, agent_version = EXCLUDED.agent_version,
		    agent_build_version = EXCLUDED.agent_build_version, reachable = TRUE`,
		reg.GetMachineId(), regBytes, reg.GetAgentVersion(), reg.GetAgentBuildVersion()); err != nil {
		return nil, err
	}
	rec, _, err := loadMachine(ctx, p.pool, reg.GetMachineId())
	if err != nil {
		return nil, err
	}
	p.notify(reg.GetMachineId())
	return rec, nil
}

func (p *Postgres) SetAssignment(machineID, strategy string, spec *pb.StrategyAssignmentSpec) (int64, error) {
	ctx, cancel := opCtx()
	defer cancel()
	var gen int64
	err := p.inTx(ctx, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM machines WHERE machine_id=$1)`,
			machineID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("set assignment: unknown machine %s", machineID)
		}
		if spec == nil {
			for _, sql := range []string{
				`DELETE FROM assignments WHERE machine_id=$1 AND strategy=$2`,
				`DELETE FROM previous_artifacts WHERE machine_id=$1 AND strategy=$2`,
				`DELETE FROM statuses WHERE machine_id=$1 AND strategy=$2`,
			} {
				if _, err := tx.Exec(ctx, sql, machineID, strategy); err != nil {
					return err
				}
			}
		} else {
			// Record the replaced artifact for empty-target rollback, matching
			// Memory: only when the digest actually changes.
			var oldBytes []byte
			err := tx.QueryRow(ctx, `SELECT spec FROM assignments WHERE machine_id=$1 AND strategy=$2`,
				machineID, strategy).Scan(&oldBytes)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if oldBytes != nil {
				old := &pb.StrategyAssignmentSpec{}
				if err := proto.Unmarshal(oldBytes, old); err != nil {
					return err
				}
				if d := old.GetArtifact().GetDigest(); d != "" && d != spec.GetArtifact().GetDigest() {
					artBytes, err := proto.Marshal(old.GetArtifact())
					if err != nil {
						return err
					}
					if _, err := tx.Exec(ctx, `INSERT INTO previous_artifacts (machine_id, strategy, artifact)
						VALUES ($1,$2,$3) ON CONFLICT (machine_id, strategy) DO UPDATE SET artifact=EXCLUDED.artifact`,
						machineID, strategy, artBytes); err != nil {
						return err
					}
				}
			}
			specBytes, err := proto.Marshal(spec)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO assignments (machine_id, strategy, spec)
				VALUES ($1,$2,$3) ON CONFLICT (machine_id, strategy) DO UPDATE SET spec=EXCLUDED.spec`,
				machineID, strategy, specBytes); err != nil {
				return err
			}
		}
		return tx.QueryRow(ctx,
			`UPDATE machines SET generation = generation + 1 WHERE machine_id=$1 RETURNING generation`,
			machineID).Scan(&gen)
	})
	if err != nil {
		return 0, err
	}
	p.notify(machineID)
	return gen, nil
}

func (p *Postgres) ApplyStatus(machineID string, report *pb.StatusReport) error {
	ctx, cancel := opCtx()
	defer cancel()
	err := p.inTx(ctx, func(tx pgx.Tx) error {
		if err := requireMachine(ctx, tx, machineID, "apply status"); err != nil {
			return err
		}
		keep := make([]string, 0, len(report.GetAssignments()))
		for _, a := range report.GetAssignments() {
			b, err := proto.Marshal(a)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO statuses (machine_id, strategy, status)
				VALUES ($1,$2,$3) ON CONFLICT (machine_id, strategy) DO UPDATE SET status=EXCLUDED.status`,
				machineID, a.GetStrategy(), b); err != nil {
				return err
			}
			keep = append(keep, a.GetStrategy())
		}
		// StatusReport is a full snapshot: drop strategies the agent no longer
		// tracks (finished undeploy/drain).
		if len(keep) == 0 {
			if _, err := tx.Exec(ctx, `DELETE FROM statuses WHERE machine_id=$1`, machineID); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(ctx,
				`DELETE FROM statuses WHERE machine_id=$1 AND NOT (strategy = ANY($2))`,
				machineID, keep); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx,
			`UPDATE machines SET observed_gen = GREATEST(observed_gen, $2) WHERE machine_id=$1`,
			machineID, report.GetObservedGeneration())
		return err
	})
	if err != nil {
		return err
	}
	p.notify(machineID)
	return nil
}

func (p *Postgres) ApplyHeartbeat(machineID string, hb *pb.Heartbeat, atUnix int64) error {
	var resBytes []byte
	if hb.GetResources() != nil {
		b, err := proto.Marshal(hb.GetResources())
		if err != nil {
			return err
		}
		resBytes = b
	}
	ctx, cancel := opCtx()
	defer cancel()
	tag, err := p.pool.Exec(ctx, `UPDATE machines
		SET last_heartbeat=$2, agent_version=$3, agent_build_version=$4, reachable=TRUE,
		    observed_gen=GREATEST(observed_gen, $5),
		    last_resources=COALESCE($6, last_resources)
		WHERE machine_id=$1`,
		machineID, atUnix, hb.GetAgentVersion(), hb.GetAgentBuildVersion(),
		hb.GetObservedGeneration(), resBytes)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("apply heartbeat: unknown machine %s", machineID)
	}
	p.notify(machineID)
	return nil
}

func (p *Postgres) SetReachable(machineID string, reachable bool) error {
	ctx, cancel := opCtx()
	defer cancel()
	tag, err := p.pool.Exec(ctx, `UPDATE machines SET reachable=$2 WHERE machine_id=$1`, machineID, reachable)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set reachable: unknown machine %s", machineID)
	}
	p.notify(machineID)
	return nil
}

func requireMachine(ctx context.Context, q querier, machineID, op string) error {
	var exists bool
	if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM machines WHERE machine_id=$1)`,
		machineID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%s: unknown machine %s", op, machineID)
	}
	return nil
}

// --- audit ---

func (p *Postgres) AppendAudit(entry *pb.AuditEntry) error {
	if entry.GetTimestamp() == nil {
		entry.Timestamp = timestamppb.Now()
	}
	b, err := proto.Marshal(entry)
	if err != nil {
		return err
	}
	ctx, cancel := opCtx()
	defer cancel()
	_, err = p.pool.Exec(ctx, `INSERT INTO audit (machine_id, strategy, ts, entry) VALUES ($1,$2,$3,$4)`,
		entry.GetMachineId(), entry.GetStrategy(), entry.GetTimestamp().GetSeconds(), b)
	return err
}

func (p *Postgres) ListAudit(machineID, strategy string) []*pb.AuditEntry {
	ctx, cancel := opCtx()
	defer cancel()
	rows, err := p.pool.Query(ctx, `SELECT entry FROM audit
		WHERE ($1='' OR machine_id=$1) AND ($2='' OR strategy=$2) ORDER BY id DESC`,
		machineID, strategy)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*pb.AuditEntry
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return out
		}
		e := &pb.AuditEntry{}
		if err := proto.Unmarshal(b, e); err != nil {
			return out
		}
		out = append(out, e)
	}
	return out
}

// --- artifacts ---

func (p *Postgres) RegisterArtifact(ref *pb.ArtifactRef) error {
	if ref.GetName() == "" || ref.GetVersion() == "" || ref.GetDigest() == "" {
		return fmt.Errorf("register artifact: name, version, and digest are required")
	}
	if ref.GetUri() == "" {
		return fmt.Errorf("register artifact: uri is required")
	}
	if err := artifacturi.Validate(ref.GetUri()); err != nil {
		return fmt.Errorf("register artifact: %w", err)
	}
	b, err := proto.Marshal(ref)
	if err != nil {
		return err
	}
	ctx, cancel := opCtx()
	defer cancel()
	_, err = p.pool.Exec(ctx, `INSERT INTO artifacts (name, version, ref) VALUES ($1,$2,$3)
		ON CONFLICT (name, version) DO UPDATE SET ref=EXCLUDED.ref`,
		ref.GetName(), ref.GetVersion(), b)
	return err
}

func (p *Postgres) GetArtifact(name, version string) (*pb.ArtifactRef, bool) {
	ctx, cancel := opCtx()
	defer cancel()
	var b []byte
	err := p.pool.QueryRow(ctx, `SELECT ref FROM artifacts WHERE name=$1 AND version=$2`, name, version).Scan(&b)
	if err != nil {
		return nil, false
	}
	ref := &pb.ArtifactRef{}
	if err := proto.Unmarshal(b, ref); err != nil {
		return nil, false
	}
	return ref, true
}

func (p *Postgres) ListArtifacts(name string) []*pb.ArtifactRef {
	ctx, cancel := opCtx()
	defer cancel()
	rows, err := p.pool.Query(ctx, `SELECT ref FROM artifacts WHERE ($1='' OR name=$1)
		ORDER BY name, version`, name)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*pb.ArtifactRef
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return out
		}
		ref := &pb.ArtifactRef{}
		if err := proto.Unmarshal(b, ref); err != nil {
			return out
		}
		out = append(out, ref)
	}
	return out
}

func (p *Postgres) PreviousArtifact(machineID, strategy string) (*pb.ArtifactRef, bool) {
	ctx, cancel := opCtx()
	defer cancel()
	var b []byte
	err := p.pool.QueryRow(ctx, `SELECT artifact FROM previous_artifacts WHERE machine_id=$1 AND strategy=$2`,
		machineID, strategy).Scan(&b)
	if err != nil {
		return nil, false
	}
	ref := &pb.ArtifactRef{}
	if err := proto.Unmarshal(b, ref); err != nil {
		return nil, false
	}
	return ref, true
}

// SetLeaseMarginCP sets the control-plane lease expiry margin.
func (p *Postgres) SetLeaseMarginCP(d time.Duration) {
	p.leaseMu.Lock()
	defer p.leaseMu.Unlock()
	if d < 0 {
		d = 0
	}
	p.leaseMargin = d
}

func (p *Postgres) LeaseMarginCP() time.Duration {
	p.leaseMu.Lock()
	defer p.leaseMu.Unlock()
	return p.leaseMargin
}

func (p *Postgres) GetLease(strategy string) (LeaseInfo, bool) {
	ctx, cancel := opCtx()
	defer cancel()
	info, err := p.loadLease(ctx, p.pool, strategy, false)
	if err != nil || info == nil {
		return LeaseInfo{}, false
	}
	return *info, true
}

func (p *Postgres) AcquireLease(machineID, strategy string, ttl time.Duration) (LeaseResult, error) {
	if machineID == "" || strategy == "" {
		return LeaseResult{}, fmt.Errorf("acquire: machine_id and strategy are required")
	}
	if ttl <= 0 {
		return LeaseResult{}, fmt.Errorf("acquire: ttl must be positive")
	}
	ctx, cancel := opCtx()
	defer cancel()
	var out LeaseResult
	err := p.inTx(ctx, func(tx pgx.Tx) error {
		now := p.now()
		cur, err := p.loadLease(ctx, tx, strategy, true)
		if err != nil {
			return err
		}
		margin := p.LeaseMarginCP()
		if free, deny := leaseFreeFor(cur, machineID, now, margin); !free {
			out = LeaseResult{DenyReason: deny}
			return nil
		}
		id, err := newLeaseID()
		if err != nil {
			return err
		}
		exp := now.Add(ttl)
		if _, err := tx.Exec(ctx, `
			INSERT INTO leases (strategy, machine_id, lease_id, expires_at, ttl_nanos)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (strategy) DO UPDATE SET
				machine_id = EXCLUDED.machine_id,
				lease_id = EXCLUDED.lease_id,
				expires_at = EXCLUDED.expires_at,
				ttl_nanos = EXCLUDED.ttl_nanos`,
			strategy, machineID, id, exp.UTC(), ttl.Nanoseconds()); err != nil {
			return err
		}
		out = LeaseResult{Granted: true, LeaseID: id, ExpiresAt: exp}
		return nil
	})
	if err != nil {
		return LeaseResult{}, err
	}
	if out.Granted {
		p.notify(machineID)
	}
	return out, nil
}

func (p *Postgres) RenewLease(machineID, strategy, leaseID string, ttl time.Duration) (LeaseResult, error) {
	if machineID == "" || strategy == "" || leaseID == "" {
		return LeaseResult{}, fmt.Errorf("renew: machine_id, strategy, and lease_id are required")
	}
	ctx, cancel := opCtx()
	defer cancel()
	var out LeaseResult
	err := p.inTx(ctx, func(tx pgx.Tx) error {
		now := p.now()
		cur, err := p.loadLease(ctx, tx, strategy, true)
		if err != nil {
			return err
		}
		if cur == nil {
			out = LeaseResult{DenyReason: "no lease"}
			return nil
		}
		margin := p.LeaseMarginCP()
		if cur.MachineID != machineID || cur.LeaseID != leaseID {
			out = LeaseResult{DenyReason: denyHeld(cur.MachineID, cur.ExpiresAt.Add(margin))}
			return nil
		}
		if !now.Before(cur.ExpiresAt) {
			out = LeaseResult{DenyReason: "lease expired"}
			return nil
		}
		if ttl <= 0 {
			ttl = cur.TTL
		}
		if ttl <= 0 {
			ttl = 30 * time.Second
		}
		exp := now.Add(ttl)
		if _, err := tx.Exec(ctx, `
			UPDATE leases SET expires_at=$1, ttl_nanos=$2
			WHERE strategy=$3 AND machine_id=$4 AND lease_id=$5`,
			exp.UTC(), ttl.Nanoseconds(), strategy, machineID, leaseID); err != nil {
			return err
		}
		out = LeaseResult{Granted: true, LeaseID: leaseID, ExpiresAt: exp}
		return nil
	})
	if err != nil {
		return LeaseResult{}, err
	}
	if out.Granted {
		p.notify(machineID)
	}
	return out, nil
}

func (p *Postgres) loadLease(ctx context.Context, q querier, strategy string, forUpdate bool) (*LeaseInfo, error) {
	sql := `SELECT machine_id, lease_id, expires_at, ttl_nanos FROM leases WHERE strategy=$1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	var machineID, leaseID string
	var expiresAt time.Time
	var ttlNanos int64
	err := q.QueryRow(ctx, sql, strategy).Scan(&machineID, &leaseID, &expiresAt, &ttlNanos)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &LeaseInfo{
		Strategy:  strategy,
		MachineID: machineID,
		LeaseID:   leaseID,
		ExpiresAt: expiresAt,
		TTL:       time.Duration(ttlNanos),
	}, nil
}

// Compile-time assertions that both stores satisfy the interface.
var (
	_ Store = (*Postgres)(nil)
	_ Store = (*Memory)(nil)
)
