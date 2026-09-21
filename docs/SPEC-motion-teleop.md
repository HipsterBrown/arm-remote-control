# Spec: motion service teleop mode for `arm-remote-control`

## Goal

Add an **opt-in** mode that applies teleop deltas through the motion service's
teleop DoCommand pipeline instead of `arm.MoveToPosition`, so deltas can be
expressed in a configurable reference frame such as the gripper.

The existing direct-arm path stays and stays the default.

## Why

Today deltas are added to the pose reported by `arm.EndPosition`, which is in
the arm's own frame. An operator driving a gripper wants to move along the
gripper's axes. With the motion service, setting the reference frame to
`gripper-1` makes `hat0Y` advance along the gripper's approach direction and
the axes **rotate with the wrist**, so "forward" stays "out of the jaws" after
a roll. The direct path cannot produce this.

## Non-goals

- **This does not add obstacle avoidance.** The teleop pipeline deliberately
  omits `WorldState` and `Constraints` from its plan request, trading
  external-obstacle checking for planning latency. Self-collision and joint
  limits still apply via the frame system. Do not describe this mode as safer.
- Not a replacement for the direct path, and not the default.
- No runtime mode switching. The service is `AlwaysRebuild`, so a config change
  rebuilds it.

## No dependency change

`go.viam.com/rdk v1.7.0`, already required, exports what is needed from
`services/motion/builtin`: `DoTeleopStart`, `DoTeleopMove`, `DoTeleopStop`,
`DoTeleopStatus`. Do not bump the RDK and do not add a `replace`.

## Configuration

```json
{
  "arm": "arm-1",
  "input_controller": "keyboard-input",
  "step_size": 10,
  "motion_service": "builtin",
  "reference_frame": "gripper-1"
}
```

| Field | Type | Default | Meaning |
|---|---|---|---|
| `motion_service` | string | unset | When set, names the motion service to route through. Unset selects the direct arm path. |
| `reference_frame` | string | the `arm` value | Frame the deltas are expressed in, and the frame moved to the goal. Only read when `motion_service` is set. |

`arm` stays required in both modes: it is used for `initializePosition`, for
`Stop()` on disconnect and in `Close`, and as the target of teleop execution.

`step_size` keeps its name but changes feel. In direct mode it is millimetres
per 10Hz tick. In teleop mode it is millimetres per *plan*, and the plan rate
is lower and varies. Expect to tune it up; say so in the README.

`Validate` adds the motion service to the dependency list only when
`motion_service` is set:

```go
if cfg.MotionService != "" {
    deps = append(deps, motion.Named(cfg.MotionService).String())
}
```

## Design

### A `mover` seam

One interface, two implementations. This is the only abstraction the change
earns; do not add others.

```go
type mover interface {
    // step applies a delta. Units are millimetres.
    step(ctx context.Context, dx, dy, dz float64) error
    // stop halts motion. Always also calls arm.Stop where relevant.
    stop(ctx context.Context) error
}
```

The direct implementation keeps today's behaviour exactly: read
`arm.EndPosition`, add the delta, `arm.MoveToPosition`.

### Relative deltas, never an accumulated target

The teleop implementation sends each tick's delta as a **relative** pose in the
reference frame. It must not accumulate an absolute target locally.

This is load bearing. The pipeline re-resolves a relative delta against its
live planning head on every replan, so one notify produces one step. Holding a
key therefore produces motion only while it is held, and releasing it stops the
arm because no notify means no plan. Accumulating locally would bank goal
during a hold (3 seconds at 10Hz is 300mm of target against perhaps 5 consumed
steps) and the arm would keep travelling after release.

Today's direct path is immune to this for the same structural reason: it adds
one step to the *measured* pose every tick and so cannot get ahead of the
hardware. Preserve that property in both implementations.

### Command shapes

Note the asymmetry; it is in the RDK, not a mistake here.

`teleop_start` — value is a JSON **string** of protojson `MoveRequest`.
`component_name` lives *inside* that JSON:

```json
{"teleop_start": "{\"component_name\":\"gripper-1\",\"destination\":{\"reference_frame\":\"gripper-1\",\"pose\":{\"x\":0,\"y\":0,\"z\":0,\"o_x\":0,\"o_y\":0,\"o_z\":1,\"theta\":0}}}"}
```

`teleop_move` — value is a JSON **string** of protojson `PoseInFrame`, and
`component_name` is a separate **top-level** key:

```json
{"teleop_move": "{\"reference_frame\":\"gripper-1\",\"pose\":{\"x\":0,\"y\":0,\"z\":10,\"o_x\":0,\"o_y\":0,\"o_z\":1,\"theta\":0}}",
 "component_name": "gripper-1"}
```

`teleop_stop` — `{"teleop_stop": true}` tears down the pipeline. It does
**not** stop the arm.

`teleop_status` — returns `running`, `plan_count`, `exec_count`,
`last_plan_ms`, `error` and others. This is the **only** channel through which
plan failures surface.

Set both `component_name` and `destination.reference_frame` to the configured
reference frame.

### Failure surfacing is mandatory

`teleop_move` returns `true` immediately and always, even when the planner is
failing or stalled. A single unreachable goal can stall the planner for up to
five seconds while every move call still reports success. Without status
polling this ships a control that silently ignores input, which is strictly
worse than the direct path where `MoveToPosition` errors return on the call.

So: poll `teleop_status` roughly once per second from the movement loop, not
per step. If `error` is set, log at Error. If `running` is false, attempt one
`teleop_start` to re-establish the pipeline, since a motion service
`Reconfigure` tears it down silently.

### Startup probe

A `reference_frame` that is not in the frame system fails inside the planner
goroutine. It is stashed and logged at Warn; `teleop_move` still returns true;
the arm never moves and the service looks healthy. `Validate` cannot catch this
because the frame system does not exist at validation time.

So probe at construction, after `teleop_start`: send one zero-delta
`teleop_move`, poll `teleop_status` until `plan_count` advances or about two
seconds elapse, and **fail construction** if `error` is set. This turns a
silently dead arm into a startup error.

### Stopping

`teleop_stop` does not stop the arm, and the executor returns while the arm is
still physically moving. Always pair `teleop_stop` with `arm.Stop`, on
controller disconnect and in `Close`. Expect up to one step of coast.

### No automatic fallback

If teleop errors repeatedly, log at Error and stop the arm. Do **not** silently
switch to the direct path mid-session: changing what "forward" means while an
operator's hand is on the controls is more dangerous than stopping.

## Files

| File | Change |
|---|---|
| `remote_control.go` | `Config` fields, `Validate` dep, `mover` interface and both implementations, movement loop calls `mover.step`, status polling, startup probe |
| `README.md` | Document both modes, the config table, the frame behaviour, and that world obstacles are not checked |

## Testing

No hardware. Unit tests with a fake motion service that records DoCommand
calls:

- Direct mode is selected when `motion_service` is unset, and its behaviour is
  unchanged.
- Teleop mode issues `teleop_start` once at construction with both
  `component_name` and `reference_frame` set to the configured frame.
- `step` emits `teleop_move` with the delta in the reference frame and
  `component_name` at top level, and the payloads parse as protojson.
- Deltas are relative: two consecutive steps of the same magnitude emit the
  same delta, not a growing one.
- `reference_frame` defaults to the arm name when unset.
- Startup fails when the probe reports an error from `teleop_status`.
- A `teleop_status` error during operation is logged and does not switch modes.
- `stop` calls both `teleop_stop` and `arm.Stop`.

## Open question for the author

This branch is cut from `main`, which still registers input callbacks. That
path does not deliver events from a modular input controller, so the feature
cannot be exercised end to end until branch `poll-input-events` is merged or
this is rebased onto it. The change is orthogonal (it replaces how a delta is
applied, not how input arrives), so the two should combine cleanly. Flag this
rather than resolving it unilaterally.
