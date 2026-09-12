package reconciler

import (
	"context"
	"strings"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/agent/artifact"
	"github.com/bullionbear/strategon/internal/agent/filebrowse"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const slotWalkInterval = 60 * time.Second

type slotSize struct {
	bytes int64
	at    time.Time
}

type slotWalkDone struct {
	sizes map[string]slotSize
}

type reapOp struct {
	requestID string
	names     []string
	reply     chan *pb.ReapStrategiesResult
}

type reapBatchDone struct {
	requestID string
	names     []string
	results   []*pb.ReapStrategyResult
	reply     chan *pb.ReapStrategiesResult
}

// SubmitReap queues a reap on the main loop and blocks until it finishes
// (or ctx / the reconciler loop ends). Safe to call from the stream handler.
func (r *Reconciler) SubmitReap(ctx context.Context, req *pb.ReapStrategies) *pb.ReapStrategiesResult {
	if req == nil {
		return &pb.ReapStrategiesResult{Error: "empty request"}
	}
	reply := make(chan *pb.ReapStrategiesResult, 1)
	op := reapOp{
		requestID: req.GetRequestId(),
		names:     append([]string(nil), req.GetStrategies()...),
		reply:     reply,
	}
	loopCtx := r.ctx
	if loopCtx == nil {
		loopCtx = context.Background()
	}
	select {
	case r.reapCh <- op:
	case <-ctx.Done():
		return &pb.ReapStrategiesResult{RequestId: req.GetRequestId(), Error: ctx.Err().Error()}
	case <-loopCtx.Done():
		return &pb.ReapStrategiesResult{RequestId: req.GetRequestId(), Error: "reconciler stopped"}
	}
	select {
	case res := <-reply:
		if res == nil {
			return &pb.ReapStrategiesResult{RequestId: req.GetRequestId(), Error: "empty result"}
		}
		return res
	case <-ctx.Done():
		return &pb.ReapStrategiesResult{RequestId: req.GetRequestId(), Error: ctx.Err().Error()}
	case <-loopCtx.Done():
		return &pb.ReapStrategiesResult{RequestId: req.GetRequestId(), Error: "reconciler stopped"}
	}
}

func (r *Reconciler) buildSlotStatus() *pb.MachineSlotStatus {
	if r.deps.Artifacts == nil {
		return nil
	}
	names, err := r.deps.Artifacts.ListStrategyDirs()
	if err != nil {
		// Omit the wrapper so ApplyStatus does not overwrite stored inventory
		// with an empty walk after a transient ReadDir failure.
		return nil
	}
	out := &pb.MachineSlotStatus{}
	for _, name := range names {
		st := &pb.StrategySlotStatus{
			Strategy:       name,
			CurrentVersion: r.deps.Artifacts.CurrentVersion(name),
		}
		if sz, ok := r.slotSizes[name]; ok {
			st.SizeBytes = sz.bytes
			if !sz.at.IsZero() {
				st.SizeComputedAt = timestamppb.New(sz.at)
			}
		}
		out.Slots = append(out.Slots, st)
	}
	return out
}

func (r *Reconciler) maybeStartSlotWalk() {
	if r.deps.Artifacts == nil || r.slotWalkInflight {
		return
	}
	names, err := r.deps.Artifacts.ListStrategyDirs()
	if err != nil {
		return
	}
	key := strings.Join(names, ",")
	due := r.lastSlotWalk.IsZero() || r.now().Sub(r.lastSlotWalk) >= slotWalkInterval || key != r.lastSlotNames
	if !due {
		return
	}
	r.slotWalkInflight = true
	r.lastSlotNames = key
	copied := append([]string(nil), names...)
	go func() {
		done := slotWalkDone{sizes: r.computeSlotSizes(copied)}
		select {
		case r.slotWalkCh <- done:
		case <-r.loopDone():
		}
	}()
}

func (r *Reconciler) loopDone() <-chan struct{} {
	if r.ctx != nil {
		return r.ctx.Done()
	}
	return nil
}

func (r *Reconciler) computeSlotSizes(names []string) map[string]slotSize {
	at := r.now()
	out := make(map[string]slotSize, len(names))
	if r.deps.Artifacts == nil {
		return out
	}
	for _, n := range names {
		sz, _ := r.deps.Artifacts.StrategyDirSize(n)
		out[n] = slotSize{bytes: sz, at: at}
	}
	return out
}

func (r *Reconciler) applySlotWalk(done slotWalkDone) {
	r.slotWalkInflight = false
	r.lastSlotWalk = r.now()
	if done.sizes == nil {
		r.slotSizes = map[string]slotSize{}
		return
	}
	r.slotSizes = done.sizes
}

func (r *Reconciler) walkSlotsNow() {
	if r.deps.Artifacts == nil {
		return
	}
	names, _ := r.deps.Artifacts.ListStrategyDirs()
	r.applySlotWalk(slotWalkDone{sizes: r.computeSlotSizes(names)})
}

func (r *Reconciler) handleReapOp(op reapOp) {
	results := make([]*pb.ReapStrategyResult, 0, len(op.names))
	var pending []string
	seen := map[string]struct{}{}
	for _, name := range op.names {
		name = strings.TrimSpace(name)
		if name == "" {
			results = append(results, &pb.ReapStrategyResult{Strategy: name, Error: "empty strategy"})
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		if reason := r.reapReject(name); reason != "" {
			results = append(results, &pb.ReapStrategyResult{Strategy: name, Error: reason})
			continue
		}
		r.reaping[name] = struct{}{}
		pending = append(pending, name)
	}
	if len(pending) == 0 {
		r.replyReap(op.reply, &pb.ReapStrategiesResult{RequestId: op.requestID, Results: results})
		return
	}
	prior := results
	go r.runReapBatch(op.requestID, pending, prior, op.reply)
}

func (r *Reconciler) reapReject(name string) string {
	if err := filebrowse.ValidateStrategy(name); err != nil {
		return "invalid strategy name"
	}
	if artifact.ReservedBaseName(name) {
		return "reserved base name"
	}
	if r.desired[name] != nil {
		return "still assigned"
	}
	if _, ok := r.reaping[name]; ok {
		return "reap in progress"
	}
	if st := r.actual[name]; st != nil {
		if st.inflight != nil {
			return "deploy in progress"
		}
		if st.proc != nil || st.stopping {
			return "process still running"
		}
	}
	return ""
}

func (r *Reconciler) runReapBatch(requestID string, pending []string, prior []*pb.ReapStrategyResult, reply chan *pb.ReapStrategiesResult) {
	results := append([]*pb.ReapStrategyResult(nil), prior...)
	for _, name := range pending {
		res := &pb.ReapStrategyResult{Strategy: name}
		if r.deps.Artifacts == nil {
			res.Error = "artifacts not configured"
			results = append(results, res)
			continue
		}
		sz, _ := r.deps.Artifacts.StrategyDirSize(name)
		if err := r.deps.Artifacts.RemoveStrategyDir(name); err != nil {
			res.Error = err.Error()
			results = append(results, res)
			continue
		}
		res.Removed = true
		res.FreedBytes = sz
		results = append(results, res)
	}
	done := reapBatchDone{requestID: requestID, names: pending, results: results, reply: reply}
	select {
	case r.reapDoneCh <- done:
	case <-r.loopDone():
		r.replyReap(reply, &pb.ReapStrategiesResult{RequestId: requestID, Results: results})
	}
}

func (r *Reconciler) applyReapDone(done reapBatchDone) {
	for _, n := range done.names {
		delete(r.reaping, n)
		delete(r.slotSizes, n)
	}
	r.replyReap(done.reply, &pb.ReapStrategiesResult{RequestId: done.requestID, Results: done.results})
}

func (r *Reconciler) replyReap(ch chan *pb.ReapStrategiesResult, res *pb.ReapStrategiesResult) {
	if ch == nil {
		return
	}
	select {
	case ch <- res:
	default:
	}
}
