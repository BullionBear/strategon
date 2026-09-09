package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// assignmentSetApplyLock is the pg_advisory_xact_lock key that serializes
// ApplyAssignmentSet so the ownership check and the write cannot interleave.
// Applies are rare (a human or GitOps action), so serializing them is free.
const assignmentSetApplyLock int64 = 0x6e6174736331 // "natsc1"

func (p *Postgres) notifyClusters() {
	if p.hub != nil {
		p.hub.NotifyClusters()
	}
}

// checkReservationTx enforces cluster ownership inside the apply transaction.
func checkReservationTx(ctx context.Context, q querier, next *pb.AssignmentSet) error {
	existing, err := listAssignmentSetsTx(ctx, q)
	if err != nil {
		return err
	}
	return reservationConflict(existing, next)
}

// listAssignmentSetsTx loads every cluster through the given querier so the read
// joins the caller's transaction.
func listAssignmentSetsTx(ctx context.Context, q querier) ([]*pb.AssignmentSet, error) {
	rows, err := q.Query(ctx, `SELECT name FROM assignment_sets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, 8)
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]*pb.AssignmentSet, 0, len(names))
	for _, n := range names {
		c, err := loadAssignmentSet(ctx, q, n, false)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (p *Postgres) ApplyAssignmentSet(cluster *pb.AssignmentSet) (*pb.AssignmentSet, bool, error) {
	if cluster == nil || cluster.GetMetadata().GetName() == "" {
		return nil, false, fmt.Errorf("apply nats cluster: name is required")
	}
	if cluster.GetSpec() == nil {
		return nil, false, fmt.Errorf("apply nats cluster: spec is required")
	}
	ctx, cancel := opCtx()
	defer cancel()
	name := cluster.GetMetadata().GetName()
	var out *pb.AssignmentSet
	var changed bool
	err := p.inTx(ctx, func(tx pgx.Tx) error {
		// Serialize applies. Row locks cannot guard ownership here: the
		// conflicting cluster may not exist yet, and FOR UPDATE does not block
		// a concurrent INSERT of it.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, assignmentSetApplyLock); err != nil {
			return err
		}
		cur, err := loadAssignmentSet(ctx, tx, name, true)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if errors.Is(err, pgx.ErrNoRows) {
			if err := checkReservationTx(ctx, tx, cluster); err != nil {
				return err
			}
			uid, err := newSetUID()
			if err != nil {
				return err
			}
			next := proto.Clone(cluster).(*pb.AssignmentSet)
			next.Metadata.Uid = uid
			next.Metadata.Generation = 1
			next.Metadata.CreatedAt = timestamppb.New(p.now())
			if next.Status == nil {
				next.Status = &pb.AssignmentSetStatus{Phase: "Pending"}
			}
			if err := upsertAssignmentSet(ctx, tx, next); err != nil {
				return err
			}
			out = next
			changed = true
			return nil
		}
		specChanged := !proto.Equal(cur.GetSpec(), cluster.GetSpec())
		labelsChanged := !labelsEqual(cur.GetMetadata().GetLabels(), cluster.GetMetadata().GetLabels())
		if !specChanged && !labelsChanged {
			out = cur
			changed = false
			return nil
		}
		if specChanged {
			if err := checkReservationTx(ctx, tx, cluster); err != nil {
				return err
			}
		}
		next := proto.Clone(cur).(*pb.AssignmentSet)
		next.Spec = proto.Clone(cluster.GetSpec()).(*pb.AssignmentSetSpec)
		next.Metadata.Labels = cloneLabels(cluster.GetMetadata().GetLabels())
		if specChanged {
			next.Metadata.Generation++
		}
		if err := upsertAssignmentSet(ctx, tx, next); err != nil {
			return err
		}
		out = next
		changed = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if changed {
		p.notifyClusters()
	}
	return proto.Clone(out).(*pb.AssignmentSet), changed, nil
}

func (p *Postgres) GetAssignmentSet(name string) (*pb.AssignmentSet, bool) {
	ctx, cancel := opCtx()
	defer cancel()
	c, err := loadAssignmentSet(ctx, p.pool, name, false)
	if err != nil {
		return nil, false
	}
	return c, true
}

func (p *Postgres) ListAssignmentSets() []*pb.AssignmentSet {
	ctx, cancel := opCtx()
	defer cancel()
	rows, err := p.pool.Query(ctx, `SELECT name FROM assignment_sets ORDER BY name`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*pb.AssignmentSet
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil
		}
		c, err := loadAssignmentSet(ctx, p.pool, n, false)
		if err == nil {
			out = append(out, c)
		}
	}
	return out
}

func (p *Postgres) UpdateAssignmentSetStatus(name string, status *pb.AssignmentSetStatus) error {
	ctx, cancel := opCtx()
	defer cancel()
	err := p.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := loadAssignmentSet(ctx, tx, name, true)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("update nats cluster status: %q not found", name)
			}
			return err
		}
		next := preserveDeleting(cur.Status, status)
		statusBytes, err := proto.Marshal(next)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE assignment_sets SET status=$2 WHERE name=$1`, name, statusBytes)
		return err
	})
	if err != nil {
		return err
	}
	p.notifyClusters()
	return nil
}

func (p *Postgres) MarkAssignmentSetDeleting(name string) (*pb.AssignmentSet, error) {
	ctx, cancel := opCtx()
	defer cancel()
	cur, err := loadAssignmentSet(ctx, p.pool, name, false)
	if err != nil {
		return nil, fmt.Errorf("delete nats cluster: %q not found", name)
	}
	if cur.Status == nil {
		cur.Status = &pb.AssignmentSetStatus{}
	}
	cur.Status.Deleting = true
	cur.Status.Phase = "Deleting"
	if err := upsertAssignmentSet(ctx, p.pool, cur); err != nil {
		return nil, err
	}
	p.notifyClusters()
	return cur, nil
}

func (p *Postgres) DeleteAssignmentSet(name string) error {
	ctx, cancel := opCtx()
	defer cancel()
	tag, err := p.pool.Exec(ctx, `DELETE FROM assignment_sets WHERE name=$1`, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delete nats cluster: %q not found", name)
	}
	p.notifyClusters()
	return nil
}

func (p *Postgres) ReservedBy(machineID, strategy string) (string, bool) {
	for _, c := range p.ListAssignmentSets() {
		if name, ok := reservedByLocked(map[string]*pb.AssignmentSet{c.GetMetadata().GetName(): c}, machineID, strategy); ok {
			return name, true
		}
	}
	return "", false
}

func upsertAssignmentSet(ctx context.Context, q querier, c *pb.AssignmentSet) error {
	specBytes, err := proto.Marshal(c.GetSpec())
	if err != nil {
		return err
	}
	status := c.GetStatus()
	if status == nil {
		status = &pb.AssignmentSetStatus{Phase: "Pending"}
	}
	statusBytes, err := proto.Marshal(status)
	if err != nil {
		return err
	}
	labelBytes, err := proto.Marshal(&pb.ObjectMeta{Labels: c.GetMetadata().GetLabels()})
	if err != nil {
		return err
	}
	created := time.Now().UTC()
	if c.GetMetadata().GetCreatedAt() != nil {
		created = c.GetMetadata().GetCreatedAt().AsTime().UTC()
	}
	_, err = q.Exec(ctx, `INSERT INTO assignment_sets (name, uid, generation, created_at, spec, status, labels)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (name) DO UPDATE SET
			uid=EXCLUDED.uid,
			generation=EXCLUDED.generation,
			created_at=EXCLUDED.created_at,
			spec=EXCLUDED.spec,
			status=EXCLUDED.status,
			labels=EXCLUDED.labels`,
		c.GetMetadata().GetName(), c.GetMetadata().GetUid(), c.GetMetadata().GetGeneration(),
		created, specBytes, statusBytes, labelBytes)
	return err
}

func loadAssignmentSet(ctx context.Context, q querier, name string, forUpdate bool) (*pb.AssignmentSet, error) {
	var uid string
	var gen int64
	var created time.Time
	var specBytes, statusBytes, labelBytes []byte
	qstr := `SELECT uid, generation, created_at, spec, status, labels FROM assignment_sets WHERE name=$1`
	if forUpdate {
		qstr += ` FOR UPDATE`
	}
	err := q.QueryRow(ctx, qstr, name).Scan(&uid, &gen, &created, &specBytes, &statusBytes, &labelBytes)
	if err != nil {
		return nil, err
	}
	spec := &pb.AssignmentSetSpec{}
	if err := proto.Unmarshal(specBytes, spec); err != nil {
		return nil, err
	}
	status := &pb.AssignmentSetStatus{}
	if err := proto.Unmarshal(statusBytes, status); err != nil {
		return nil, err
	}
	meta := &pb.ObjectMeta{
		Name:       name,
		Uid:        uid,
		Generation: gen,
		CreatedAt:  timestamppb.New(created.UTC()),
	}
	if len(labelBytes) > 0 {
		lm := &pb.ObjectMeta{}
		if err := proto.Unmarshal(labelBytes, lm); err == nil {
			meta.Labels = lm.GetLabels()
		}
	}
	return &pb.AssignmentSet{Metadata: meta, Spec: spec, Status: status}, nil
}
