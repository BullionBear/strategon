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

func (p *Postgres) notifyClusters() {
	if p.hub != nil {
		p.hub.NotifyClusters()
	}
}

func (p *Postgres) ApplyNatsCluster(cluster *pb.NatsCluster) (*pb.NatsCluster, bool, error) {
	if cluster == nil || cluster.GetMetadata().GetName() == "" {
		return nil, false, fmt.Errorf("apply nats cluster: name is required")
	}
	if cluster.GetSpec() == nil {
		return nil, false, fmt.Errorf("apply nats cluster: spec is required")
	}
	ctx, cancel := opCtx()
	defer cancel()
	name := cluster.GetMetadata().GetName()
	var out *pb.NatsCluster
	var changed bool
	err := p.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := loadNatsCluster(ctx, tx, name, true)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if errors.Is(err, pgx.ErrNoRows) {
			uid, err := newClusterUID()
			if err != nil {
				return err
			}
			next := proto.Clone(cluster).(*pb.NatsCluster)
			next.Metadata.Uid = uid
			next.Metadata.Generation = 1
			next.Metadata.CreatedAt = timestamppb.New(p.now())
			if next.Status == nil {
				next.Status = &pb.NatsClusterStatus{Phase: "Pending"}
			}
			if err := upsertNatsCluster(ctx, tx, next); err != nil {
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
		next := proto.Clone(cur).(*pb.NatsCluster)
		next.Spec = proto.Clone(cluster.GetSpec()).(*pb.NatsClusterSpec)
		next.Metadata.Labels = cloneLabels(cluster.GetMetadata().GetLabels())
		if specChanged {
			next.Metadata.Generation++
		}
		if err := upsertNatsCluster(ctx, tx, next); err != nil {
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
	return proto.Clone(out).(*pb.NatsCluster), changed, nil
}

func (p *Postgres) GetNatsCluster(name string) (*pb.NatsCluster, bool) {
	ctx, cancel := opCtx()
	defer cancel()
	c, err := loadNatsCluster(ctx, p.pool, name, false)
	if err != nil {
		return nil, false
	}
	return c, true
}

func (p *Postgres) ListNatsClusters() []*pb.NatsCluster {
	ctx, cancel := opCtx()
	defer cancel()
	rows, err := p.pool.Query(ctx, `SELECT name FROM nats_clusters ORDER BY name`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*pb.NatsCluster
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil
		}
		c, err := loadNatsCluster(ctx, p.pool, n, false)
		if err == nil {
			out = append(out, c)
		}
	}
	return out
}

func (p *Postgres) UpdateNatsClusterStatus(name string, status *pb.NatsClusterStatus) error {
	ctx, cancel := opCtx()
	defer cancel()
	err := p.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := loadNatsCluster(ctx, tx, name, true)
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
		_, err = tx.Exec(ctx, `UPDATE nats_clusters SET status=$2 WHERE name=$1`, name, statusBytes)
		return err
	})
	if err != nil {
		return err
	}
	p.notifyClusters()
	return nil
}

func (p *Postgres) MarkNatsClusterDeleting(name string) (*pb.NatsCluster, error) {
	ctx, cancel := opCtx()
	defer cancel()
	cur, err := loadNatsCluster(ctx, p.pool, name, false)
	if err != nil {
		return nil, fmt.Errorf("delete nats cluster: %q not found", name)
	}
	if cur.Status == nil {
		cur.Status = &pb.NatsClusterStatus{}
	}
	cur.Status.Deleting = true
	cur.Status.Phase = "Deleting"
	if err := upsertNatsCluster(ctx, p.pool, cur); err != nil {
		return nil, err
	}
	p.notifyClusters()
	return cur, nil
}

func (p *Postgres) DeleteNatsCluster(name string) error {
	ctx, cancel := opCtx()
	defer cancel()
	tag, err := p.pool.Exec(ctx, `DELETE FROM nats_clusters WHERE name=$1`, name)
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
	for _, c := range p.ListNatsClusters() {
		if name, ok := reservedByLocked(map[string]*pb.NatsCluster{c.GetMetadata().GetName(): c}, machineID, strategy); ok {
			return name, true
		}
	}
	return "", false
}

func upsertNatsCluster(ctx context.Context, q querier, c *pb.NatsCluster) error {
	specBytes, err := proto.Marshal(c.GetSpec())
	if err != nil {
		return err
	}
	status := c.GetStatus()
	if status == nil {
		status = &pb.NatsClusterStatus{Phase: "Pending"}
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
	_, err = q.Exec(ctx, `INSERT INTO nats_clusters (name, uid, generation, created_at, spec, status, labels)
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

func loadNatsCluster(ctx context.Context, q querier, name string, forUpdate bool) (*pb.NatsCluster, error) {
	var uid string
	var gen int64
	var created time.Time
	var specBytes, statusBytes, labelBytes []byte
	qstr := `SELECT uid, generation, created_at, spec, status, labels FROM nats_clusters WHERE name=$1`
	if forUpdate {
		qstr += ` FOR UPDATE`
	}
	err := q.QueryRow(ctx, qstr, name).Scan(&uid, &gen, &created, &specBytes, &statusBytes, &labelBytes)
	if err != nil {
		return nil, err
	}
	spec := &pb.NatsClusterSpec{}
	if err := proto.Unmarshal(specBytes, spec); err != nil {
		return nil, err
	}
	status := &pb.NatsClusterStatus{}
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
	return &pb.NatsCluster{Metadata: meta, Spec: spec, Status: status}, nil
}
