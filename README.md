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

### Configuration

The following attribute template can be used to configure this model:

```json
{
  "arm": "your_arm_name",
  "input_controller": "your_gamepad_name",
  "step_size": 10.0
}
```

#### Attributes

The following attributes are available for this model:

| Name | Type | Inclusion | Description |
|------|------|-----------|-------------|
| `arm` | string | Required | Name of the arm component to control |
| `input_controller` | string | Required | Name of the input controller (gamepad) component |
| `step_size` | float | Optional | Movement step size in millimeters per update (default: 10.0) |

#### Example Configuration

```json
{
  "arm": "my_robot_arm",
  "input_controller": "my_gamepad",
  "step_size": 5.0
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
