package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/platform/apperr"
	"pad-analyzer/internal/redisx"
	"pad-core/models"
)

// Stream commands across replicas (ADR 0033). A chat stream lives on the replica that started it; the requests that
// stop it or decide its approvals land on any replica. The replica that gets one looks the owner up in the resume
// snapshot (which names it), asks it over the owner's command channel and waits a bounded time for the answer. The
// owner repeats every check before it acts. With no Redis nothing in this file runs: a stream is local and the
// requests that reach it are answered as they always were.

// CodeReplicaUnreachable is the machine code of ErrReplicaUnreachable.
const CodeReplicaUnreachable = "REPLICA_UNREACHABLE"

// ReplicaRetryAfterSeconds is the wait the 503 suggests: a replica that was restarting is back, or the Redis blip is
// over, within about this long.
const ReplicaRetryAfterSeconds = 2

// ErrReplicaUnreachable is the answer to a stop or an approval decision for a stream another replica runs that this
// one could not get through to (Redis is down, the owner is not listening, it did not answer in time): 503, retryable,
// and never a false "ok". Nothing is known to have happened: the command may or may not have been delivered, and
// repeating it is safe.
var ErrReplicaUnreachable = apperr.Unavailable(CodeReplicaUnreachable,
	"the server running this response could not be reached; try again in a moment").
	WithDetails(map[string]any{"retryAfterSeconds": ReplicaRetryAfterSeconds})

// ErrStreamNotOwned: the stream exists but is not the caller's. Handlers answer it as an unknown stream.
var ErrStreamNotOwned = errors.New("stream not found")

// The declared Redis families of this file (docs/adr/0033-stream-commands-across-replicas.md).
var (
	// chatCmdChannel carries commands to the replica named by the last part. Fire-and-forget pub/sub: nothing is
	// queued for a replica that is not subscribed, and a command sent while Redis is down is refused to the user
	// (REPLICA_UNREACHABLE), never dropped silently.
	chatCmdChannel = redisx.Declare(redisx.Spec{
		Name:    "chat.cmd",
		Kind:    redisx.KindPubSub,
		Pattern: "baki:chat:cmd:{instance}",
		Version: 1,
		OnDown:  redisx.OnDownClosed,
		Owner:   "internal/service",
	})
	// chatReplyChannel carries the answers back to the replica that asked.
	chatReplyChannel = redisx.Declare(redisx.Spec{
		Name:    "chat.reply",
		Kind:    redisx.KindPubSub,
		Pattern: "baki:chat:reply:{instance}",
		Version: 1,
		OnDown:  redisx.OnDownClosed,
		Owner:   "internal/service",
	})
	// chatApprovalKey marks an approval pending on its owner: the stream, the replica and the scope a token needs
	// to decide it. Written before the card is shown, removed when it resolves; a failed write only means a
	// decision from another replica answers 404 (the owner's own replica never reads it).
	chatApprovalKey = redisx.Declare(redisx.Spec{
		Name:    "chat.approval",
		Kind:    redisx.KindString,
		Pattern: "baki:chat:approval:{proposal}",
		TTL:     ai.FixDecisionTimeout + approvalKeySlack,
		Version: 1,
		OnDown:  redisx.OnDownOpen,
		Owner:   "internal/service",
	})
	// chatDecisionKey is the SET NX claim of a proposal's first decision from another replica.
	chatDecisionKey = redisx.Declare(redisx.Spec{
		Name:    "chat.decision",
		Kind:    redisx.KindString,
		Pattern: "baki:chat:decision:{proposal}",
		TTL:     decisionKeyTTL,
		Version: 1,
		OnDown:  redisx.OnDownClosed,
		Owner:   "internal/service",
	})
	// chatActiveKey points a conversation at the stream whose answer is not stored yet, so a reload that lands on
	// another replica re-attaches. The last part is a digest of (scope, flow, conversation): no id of a user or a
	// flow is in a key name.
	chatActiveKey = redisx.Declare(redisx.Spec{
		Name:    "chat.active",
		Kind:    redisx.KindString,
		Pattern: "baki:chat:active:{conversation}",
		TTL:     maxChatStreamDuration + resumeRetention,
		Version: 1,
		OnDown:  redisx.OnDownOpen,
		Owner:   "internal/service",
	})
)

const (
	chatCmdKind   = "chat.cmd"
	chatReplyKind = "chat.reply"

	// approvalKeySlack lets the pending key outlive the wait a little, so a decision sent at the last second finds it.
	approvalKeySlack = 10 * time.Second
	// decisionKeyTTL bounds the first-decision claim: longer than any wait, short enough that the keys do not pile up.
	decisionKeyTTL = 60 * time.Second
	// clusterOpTimeout bounds one Redis round trip of the approval and pointer keys.
	clusterOpTimeout = 1500 * time.Millisecond
	// activeReadTimeout bounds the pointer read of a conversation GET: a slow Redis must not slow the conversation load.
	activeReadTimeout = 400 * time.Millisecond
	// maxProposalIDLen bounds the client-supplied proposal id before it becomes part of a key name.
	maxProposalIDLen = 128
)

// chatCommandWait is how long the origin waits for the owner's answer. The owner only flips a flag, so a longer wait
// is a dead or stuck replica; a var for tests.
var chatCommandWait = 2 * time.Second

// Operations of a chat command.
const (
	opCancel = "cancel"
	opDecide = "decide"
)

// Statuses of a chat reply.
const (
	replyOK          = "ok"
	replyGone        = "gone"
	replyNoPending   = "no_pending"
	replyForbidden   = "forbidden"
	replyScopeDenied = "scope_denied"
	// replyUnsupported answers an op this release does not know (a newer replica during a rolling deploy).
	replyUnsupported = "unsupported"
)

// chatCommand is the wire form of one command. Every field beside v and kind is additive: a replica ignores what it
// does not know.
type chatCommand struct {
	V    int    `json:"v"`
	Kind string `json:"kind"`
	Op   string `json:"op"`
	// StreamID is the stream acted on; Scope is the CALLER's stream scope (the key scope of the request), which the
	// owner compares to the scope that started the stream.
	StreamID  string `json:"streamId"`
	Scope     string `json:"scope"`
	RequestID string `json:"requestId"`
	ReplyTo   string `json:"replyTo"`
	// The decision (op decide).
	ProposalID string `json:"proposalId,omitempty"`
	Approved   bool   `json:"approved,omitempty"`
	Excluded   []int  `json:"excluded,omitempty"`
	// TokenScopes are the API-token scopes of the caller (empty: not a scoped token), for the owner's repeat of the
	// approval's scope check.
	TokenScopes []string `json:"tokenScopes,omitempty"`
}

// chatReply is the wire form of the answer.
type chatReply struct {
	V         int    `json:"v"`
	Kind      string `json:"kind"`
	RequestID string `json:"requestId"`
	Status    string `json:"status"`
}

// approvalRecord is the value of chatApprovalKey.
type approvalRecord struct {
	V       int    `json:"v"`
	Kind    string `json:"kind"`
	Stream  string `json:"stream"`
	Replica string `json:"replica"`
	// Scope is the API-token scope the tool requires to decide ("" = none).
	Scope string `json:"scope,omitempty"`
	// Live: the replica keeps a liveness key, so its absence means the approval died with it (resumeSnapshot.Live).
	Live bool `json:"live,omitempty"`
}

// activeRecord is the value of chatActiveKey.
type activeRecord struct {
	V       int    `json:"v"`
	Kind    string `json:"kind"`
	Stream  string `json:"stream"`
	Replica string `json:"replica"`
}

// chatCluster is a replica's link to the others: the shared client, its own instance name and the command bus. A nil
// *chatCluster is the single-replica deployment, and every method is a no-op on it.
type chatCluster struct {
	c        *redis.Client
	instance string
	// bus is nil when the replica has no instance name to be addressed by.
	bus *redisx.CommandBus
	// live keeps this replica's liveness key while it listens for commands; nil with one replica configured
	// (shareAcrossReplicas), and then nothing it writes says it keeps one.
	live *redisx.Liveness
	// health is the shared client's (nil: none), and now the clock: a missing liveness key is believed only once this
	// replica's own view of Redis has been steady for a whole key TTL (steadyView).
	health *redisx.Health
	now    func() time.Time
}

func newChatCluster(c *redis.Client, instance string, opts ...redisx.BusOption) *chatCluster {
	cl := &chatCluster{c: c, instance: instance, now: time.Now}
	if instance != "" {
		cl.bus = redisx.NewCommandBus(c, instance, chatCmdChannel, chatReplyChannel, opts...)
	}
	return cl
}

// attachCluster joins this replica to the others over c: the resume store and the command bus, under one name. The
// name is the configured instance id with a boot suffix (redisx.BootOrigin), which the snapshots carry as their owner
// and the command channel is addressed by, so two replicas given the same PAD_INSTANCE_ID still reach each other
// rather than each taking the other's stop for its own. A non-empty busKey signs and verifies the commands.
func (s *ChatService) attachCluster(c *redis.Client, instance string, busKey []byte) {
	origin := ""
	if instance != "" {
		origin = redisx.BootOrigin(instance)
	}
	s.resume = redisResumeStore{c: c, instance: origin}
	s.cluster = newChatCluster(c, origin, redisx.WithBusKey(busKey))
}

// StartCommands subscribes this replica to its command channel; the fx start hook calls it. A no-op without Redis.
func (s *ChatService) StartCommands() {
	if s.cluster != nil && s.cluster.bus != nil {
		s.cluster.bus.Start(s.handleChatCommand)
		s.cluster.live.Start()
	}
}

// StopCommands ends the subscriptions; the fx stop hook calls it.
func (s *ChatService) StopCommands() {
	if s.cluster != nil && s.cluster.bus != nil {
		s.cluster.bus.Stop()
		// A clean stop says at once that this replica is gone: a stop for a stream it left behind is not 503 meanwhile.
		s.cluster.live.Stop()
	}
}

// --- metrics ----------------------------------------------------------------

var (
	chatCommandsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_chat_commands_total",
			Help: "Chat stream commands between replicas by op (cancel, decide), side (origin: the replica that got the request; owner: the replica running the stream) and outcome: ok, gone (the stream had ended), no_pending (no such approval), forbidden (not the caller's stream), scope_denied (the token lacks the tool's scope), unsupported (an op this release does not know), unreachable (origin only: the owner could not be reached, the request answered 503 REPLICA_UNREACHABLE), unknown_owner (origin only: the stream's snapshot names no owner replica, a stream of the release before the commands; answered STREAM_NOT_FOUND), owner_gone (origin only: the owner did not answer and its liveness key has lapsed, a replica that died; a stop is answered as for an ended stream, a decision as no longer pending), other (a reply status this release does not know). A steady unreachable rate is a stuck replica or a Redis outage; a replica that died shows as unreachable until its liveness key lapses (15 s), then as owner_gone.",
		},
		[]string{"op", "side", "outcome"},
	)
	chatCommandSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "pad_chat_command_seconds",
			Help:    "Round trip of a chat stream command to another replica, from publish to answer, by op and whether it was answered; unanswered calls are bounded by the command wait.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"op", "answered"},
	)
)

func init() { metrics.MustRegister(chatCommandsTotal, chatCommandSeconds) }

// --- the owner's side --------------------------------------------------------

// handleChatCommand acts on one command for a stream this replica runs and returns the reply (nil sends none). The
// requester is trusted to be a replica, not to have checked: stream ownership and the approval's scope are decided
// here, as the local path decides them.
func (s *ChatService) handleChatCommand(ctx context.Context, payload []byte) []byte {
	var cmd chatCommand
	if err := json.Unmarshal(payload, &cmd); err != nil || cmd.RequestID == "" {
		return nil
	}
	status := replyUnsupported
	switch cmd.Op {
	case opCancel:
		status = s.ownerCancel(cmd)
	case opDecide:
		status = s.ownerDecide(ctx, cmd)
	}
	opLabel := cmd.Op
	if opLabel != opCancel && opLabel != opDecide {
		opLabel = "other"
	}
	chatCommandsTotal.WithLabelValues(opLabel, "owner", status).Inc()
	out, err := json.Marshal(chatReply{V: 1, Kind: chatReplyKind, RequestID: cmd.RequestID, Status: status})
	if err != nil {
		return nil
	}
	return out
}

func (s *ChatService) ownerCancel(cmd chatCommand) string {
	v, ok := s.activeStreams.Load(cmd.StreamID)
	if !ok {
		return replyGone
	}
	ctl := v.(*streamCtl)
	if ctl.ownerID != cmd.Scope {
		return replyForbidden
	}
	ctl.cancelWithReason("response stopped")
	return replyOK
}

func (s *ChatService) ownerDecide(ctx context.Context, cmd chatCommand) string {
	if v, ok := s.activeStreams.Load(cmd.StreamID); !ok {
		return replyNoPending
	} else if v.(*streamCtl).ownerID != cmd.Scope {
		return replyForbidden
	}
	if len(cmd.TokenScopes) > 0 {
		ctx = auth.WithActor(ctx, &auth.Actor{Kind: auth.ActorToken, Scopes: cmd.TokenScopes})
	}
	switch err := s.ResolveApproval(ctx, cmd.StreamID, cmd.ProposalID, cmd.Approved, cmd.Excluded); {
	case err == nil:
		return replyOK
	case errors.Is(err, ErrScopeDenied):
		return replyScopeDenied
	default: // ErrNoPendingFix: unknown, decided already, or another stream's
		return replyNoPending
	}
}

// --- the origin's side -------------------------------------------------------

// errNotDelivered marks the failures of call after which the command is known not to have reached the owner (nothing
// listens on its channel, this replica's own bus is not up): a decision can be sent again freely. Any other failure may
// have been delivered.
var errNotDelivered = errors.New("command not delivered")

// call sends cmd to the replica target and returns its answer. Every failure is ErrReplicaUnreachable, joined with
// errNotDelivered when the command is known not to have been delivered.
func (cl *chatCluster) call(ctx context.Context, target string, cmd chatCommand) (string, error) {
	return cl.callOwner(ctx, target, false, cmd)
}

// callOwner is call for an owner that may keep a liveness key (live: the snapshot or approval record says it does).
// When the owner does not answer and its key has lapsed, the failure is errOwnerGone: the stream died with its replica.
func (cl *chatCluster) callOwner(ctx context.Context, target string, live bool, cmd chatCommand) (string, error) {
	if cl == nil || cl.bus == nil {
		recordUnreachable(cmd.Op, "no_bus")
		return "", errors.Join(ErrReplicaUnreachable, errNotDelivered)
	}
	cmd.V, cmd.Kind, cmd.RequestID, cmd.ReplyTo = 1, chatCmdKind, uuid.NewString(), cl.instance
	payload, err := json.Marshal(cmd)
	if err != nil {
		return "", err
	}
	start := time.Now()
	raw, err := cl.bus.Call(ctx, target, cmd.RequestID, payload, chatCommandWait)
	answered := "true"
	if err != nil {
		answered = "false"
	}
	chatCommandSeconds.WithLabelValues(cmd.Op, answered).Observe(time.Since(start).Seconds())
	if err != nil {
		reason := "redis_error"
		switch {
		case errors.Is(err, redisx.ErrNoReceiver):
			reason = "no_receiver"
		case errors.Is(err, redisx.ErrNoReply):
			reason = "no_reply"
		case errors.Is(err, redisx.ErrNotReady):
			reason = "not_ready"
		}
		if cl.ownerGone(ctx, target, live) {
			chatCommandsTotal.WithLabelValues(cmd.Op, "origin", "owner_gone").Inc()
			return "", errOwnerGone
		}
		recordUnreachable(cmd.Op, reason)
		if errors.Is(err, redisx.ErrNoReceiver) || errors.Is(err, redisx.ErrNotReady) {
			return "", errors.Join(ErrReplicaUnreachable, errNotDelivered)
		}
		return "", ErrReplicaUnreachable
	}
	var rep chatReply
	if json.Unmarshal(raw, &rep) != nil {
		recordUnreachable(cmd.Op, "bad_reply")
		return "", ErrReplicaUnreachable
	}
	return rep.Status, nil
}

// recordUnreachable counts and logs a command that got no answer. The log carries the reason class, never the
// transport error's text (it can carry the server's address).
func recordUnreachable(op, reason string) {
	chatCommandsTotal.WithLabelValues(op, "origin", "unreachable").Inc()
	// Once per reason and interval: with Redis down every stop and decision of every user lands here.
	redisx.LimitedWarn("chat_command:"+reason, "chat command could not reach the stream's replica", nil, "op", op, "reason", reason)
}

// CancelStreamAs stops a stream for the caller whose key scope is scope, wherever it runs. A stream on this replica
// is cancelled as CancelStream does; one that ended is a no-op; one on another replica is asked over the command
// channel. A stream this replica cannot get an answer about is ErrReplicaUnreachable: never an ok that stopped
// nothing. Without Redis a stream that is not here is not anywhere, and the answer is the one it always was.
func (s *ChatService) CancelStreamAs(ctx context.Context, scope, streamID string) error {
	if _, ok := s.activeStreams.Load(streamID); ok {
		s.CancelStream(streamID)
		return nil
	}
	if _, ok := s.finishedStreams.Load(streamID); ok || s.cluster == nil {
		return nil
	}
	snap, found, err := lookupSnapshot(ctx, s.resumeBackplane(), streamID)
	if err != nil {
		recordUnreachable(opCancel, "snapshot_unreadable")
		return ErrReplicaUnreachable
	}
	if !found || snap.Done || snap.Error != "" {
		return nil // nothing is left to stop
	}
	if snap.Owner != scope {
		return ErrStreamNotOwned
	}
	if snap.Replica == s.cluster.instance {
		return nil // this replica's, and no longer running (a restart under the same name)
	}
	if snap.Replica == "" {
		// Mirrored by a release before the commands, which cannot be asked. The answer is the one that release gave for a
		// stream its replica did not run (STREAM_NOT_FOUND, which the client takes as gone): a retryable 503 would promise
		// what cannot happen while the stream lives, and the user would be told to try again for the length of a turn.
		chatCommandsTotal.WithLabelValues(opCancel, "origin", "unknown_owner").Inc()
		return ErrStreamNotOwned
	}
	status, err := s.cluster.callOwner(ctx, snap.Replica, snap.Live, chatCommand{Op: opCancel, StreamID: streamID, Scope: scope})
	if errors.Is(err, errOwnerGone) {
		// The stream ended with its replica: the answer to a stop for an ended stream. Its snapshot goes, so a resume
		// reports it interrupted instead of a stream that never ends.
		s.cluster.dropDeadSnapshot(streamID, snap.Replica)
		return nil
	}
	if err != nil {
		return ErrReplicaUnreachable
	}
	chatCommandsTotal.WithLabelValues(opCancel, "origin", statusLabel(status)).Inc()
	switch status {
	case replyOK, replyGone:
		return nil
	case replyForbidden:
		return ErrStreamNotOwned
	}
	recordUnreachable(opCancel, "unexpected_status")
	return ErrReplicaUnreachable
}

// DecideApproval delivers the caller's decision on an approval, wherever the stream runs: ResolveApproval on this
// replica, else, for an approval another replica holds, the decision goes to that replica. The pending key (stream,
// replica, required scope) answers "is there such an approval" and "may this caller decide it" here; the owner
// repeats both before it acts. The SET NX claim makes the first decision from any replica the only one sent, and a
// replica that loses it answers with the winner's outcome, not with a guess (acquireDecision).
func (s *ChatService) DecideApproval(ctx context.Context, scope, streamID, proposalID string, approved bool, excluded []int) error {
	err := s.ResolveApproval(ctx, streamID, proposalID, approved, excluded)
	if !errors.Is(err, ErrNoPendingFix) || s.cluster == nil || len(proposalID) > maxProposalIDLen {
		return err
	}
	rec, found, lerr := s.cluster.loadApproval(ctx, proposalID)
	if lerr != nil {
		recordUnreachable(opDecide, "approval_unreadable")
		return ErrReplicaUnreachable
	}
	if !found {
		// Gone: decided, timed out, or never pending. A decision that was sent and whose answer was lost left its claim
		// behind; the same decision again is that decision, applied.
		if prior, ok := s.cluster.readClaim(ctx, proposalID); ok && prior.State == claimLost && prior.Approved == approved {
			chatCommandsTotal.WithLabelValues(opDecide, "origin", replyOK).Inc()
			return nil
		}
		return ErrNoPendingFix
	}
	if rec.Stream != streamID || rec.Replica == "" || rec.Replica == s.cluster.instance {
		return ErrNoPendingFix
	}
	actor := auth.ActorFromContext(ctx)
	if rec.Scope != "" && actor != nil && !actor.Allows(rec.Scope) {
		return scopeDenied(rec.Scope)
	}
	claim, settled, err := s.cluster.acquireDecision(ctx, proposalID, approved, rec.Scope)
	if settled {
		return err
	}
	cmd := chatCommand{Op: opDecide, StreamID: streamID, Scope: scope, ProposalID: proposalID, Approved: approved}
	if approved {
		cmd.Excluded = excluded
	}
	if actor != nil && actor.Scoped() {
		cmd.TokenScopes = actor.Scopes
	}
	status, err := s.cluster.callOwner(ctx, rec.Replica, rec.Live, cmd)
	if errors.Is(err, errOwnerGone) {
		// The approval died with its replica: answered as one no longer pending. The record goes before the claim, so a
		// replica waiting on the claim finds nothing pending once it is free.
		s.cluster.dropDeadApproval(proposalID, rec.Replica)
		s.cluster.releaseDecision(proposalID, claim)
		return ErrNoPendingFix
	}
	if err != nil {
		// Not delivered at all (nobody listens, the bus is not up): the retry must be able to send. Possibly delivered
		// (the reply did not come, the publish timed out): the claim is kept and says so, so the retry neither sends a
		// second decision blindly nor reports the first as unknown.
		if errors.Is(err, errNotDelivered) {
			s.cluster.releaseDecision(proposalID, claim)
		} else {
			s.cluster.markClaimLost(proposalID, claim, approved)
		}
		return ErrReplicaUnreachable
	}
	chatCommandsTotal.WithLabelValues(opDecide, "origin", statusLabel(status)).Inc()
	conclusive, result := decisionResult(status, rec.Scope)
	if !conclusive {
		s.cluster.releaseDecision(proposalID, claim)
		recordUnreachable(opDecide, "unexpected_status")
		return ErrReplicaUnreachable
	}
	s.cluster.finishDecision(proposalID, claim, status)
	return result
}

// decisionResult maps the owner's status to what the request answers; conclusive is false for a status that says
// nothing about the decision (an op this release does not know).
func decisionResult(status, requiredScope string) (conclusive bool, err error) {
	switch status {
	case replyOK:
		return true, nil
	case replyNoPending:
		return true, ErrNoPendingFix
	case replyScopeDenied:
		return true, scopeDenied(requiredScope)
	case replyForbidden:
		return true, ErrStreamNotOwned
	}
	return false, nil
}

// statusLabel is the label value of a reply status: the known set, else "other". A reply comes from another process
// (a newer release, or something that is not a replica), and what it says must not become a series.
func statusLabel(status string) string {
	switch status {
	case replyOK, replyGone, replyNoPending, replyForbidden, replyScopeDenied, replyUnsupported:
		return status
	}
	return "other"
}

// scopeDenied is the scope refusal ResolveApproval words the same way.
func scopeDenied(scope string) error {
	return &scopeDeniedError{scope: scope}
}

type scopeDeniedError struct{ scope string }

func (e *scopeDeniedError) Error() string {
	return ErrScopeDenied.Error() + " (deciding this approval needs " + e.scope + ")"
}
func (e *scopeDeniedError) Unwrap() error { return ErrScopeDenied }

// OwnerOfChecked is OwnerOf for the commands: the stream's owner scope, "" when the stream is unknown, and
// ErrReplicaUnreachable when it is not on this replica and the shared snapshot could not be read (Redis is down), so
// the request is told to retry instead of being refused as an unknown stream.
func (s *ChatService) OwnerOfChecked(ctx context.Context, streamID string) (string, error) {
	if val, ok := s.activeStreams.Load(streamID); ok {
		return val.(*streamCtl).ownerID, nil
	}
	if val, ok := s.finishedStreams.Load(streamID); ok {
		return val.(*streamCtl).ownerID, nil
	}
	snap, found, err := lookupSnapshot(ctx, s.resumeBackplane(), streamID)
	if err != nil {
		recordUnreachable("lookup", "snapshot_unreadable")
		return "", ErrReplicaUnreachable
	}
	if !found {
		return "", nil
	}
	return snap.Owner, nil
}

// --- the approval and pointer keys -------------------------------------------

func (cl *chatCluster) opCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), clusterOpTimeout)
}

// registerApproval publishes a pending approval for the other replicas and reports whether it is published. Best
// effort: a failure is logged and the approval still works for decisions that reach this replica.
func (cl *chatCluster) registerApproval(proposalID, streamID, requiredScope string, wait time.Duration) bool {
	if cl == nil || len(proposalID) > maxProposalIDLen {
		return false
	}
	val, err := json.Marshal(approvalRecord{V: 1, Kind: chatApprovalKey.Name(), Stream: streamID, Replica: cl.instance, Scope: requiredScope, Live: cl.live != nil})
	if err != nil {
		return false
	}
	ctx, cancel := cl.opCtx(context.Background())
	defer cancel()
	if err := cl.c.Set(ctx, chatApprovalKey.Key(proposalID), val, wait+approvalKeySlack).Err(); err != nil {
		redisx.LimitedWarn("chat_approval_publish", "chat approval not published: a decision from another replica will not find it", err)
		return false
	}
	return true
}

// dropApproval removes the pending marker once the approval resolved. The TTL covers a failure. The decision claim stays:
// it holds the outcome the replicas that lost it are waiting for, and expires on its own TTL.
func (cl *chatCluster) dropApproval(proposalID string) {
	if cl == nil || len(proposalID) > maxProposalIDLen {
		return
	}
	ctx, cancel := cl.opCtx(context.Background())
	defer cancel()
	_ = cl.c.Del(ctx, chatApprovalKey.Key(proposalID)).Err()
}

func (cl *chatCluster) loadApproval(ctx context.Context, proposalID string) (approvalRecord, bool, error) {
	ctx, cancel := cl.opCtx(ctx)
	defer cancel()
	b, err := cl.c.Get(ctx, chatApprovalKey.Key(proposalID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return approvalRecord{}, false, nil
	}
	if err != nil {
		return approvalRecord{}, false, err
	}
	var rec approvalRecord
	if redisx.Decode(chatApprovalKey.Name(), chatApprovalKey.Spec().Version, b, &rec) != redisx.ResultOK {
		return approvalRecord{}, false, nil
	}
	return rec, true, nil
}

// The states of a decision claim (decisionClaim.State).
const (
	// claimSending: a replica won the claim and is delivering its decision to the owner.
	claimSending = "sending"
	// claimDone: the owner answered; Status is its answer. Losers return it as their own.
	claimDone = "done"
	// claimLost: the decision was sent and no answer came (the owner may or may not have applied it). Approved is the
	// decision, so the same decision sent again is recognised as that decision.
	claimLost = "lost"
)

// claimStale is how long a claim in claimSending is worked on: the winner's call is bounded by chatCommandWait, so a
// claim older than that, plus a margin for the claim write and the clock skew, belongs to a replica that died.
func claimStale() time.Duration { return chatCommandWait + 2*time.Second }

// decisionClaim is the value of chatDecisionKey: who claimed the proposal's first decision and how it went. Every
// replica that loses the claim reads it to answer with the winner's outcome.
type decisionClaim struct {
	V        int    `json:"v"`
	Kind     string `json:"kind"`
	By       string `json:"by"`
	Token    string `json:"token"`
	State    string `json:"state"`
	Status   string `json:"status,omitempty"`
	Approved bool   `json:"approved,omitempty"`
	At       int64  `json:"at"` // unix ms of the claim (or of the last state change)
}

func (cl *chatCluster) claimValue(state, status string, approved bool) string {
	b, _ := json.Marshal(decisionClaim{V: 1, Kind: chatDecisionKey.Name(), By: cl.instance, Token: uuid.NewString(), State: state, Status: status, Approved: approved, At: time.Now().UnixMilli()})
	return string(b)
}

// claimDecision takes the proposal's first-decision claim: won is true for exactly one caller across the replicas.
// claim is the value written, which the other claim calls need to prove they still hold it.
func (cl *chatCluster) claimDecision(ctx context.Context, proposalID string) (won bool, claim string, err error) {
	ctx, cancel := cl.opCtx(ctx)
	defer cancel()
	val := cl.claimValue(claimSending, "", false)
	won, err = cl.c.SetNX(ctx, chatDecisionKey.Key(proposalID), val, decisionKeyTTL).Result()
	return won, val, err
}

// readClaim reads the claim. ok is false when there is none, it cannot be read, or it is not a shape this release knows.
func (cl *chatCluster) readClaim(ctx context.Context, proposalID string) (decisionClaim, bool) {
	c, _, err := cl.readClaimRaw(ctx, proposalID)
	return c, err == nil && c.State != ""
}

// readClaimRaw is readClaim that also returns the stored text, which the compare-and-set calls need. err is redis.Nil
// when there is no claim; a claim of a shape this release does not know comes back with an empty State.
func (cl *chatCluster) readClaimRaw(ctx context.Context, proposalID string) (decisionClaim, string, error) {
	ctx, cancel := cl.opCtx(ctx)
	defer cancel()
	raw, err := cl.c.Get(ctx, chatDecisionKey.Key(proposalID)).Result()
	if err != nil {
		return decisionClaim{}, "", err
	}
	var c decisionClaim
	if redisx.Decode(chatDecisionKey.Name(), chatDecisionKey.Spec().Version, []byte(raw), &c) != redisx.ResultOK {
		return decisionClaim{}, raw, nil
	}
	return c, raw, nil
}

// releaseScript deletes a key only while it still holds the value the caller wrote.
var releaseScript = redis.NewScript(`if redis.call("get", KEYS[1]) == ARGV[1] then return redis.call("del", KEYS[1]) else return 0 end`)

// replaceScript replaces a key's value only while it still holds the value the caller read, and sets its expiry.
var replaceScript = redis.NewScript(`if redis.call("get", KEYS[1]) == ARGV[1] then redis.call("set", KEYS[1], ARGV[2], "EX", tonumber(ARGV[3])) return 1 else return 0 end`)

// releaseDecision gives the claim back after a decision that was not delivered, so the retry can send it. Best effort.
func (cl *chatCluster) releaseDecision(proposalID, claim string) {
	ctx, cancel := cl.opCtx(context.Background())
	defer cancel()
	_ = releaseScript.Run(ctx, cl.c, []string{chatDecisionKey.Key(proposalID)}, claim).Err()
}

// replaceClaim swaps the claim for a new value while it is still the one the caller holds.
func (cl *chatCluster) replaceClaim(proposalID, from, to string, ttl time.Duration) bool {
	ctx, cancel := cl.opCtx(context.Background())
	defer cancel()
	n, err := replaceScript.Run(ctx, cl.c, []string{chatDecisionKey.Key(proposalID)}, from, to, int(ttl/time.Second)).Int()
	return err == nil && n == 1
}

// finishDecision records the owner's answer in the claim, for the replicas that lost it. Best effort: a loser that
// cannot read an answer waits out its bound and reports the owner unreachable, which is safe to retry.
func (cl *chatCluster) finishDecision(proposalID, claim, status string) {
	cl.replaceClaim(proposalID, claim, cl.claimValue(claimDone, status, false), doneClaimTTL)
}

// markClaimLost records that the decision was sent and unanswered.
func (cl *chatCluster) markClaimLost(proposalID, claim string, approved bool) {
	cl.replaceClaim(proposalID, claim, cl.claimValue(claimLost, "", approved), decisionKeyTTL)
}

// doneClaimTTL is how long a finished claim stays readable to the losers that are still waiting on it.
const doneClaimTTL = 30 * time.Second

// claimPoll is the interval at which a replica that lost the claim looks at it again.
const claimPoll = 25 * time.Millisecond

// maxClaimRounds bounds how many times a replica tries to become the sender before it gives up (each round it lost to
// a winner that then released).
const maxClaimRounds = 4

// acquireDecision makes this replica the one sender of the proposal's decision, or settles the request from the
// winner's outcome. It returns claim and settled=false when this replica won (or took over a dead winner's claim) and
// must send; settled=true means the request is answered by err (nil: the decision reached the owner or is recorded).
//
// A replica that loses the claim never answers ok on the strength of losing: it waits, bounded by the winner's own
// call timeout, for the claim to say the owner answered, and otherwise reports the owner unreachable so the user
// retries. A claim released after a failed delivery is claimed again; one whose winner died is taken over.
func (cl *chatCluster) acquireDecision(ctx context.Context, proposalID string, approved bool, requiredScope string) (claim string, settled bool, err error) {
	for round := 0; round < maxClaimRounds; round++ {
		won, claim, cerr := cl.claimDecision(ctx, proposalID)
		if cerr != nil {
			recordUnreachable(opDecide, "claim_failed")
			return "", true, ErrReplicaUnreachable
		}
		if won {
			return claim, false, nil
		}
		next, settled, err := cl.awaitClaim(ctx, proposalID, approved, requiredScope)
		if settled {
			return "", true, err
		}
		if next != "" {
			return next, false, nil // took over
		}
	}
	recordUnreachable(opDecide, "claim_contended")
	return "", true, ErrReplicaUnreachable
}

// awaitClaim watches the claim another replica holds. It returns settled=true with the request's answer, or
// settled=false with a non-empty takeover claim (this replica sends), or both empty (the claim is free: claim it).
func (cl *chatCluster) awaitClaim(ctx context.Context, proposalID string, approved bool, requiredScope string) (takeover string, settled bool, err error) {
	deadline := time.Now().Add(claimStale() + chatCommandWait)
	for {
		c, raw, rerr := cl.readClaimRaw(ctx, proposalID)
		switch {
		case rerr != nil && !errors.Is(rerr, redis.Nil):
			recordUnreachable(opDecide, "claim_unreadable")
			return "", true, ErrReplicaUnreachable
		case errors.Is(rerr, redis.Nil):
			// Released (the winner could not deliver) or expired: nothing claims it. If the approval is gone too, the
			// decision was applied or timed out, and there is nothing left to decide.
			if _, pending, lerr := cl.loadApproval(ctx, proposalID); lerr == nil && !pending {
				return "", true, ErrNoPendingFix
			}
			return "", false, nil
		case c.State == "":
			// A claim this release cannot read (another release's shape): not ours to judge. Wait it out.
		case c.State == claimDone:
			if conclusive, result := decisionResult(c.Status, requiredScope); conclusive {
				return "", true, result
			}
			return "", true, ErrReplicaUnreachable
		case c.State == claimLost:
			// The winner's decision went out and its answer did not come back. If the approval is gone it was decided:
			// the same decision is that decision; the opposite one is too late. If it is still pending the owner never
			// got it: take the claim over and send.
			if _, pending, lerr := cl.loadApproval(ctx, proposalID); lerr == nil && !pending {
				if c.Approved == approved {
					chatCommandsTotal.WithLabelValues(opDecide, "origin", replyOK).Inc()
					return "", true, nil
				}
				return "", true, ErrNoPendingFix
			}
			if claim := cl.claimValue(claimSending, "", false); cl.replaceClaim(proposalID, raw, claim, decisionKeyTTL) {
				return claim, false, nil
			}
		case c.State == claimSending && time.Since(time.UnixMilli(c.At)) > claimStale():
			// Its winner died (or hung) after claiming: nobody is delivering this decision.
			if claim := cl.claimValue(claimSending, "", false); cl.replaceClaim(proposalID, raw, claim, decisionKeyTTL) {
				return claim, false, nil
			}
		}
		if time.Now().After(deadline) {
			recordUnreachable(opDecide, "claim_inflight")
			return "", true, ErrReplicaUnreachable
		}
		select {
		case <-ctx.Done():
			return "", true, ErrReplicaUnreachable
		case <-time.After(claimPoll):
		}
	}
}

// conversationToken names a conversation of a scope in a key without putting an id in it.
func conversationToken(scope, flowID, convKey string) string {
	h := sha256.New()
	for _, p := range []string{scope, flowID, convKey} {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// publishActive points the stream's conversation at it (called once the question is stored). Reports whether it was
// written.
func (cl *chatCluster) publishActive(ctl *streamCtl) bool {
	if cl == nil || ctl.flowID == "" {
		return false
	}
	val, err := json.Marshal(activeRecord{V: 1, Kind: chatActiveKey.Name(), Stream: ctl.streamID, Replica: cl.instance})
	if err != nil {
		return false
	}
	ctx, cancel := cl.opCtx(context.Background())
	defer cancel()
	return cl.c.Set(ctx, chatActiveKey.Key(conversationToken(ctl.ownerID, ctl.flowID, ctl.convKey)), val, chatActiveKey.Spec().TTL).Err() == nil
}

// clearActive removes the pointer if it still names ctl's stream (a newer turn may have replaced it). The read and the
// delete are one WATCH transaction: a newer turn that writes its pointer between them makes the delete fail, so the
// newer pointer is never the one removed.
func (cl *chatCluster) clearActive(ctl *streamCtl) {
	if cl == nil || ctl.flowID == "" {
		return
	}
	key := chatActiveKey.Key(conversationToken(ctl.ownerID, ctl.flowID, ctl.convKey))
	ctx, cancel := cl.opCtx(context.Background())
	defer cancel()
	_ = cl.c.Watch(ctx, func(tx *redis.Tx) error {
		val, err := tx.Get(ctx, key).Bytes()
		if err != nil {
			return err
		}
		var rec activeRecord
		if json.Unmarshal(val, &rec) != nil || rec.Stream != ctl.streamID {
			return nil
		}
		_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
			p.Del(ctx, key)
			return nil
		})
		return err
	}, key)
}

// activeStreamOf reads the conversation's pointer and the snapshot behind it: a stream of this scope running (or just
// finished) on another replica. Redis trouble or no pointer is "none", as it was before the pointer existed.
func (cl *chatCluster) activeStreamOf(ctx context.Context, rs resumeStore, scope, flowID, convKey string) (string, resumeSnapshot, bool) {
	if cl == nil {
		return "", resumeSnapshot{}, false
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), activeReadTimeout)
	b, err := cl.c.Get(rctx, chatActiveKey.Key(conversationToken(scope, flowID, convKey))).Bytes()
	cancel()
	if err != nil {
		return "", resumeSnapshot{}, false
	}
	var rec activeRecord
	if redisx.Decode(chatActiveKey.Name(), chatActiveKey.Spec().Version, b, &rec) != redisx.ResultOK || rec.Stream == "" || rec.Replica == cl.instance {
		return "", resumeSnapshot{}, false
	}
	snap, found, err := lookupSnapshot(ctx, rs, rec.Stream)
	if err != nil || !found || snap.Owner != scope {
		return "", resumeSnapshot{}, false
	}
	return rec.Stream, snap, true
}

// settledWait bounds how long the mirror waits for a finished stream's answer to be stored before it clears the
// conversation's pointer; past it the pointer's TTL does the clearing.
const settledWait = 15 * time.Second

// clearActiveWhenSettled clears ctl's pointer once its answer is stored (the stored history then carries it), or
// after settledWait.
func (cl *chatCluster) clearActiveWhenSettled(ctl *streamCtl) {
	t := time.NewTimer(settledWait)
	defer t.Stop()
	select {
	case <-ctl.settled:
	case <-t.C:
	}
	cl.clearActive(ctl)
}

// pointActive writes the conversation's pointer the first time it is called after the stream's question is stored,
// and reports whether it is written. The mirror calls it every tick.
func (s *ChatService) pointActive(ctl *streamCtl, written bool) bool {
	if written || s.cluster == nil || !ctl.userTurnStored.Load() {
		return written
	}
	return s.cluster.publishActive(ctl)
}

// remoteStream is the caller's stream on a conversation that another replica runs (or just finished running), as
// the shared pointer and snapshot describe it.
type remoteStream struct {
	id   string
	snap resumeSnapshot
}

// remoteActiveStream finds the caller's stream on the conversation among the other replicas', when the local maps
// have none. It is ActiveStreamFor's second source: without it a reload that lands on another replica loses the
// stream. nil without Redis, without a pointer, or when Redis cannot answer (as before the pointer existed).
func (s *ChatService) remoteActiveStream(ctx context.Context, scope, flowID, convKey string) *remoteStream {
	id, snap, ok := s.cluster.activeStreamOf(ctx, s.resumeBackplane(), scope, flowID, convKey)
	if !ok {
		return nil
	}
	return &remoteStream{id: id, snap: snap}
}

// answerStored reports whether history already ends with the finished answer the snapshot holds (answerStored for
// a stream this replica does not have).
func (r *remoteStream) answerStored(history []models.ChatMessage) bool {
	n := len(history)
	return n > 0 && history[n-1].Role == "assistant" && r.snap.Done && history[n-1].Content == r.snap.Text
}

// snapshotLooker is a resume store that can tell a miss from a failure (redisResumeStore).
type snapshotLooker interface {
	Lookup(ctx context.Context, streamID string) (snap resumeSnapshot, found bool, err error)
}

// lookupSnapshot reads a stream's shared snapshot; err is set only when the store could not answer. A store that
// cannot say (the single-replica no-op) answers as Load does.
func lookupSnapshot(ctx context.Context, rs resumeStore, streamID string) (resumeSnapshot, bool, error) {
	if l, ok := rs.(snapshotLooker); ok {
		return l.Lookup(ctx, streamID)
	}
	snap, found := rs.Load(ctx, streamID)
	return snap, found, nil
}
