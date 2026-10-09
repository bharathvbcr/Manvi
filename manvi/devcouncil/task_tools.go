package devcouncil

import (
	"context"
	"errors"
	"fmt"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// --- task lifecycle ---

func (r *Registry) nextTask(ctx context.Context, call tools.Call) tools.Result {
	ids, err := r.deps.Store.ReadyTasks(ctx)
	if err != nil {
		return unavailable("ready tasks", err)
	}
	if len(ids) == 0 {
		return ok(map[string]any{
			"task_id": nil,
			"message": "no unheld ready tasks",
		})
	}
	task, err := r.deps.Store.Task(ctx, ids[0])
	if err != nil {
		return unavailable("task scope", err)
	}
	return ok(map[string]any{"task_id": ids[0], "task": task, "ready_count": len(ids)})
}

func (r *Registry) getTask(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		TaskID string `json:"task_id"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	task, err := r.deps.Store.Task(ctx, args.TaskID)
	if err != nil {
		return unavailable("task", err)
	}
	if task == nil {
		return tools.Errorf("no task %q", args.TaskID)
	}
	return ok(task)
}

func (r *Registry) checkout(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		TaskID string `json:"task_id"`
		Owner  string `json:"owner"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	sess := r.sessionFor(ctx)
	owner := args.Owner
	if owner == "" {
		owner = sess.State().Owner
	}
	if owner == "" {
		owner = "manvi"
	}

	lease, err := r.deps.Store.Acquire(ctx, store.AcquireRequest{
		TaskID: args.TaskID, Owner: owner, TTL: r.deps.LeaseTTL,
	})
	var conflict *store.Conflict
	if errors.As(err, &conflict) {
		// Contention is routable, not fatal: the agent takes another task.
		return ok(map[string]any{
			"acquired":         false,
			"code":             string(conflict.Code()),
			"holder":           conflict.Holder,
			"suggested_action": "pick_other_task",
			"suggested_tool":   "devcouncil_next_task",
		})
	}
	if err != nil {
		return unavailable("lease", err)
	}

	task, err := r.deps.Store.Task(ctx, lease.TaskID)
	if err != nil {
		// The lease is already taken at this point, and a checkout that cannot
		// report its scope must not leave one behind holding the task against
		// every other builder until the TTL lapses.
		r.releaseQuietly(ctx, lease.TaskID, lease.Token)
		return unavailable("task scope after checkout", err)
	}
	if task == nil {
		// The store leases any id, including one no task was ever planned
		// under, because a lease is a mutex and a mutex does not ask what it is
		// protecting. That is right for `manvi lease acquire`, which a human
		// runs deliberately. It is wrong here, and the wrongness was not
		// theoretical: a model asked to edit one file invented a task id from
		// the prompt, checked it out successfully, was refused by every write
		// with "held but has no record", released, invented it again, and spent
		// its entire step budget in that loop while its context grew to 77k
		// tokens. The checkout said yes to something nothing else would honour.
		//
		// So it fails closed and gives back what it took, and the refusal names
		// the tool that lists real work.
		r.releaseQuietly(ctx, lease.TaskID, lease.Token)
		return ok(map[string]any{
			"acquired":         false,
			"task_id":          lease.TaskID,
			"reason":           fmt.Sprintf("no task %q exists; a task id cannot be invented", lease.TaskID),
			"suggested_action": "list_real_tasks",
			"suggested_tool":   "devcouncil_next_task",
			"note": "if this work belongs to no planned task, do not check one out — " +
				"work directly, and the policy gate will decide each write on its own",
		})
	}

	// Recorded through the session rather than on it, so the lease is handed
	// to whatever is watching this agent's holdings before the agent gets to
	// do anything under it. A fan-out's cleanup can only give back a lease it
	// was told about, and a child that took one and then died is exactly the
	// case that has nobody left to tell it.
	sess.adopt(lease.TaskID, lease.Token, owner)

	// The token is deliberately absent from the payload: it is a credential,
	// and the model does not need it to work. The harness holds it.
	return ok(map[string]any{
		"acquired":   true,
		"task_id":    lease.TaskID,
		"expires_at": lease.ExpiresAt,
		"task":       task,
	})
}

func (r *Registry) renew(ctx context.Context, call tools.Call) tools.Result {
	sess := r.sessionFor(ctx)
	state := sess.State()
	if err := held(state); err != nil {
		return tools.Errorf("%v", err)
	}
	if refusal := refuseInherited(state, "renew"); refusal != nil {
		return *refusal
	}
	lease, err := r.deps.Store.Renew(ctx, state.TaskID, state.Token, r.deps.LeaseTTL)
	if err != nil {
		return unavailable("lease renewal", err)
	}
	if lease == nil {
		return ok(map[string]any{
			"renewed":          false,
			"code":             string(dc.LeaseExpired),
			"suggested_action": "checkout_again",
			"suggested_tool":   "devcouncil_checkout_task",
		})
	}
	return ok(map[string]any{"renewed": true, "expires_at": lease.ExpiresAt})
}

func (r *Registry) release(ctx context.Context, call tools.Call) tools.Result {
	sess := r.sessionFor(ctx)
	state := sess.State()
	if err := held(state); err != nil {
		return tools.Errorf("%v", err)
	}
	if refusal := refuseInherited(state, "release"); refusal != nil {
		return *refusal
	}
	released, err := r.deps.Store.Release(ctx, state.TaskID, state.Token)
	if err != nil {
		return unavailable("lease release", err)
	}
	sess.clear()
	return ok(map[string]any{"released": released})
}

// refuseInherited stops a dispatched sub-agent giving away or extending the
// lease its parent took.
//
// A child inherits the parent's task so its writes are judged against the
// parent's scope, which means it also inherits a token the store will honour.
// Acting on that token is not a scope question the posture can soften: the
// parent's own record would go on saying it holds a task the store has already
// released, and the next sibling's write would be judged against a lease
// nobody holds.
func refuseInherited(state SessionState, verb string) *tools.Result {
	if !state.Inherited {
		return nil
	}
	res := tools.Errorf(
		"this sub-agent did not check %s out — it inherited that lease from the agent that dispatched it, "+
			"and cannot %s a lease it does not own. Report what you found and let the dispatching agent "+
			"decide; or check out a task of your own if this work belongs to a different one",
		state.TaskID, verb)
	res.Rule = "lease.not_owned"
	res.Severity = "hard"
	return &res
}
