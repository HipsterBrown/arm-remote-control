# Module arm-remote-control 

This module provides remote control functionality for robotic arms using gamepad input. It enables smooth, real-time control of an arm's position through continuous movement while gamepad controls are active.

## Model hipsterbrown:arm-remote-control:gamepad

The gamepad model allows you to control a robotic arm using a gamepad controller. It provides continuous movement on all three axes (X, Y, Z) with configurable step sizes and automatic safety stops when the controller connects or disconnects.

### Features

- **Continuous Movement**: Smooth movement at 10Hz while controls are held down
- **Multi-Axis Control**: Independent control of X, Y, and Z axes
- **Safety Features**: Automatic arm stop on controller connect/disconnect events
- **Configurable Speed**: Adjustable step size for movement sensitivity
- **Real-time Response**: Immediate start/stop based on control input

### Control Mapping

| Control | Axis | Movement Direction |
|---------|------|-------------------|
| `AbsoluteHat0X` | X-axis | Left/Right on D-Pad (continuous while held) |
| `AbsoluteHat0Y` | Y-axis | Forward/Back on D-Pad (continuous while held) |
| `ButtonRT` | Z-axis | Up (continuous while held) |
| `ButtonLT` | Z-axis | Down (continuous while held) |

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

  `step_size` keeps its name in both modes but the feel changes: in direct
  mode it's millimetres per 10Hz tick, while in teleop mode it's millimetres
  per *plan*, and the motion service plans at a lower, variable rate. Expect
  to need a noticeably larger `step_size` in teleop mode to get comparable
  responsiveness.

### Configuration

The following attribute template can be used to configure this model:

```json
{
  "arm": "your_arm_name",
  "input_controller": "your_gamepad_name",
  "step_size": 10.0,
  "motion_service": "builtin",
  "reference_frame": "gripper-1"
}
```

#### Attributes

The following attributes are available for this model:

| Name | Type | Inclusion | Description |
|------|------|-----------|-------------|
| `arm` | string | Required | Name of the arm component to control. Also used as the target of `Stop()` on disconnect/close, and (in teleop mode) as the component moved. |
| `input_controller` | string | Required | Name of the input controller (gamepad) component |
| `step_size` | float | Optional | Movement step size in millimeters (default: 10.0). Per 10Hz tick in direct mode; per plan in teleop mode -- expect to tune it up when `motion_service` is set. |
| `motion_service` | string | Optional | Name of a motion service to route movement through its teleop pipeline. When unset (the default), movement uses the direct arm path. |
| `reference_frame` | string | Optional | Frame the deltas (and the teleop destination) are expressed in. Defaults to the `arm` value. Only used when `motion_service` is set. |

#### Example Configuration

Direct mode (default):

```json
{
  "arm": "my_robot_arm",
  "input_controller": "my_gamepad",
  "step_size": 5.0
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

### Usage

1. Configure your arm and gamepad components in your robot configuration
2. Add the arm remote control service with the above configuration
3. Connect your gamepad and use the mapped controls to move the arm:
   - Use the directional pad/hat for X/Y movement
   - Use the right/left triggers for Z movement up/down
   - Movement continues smoothly while controls are held
   - Release controls to stop movement immediately

### Safety

- The arm will automatically stop if the gamepad disconnects
- The arm will stop if the gamepad reconnects (to ensure safe state)
- All movement stops immediately when controls are released
- In teleop mode, disconnect/reconnect and service `Close()` always tear down
  the teleop pipeline (`teleop_stop`) *and* call `arm.Stop()`, since
  `teleop_stop` alone does not stop the arm; expect up to one step of coast
  after a stop
- In teleop mode, at startup the service sends a zero-delta move through the
  configured `reference_frame` and confirms the motion service actually plans
  it before finishing construction. This catches a `reference_frame` that
  does not exist in the frame system as a startup error, rather than
  starting a service that looks healthy but never actually moves the arm
