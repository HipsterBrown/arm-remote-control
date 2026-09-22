# Module arm-remote-control 

This module provides remote control functionality for robotic arms using gamepad input. It enables smooth, real-time control of an arm's full six-degree-of-freedom pose — translation and orientation — through continuous movement while a deadman control and the gamepad's axis controls are held.

## Model hipsterbrown:arm-remote-control:gamepad

The gamepad model allows you to control a robotic arm using a gamepad controller. It provides continuous, proportional movement on all six degrees of freedom (X, Y, Z translation and roll/pitch/yaw rotation), a deadman-gated enable control, a latching emergency stop, an optional gripper, and an automatic safety stop when the controller disconnects or goes silent mid-move.

### Features

- **Continuous Movement**: Smooth, proportional movement at 10Hz while the deadman and an axis control are held
- **Six-DOF Control**: Independent, proportional control of X/Y/Z translation and roll/pitch/yaw rotation, with Z read from the analog triggers (or a digital fallback on pads without them)
- **Deadman Enable**: The arm and gripper only actuate while the deadman is held (configurable)
- **Fine Speed**: A modifier button quarters both translation and rotation speed for precise positioning
- **Latching E-Stop**: One button halts the arm and gripper immediately and holds them stopped until explicitly cleared
- **Dead-Operator Timer**: Motion stops automatically if the controller stops reporting fresh events mid-move
- **Gripper Control**: Optional grab/open control, gated by the same deadman as arm motion
- **Disconnect Safety**: Automatic arm stop when the controller disconnects
- **Configurable Speed**: Adjustable, independent step sizes for translation and rotation sensitivity

### Control Mapping

| Control | Action |
|---------|--------|
| Left stick (`AbsoluteX` / `AbsoluteY`) | Translate X / Y, proportional |
| Right analog trigger (`AbsoluteRZ`) | Translate +Z, proportional |
| Left analog trigger (`AbsoluteZ`) | Translate −Z, proportional |
| Digital right trigger (`ButtonRT2`) | Translate +Z (fallback on pads with no analog trigger axes) |
| Digital left trigger (`ButtonLT2`) | Translate −Z (fallback on pads with no analog trigger axes) |
| Right stick (`AbsoluteRX` / `AbsoluteRY`) | Yaw / pitch, proportional |
| D-pad left/right (`AbsoluteHat0X`) | Roll, proportional |
| Left bumper (`ButtonLT`) | **Deadman** — hold to enable motion |
| Right bumper (`ButtonRT`) | Fine speed — hold to quarter (0.25×) both translation and rotation speed |
| `ButtonSouth` | Gripper grab (deadman must be held) |
| `ButtonEast` | Gripper open (deadman must be held) |
| `ButtonMenu` / `ButtonEStop` | **E-stop**, latching |
| `ButtonStart` | Clear a latched E-stop |

> **Breaking change: the D-pad no longer translates.** X and Y have moved to
> the left stick. D-pad left/right (`AbsoluteHat0X`) is now roll, and D-pad
> forward/back (`AbsoluteHat0Y`) is unbound. Z-axis control stays on the
> analog triggers, and the right bumper (`ButtonRT`) — previously unmapped —
> is now the fine-speed modifier. On pads that report no analog trigger axes
> — the Nintendo and 8BitDo "Pro Controller" S-input profile and "USB
> Gamepad" among them — Z falls back to the digital `ButtonLT2`/`ButtonRT2`
> triggers automatically.

> **The arm will not move unless the deadman is held**, and neither will the
> gripper — the deadman gates all actuation, not just arm motion. Set
> `require_enable: false` to disable this, accepting that any held control
> then moves the arm. With the deadman disabled, the dead-operator timer
> (`max_continuous_motion`) becomes the primary automatic backstop against a
> stuck or vanished controller; disabling that too
> (`max_continuous_motion: 0`) leaves only the manual E-stop.

### Movement modes

This module supports two ways of applying a movement step:

- **Direct (default).** Each tick reads `arm.EndPosition`, adds the delta, and
  calls `arm.MoveToPosition`. Deltas are expressed in the arm's own frame.
- **Motion service teleop (opt-in).** Set `motion_service` to route deltas
  through the named motion service's teleop `DoCommand` pipeline instead,
  expressed in a configurable `reference_frame` such as a gripper. Setting the
  reference frame to a gripper's frame makes forward/back/up/down move along
  the gripper's own axes, and those axes **rotate with the wrist** -- so
  "forward" stays "out of the jaws" after a roll. The direct path cannot do
  this, because it only ever adds to the arm's own frame.

  Motion service teleop does **not** add obstacle avoidance: the teleop
  pipeline omits `WorldState`/`Constraints` from its plan requests, trading
  external-obstacle checking for planning latency. Self-collision and joint
  limits still apply via the frame system, but this mode is not safer than
  the direct path.

  There is no runtime switching between modes: a config change to
  `motion_service` rebuilds the service, and once running it never falls back
  from teleop to direct mode on its own, even if the motion service starts
  erroring. If the teleop pipeline fails repeatedly, the service stops the
  arm instead.

  `step_size` and `rotation_step_size` keep their names in both modes but the
  feel changes: in direct mode they're the *maximum* millimetres/degrees per
  10Hz tick, reached at full stick deflection; in teleop mode they're the
  maximum per *plan*, and the motion service plans at a lower, variable rate.
  Expect to need noticeably larger values in teleop mode to get comparable
  responsiveness.

  Rotation composes onto the tool, not the arm frame: `directMover` composes
  each tick's rotation delta onto the arm's current orientation, and
  `teleopMover` sends the rotation as part of a relative pose resolved
  against `reference_frame` — tool-frame by construction when that frame is
  the gripper's, which is the intended setup. Translation is always applied
  in the arm/reference frame and never rotated by the same delta's rotation,
  so twisting the wrist does not change where the stick's "forward" points.

### Configuration

The following attribute template can be used to configure this model:

```json
{
  "arm": "your_arm_name",
  "input_controller": "your_gamepad_name",
  "step_size": 10.0,
  "rotation_step_size": 2.0,
  "motion_service": "builtin",
  "reference_frame": "gripper-1",
  "position_only": false,
  "gripper": "your_gripper_name",
  "require_enable": true,
  "max_continuous_motion": 30
}
```

#### Attributes

The following attributes are available for this model:

| Name | Type | Inclusion | Description |
|------|------|-----------|-------------|
| `arm` | string | Required | Name of the arm component to control. Also used as the target of `Stop()` on disconnect/close, and (in teleop mode) as the component moved. |
| `input_controller` | string | Required | Name of the input controller (gamepad) component |
| `step_size` | float | Optional | **Maximum** translation in millimetres per tick, reached at full left-stick deflection (default: `10.0`). Per 10Hz tick in direct mode; per plan in teleop mode -- expect to tune it up when `motion_service` is set. Previously a fixed per-tick step; existing configs stay valid and simply move more slowly at partial deflection now. |
| `rotation_step_size` | float | Optional | Maximum rotation in degrees per axis per tick, reached at full right-stick/D-pad deflection (default: `2.0`). Non-positive values fall back to the default; there is no upper-bound validation, so an excessively large value can reach gimbal-lock territory in the underlying Euler composition. |
| `position_only` | bool | Optional | Leaves orientation unconstrained when planning in teleop mode, by setting `goal_metric_type: position_only` on `teleop_start` (default: `false`). Only used when `motion_service` is set. **Turning this on makes the rotation controls do nothing**, because the planner then ignores goal orientation entirely. Required for arms with fewer than six DOF (the RoArm-M3, for example) that cannot hold an orientation while translating. |
| `motion_service` | string | Optional | Name of a motion service to route movement through its teleop pipeline. When unset (the default), movement uses the direct arm path. |
| `reference_frame` | string | Optional | Frame the deltas (and the teleop destination) are expressed in. Defaults to the `arm` value. Only used when `motion_service` is set. |
| `gripper` | string | Optional | Gripper component to bind the gripper controls to. Without it those controls do nothing. |
| `require_enable` | bool | Optional | Whether the deadman must be held for the arm to move (default: `true`). |
| `max_continuous_motion` | int | Optional | Seconds of continuous motion with no controller activity before the arm is stopped (default: `30`, `0` disables). Catches a controller that disappears without a disconnect event. |

> **Breaking change for teleop deployments on arms with fewer than six DOF.**
> Before this release, teleop mode always sent `goal_metric_type:
> position_only`; it is now opt-in via `position_only`, because leaving it on
> unconditionally would silently discard every rotation command from a
> six-DOF-capable arm. An existing <6-DOF teleop deployment **must add
> `"position_only": true`** to its config to keep working after upgrading.
>
> **The startup probe cannot catch a missing `position_only`.** It sends a
> zero-delta move, which asks the planner to hold the current pose — a goal
> satisfiable under any goal metric, position-only or not. So a machine that
> upgrades without adding the attribute constructs cleanly and passes the
> startup probe, then silently refuses X/Y motion the first time the operator
> tries to translate. This surfaces only through `teleop_status` error logs
> and the arm being stopped after three consecutive teleop failures — a
> fail-safe outcome, but a silent one at startup, which is why it is called
> out here rather than left to be discovered on hardware.

#### Example Configuration

Direct mode (default), with rotation tuned independently of translation:

```json
{
  "arm": "my_robot_arm",
  "input_controller": "my_gamepad",
  "step_size": 5.0,
  "rotation_step_size": 3.0
}
```

Motion service teleop mode, driving relative to the gripper's own frame:

```json
{
  "arm": "my_robot_arm",
  "input_controller": "my_gamepad",
  "step_size": 25.0,
  "motion_service": "builtin",
  "reference_frame": "gripper-1"
}
```

Teleop mode on a fewer-than-six-DOF arm, which cannot hold an orientation
while translating and so must opt in to `position_only` (accepting that the
rotation controls will do nothing):

```json
{
  "arm": "my_roarm_m3",
  "input_controller": "my_gamepad",
  "motion_service": "builtin",
  "reference_frame": "gripper-1",
  "position_only": true
}
```

With a gripper and the safety defaults left in place:

```json
{
  "arm": "my_robot_arm",
  "input_controller": "my_gamepad",
  "gripper": "my_gripper"
}
```

### Usage

1. Configure your arm and gamepad components in your robot configuration
2. Add the arm remote control service with the above configuration
3. Connect your gamepad and hold the left bumper (`ButtonLT`, the deadman) to
   enable the mapped controls:
   - Use the left stick to translate X/Y, proportional to deflection
   - Use the analog triggers to translate Z up/down, proportional to
     deflection
   - Use the right stick to rotate yaw/pitch, and the D-pad left/right to
     roll, both proportional to deflection
   - Hold the right bumper (`ButtonRT`) for fine speed, quartering both
     translation and rotation for precise positioning
   - Press `ButtonSouth`/`ButtonEast` to grab/open a configured gripper
   - Movement continues smoothly while the deadman and a control are held;
     releasing either stops it immediately
   - Press `ButtonMenu`/`ButtonEStop` at any time for an emergency stop; press
     `ButtonStart` to clear it before resuming

### Safety

- **The deadman gates all actuation.** The arm will not move, and the
  gripper will not grab or open, unless the left bumper (`ButtonLT`) is held.
  Set `require_enable: false` to disable this gate.
- **The E-stop latches.** `ButtonMenu` or `ButtonEStop` stops the arm and
  gripper immediately and holds them stopped -- releasing the button does
  *not* clear it. Only `ButtonStart` clears a latched E-stop; no other input
  is read while latched.
- **The dead-operator timer catches a controller that goes silent mid-move.**
  If a controller stops reporting fresh events -- most commonly because it
  vanished without emitting a `Disconnect` event, leaving its last events
  still saying a control is "held" -- the arm would otherwise keep moving on
  stale input forever. `max_continuous_motion` bounds how long continuous
  motion can run without a fresh event before the service stops it, logs a
  warning, and requires the operator to move a control again to resume. The
  default of 30 seconds is a starting point, not a tuned value; expect to
  adjust it for your hardware and workflow, and set it to `0` to disable the
  timer entirely.
- The arm will automatically stop if the gamepad disconnects
- In teleop mode, disconnect and service `Close()` always tear down the
  teleop pipeline (`teleop_stop`) *and* call `arm.Stop()`, since
  `teleop_stop` alone does not stop the arm; expect up to one step of coast
  after a stop
- In teleop mode, at startup the service sends a zero-delta move through the
  configured `reference_frame` and confirms the motion service actually plans
  it before finishing construction. This catches a `reference_frame` that
  does not exist in the frame system as a startup error, rather than
  starting a service that looks healthy but never actually moves the arm
