package armremotecontrol

import (
	"context"
	"sync"
	"time"

	"github.com/pkg/errors"
	"go.viam.com/utils"

	"go.viam.com/rdk/components/arm"
	"go.viam.com/rdk/components/input"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/generic"
	"go.viam.com/rdk/spatialmath"
	"go.viam.com/utils/rpc"
)

var (
	Gamepad          = resource.NewModel("hipsterbrown", "arm-remote-control", "gamepad")
	errUnimplemented = errors.New("unimplemented")
)

const (
	defaultStepSize = 10.0 // mm for all axis movements
)

func init() {
	resource.RegisterService(generic.API, Gamepad,
		resource.Registration[resource.Resource, *Config]{
			Constructor: newArmRemoteControlGamepad,
		},
	)
}

type Config struct {
	ArmName             string  `json:"arm"`
	InputControllerName string  `json:"input_controller"`
	StepSize            float64 `json:"step_size,omitempty"`
}

// Validate ensures all parts of the config are valid and important fields exist.
// Returns implicit required (first return) and optional (second return) dependencies based on the config.
// The path is the JSON path in your robot's config (not the `Config` struct) to the
// resource being validated; e.g. "components.0".
func (cfg *Config) Validate(path string) ([]string, []string, error) {
	var deps []string
	if cfg.InputControllerName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "input_controller")
	}
	deps = append(deps, cfg.InputControllerName)

	if cfg.ArmName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "arm")
	}
	deps = append(deps, cfg.ArmName)
	return deps, nil, nil
}

type armRemoteControlGamepad struct {
	resource.AlwaysRebuild

	name resource.Name

	arm             arm.Arm
	inputController input.Controller
	logger          logging.Logger
	cfg             *Config

	cancelCtx  context.Context
	cancelFunc func()

	// State management
	mu          sync.RWMutex
	targetPose  spatialmath.Pose
	initialized bool
	stepSize    float64

	// Event processing
	events                  chan struct{}
	activeBackgroundWorkers sync.WaitGroup
}

func newArmRemoteControlGamepad(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (resource.Resource, error) {
	conf, err := resource.NativeConfig[*Config](rawConf)
	if err != nil {
		return nil, err
	}

	return NewGamepad(ctx, deps, rawConf.ResourceName(), conf, logger)
}

func NewGamepad(ctx context.Context, deps resource.Dependencies, name resource.Name, conf *Config, logger logging.Logger) (resource.Resource, error) {
	arm1, err := arm.FromDependencies(deps, conf.ArmName)
	if err != nil {
		return nil, err
	}

	controller, err := input.FromDependencies(deps, conf.InputControllerName)
	if err != nil {
		return nil, err
	}

	cancelCtx, cancelFunc := context.WithCancel(context.Background())

	stepSize := conf.StepSize
	if stepSize <= 0 {
		stepSize = defaultStepSize
	}

	arc := &armRemoteControlGamepad{
		name:            name,
		arm:             arm1,
		inputController: controller,
		logger:          logger,
		cfg:             conf,
		cancelCtx:       cancelCtx,
		cancelFunc:      cancelFunc,
		stepSize:        stepSize,
		events:          make(chan struct{}, 1),
	}

	// Initialize current position
	if err := arc.initializePosition(ctx); err != nil {
		return nil, errors.Wrap(err, "failed to initialize arm position")
	}

	// Start event processor
	arc.startEventProcessor()

	// Register input callbacks
	if err := arc.registerCallbacks(ctx); err != nil {
		return nil, errors.Errorf("error with starting remote control service: %q", err)
	}

	return arc, nil
}

func (arc *armRemoteControlGamepad) Name() resource.Name {
	return arc.name
}

func (arc *armRemoteControlGamepad) initializePosition(ctx context.Context) error {
	currentPose, err := arc.arm.EndPosition(ctx, nil)
	if err != nil {
		return errors.Wrap(err, "failed to get current arm position")
	}

	arc.mu.Lock()
	arc.targetPose = currentPose
	arc.initialized = true
	arc.mu.Unlock()

	arc.logger.Infof("Initialized arm remote control at position: %v", currentPose.Point())
	return nil
}

func (arc *armRemoteControlGamepad) startEventProcessor() {
	arc.activeBackgroundWorkers.Add(1)
	utils.ManagedGo(func() {
		arc.eventProcessor()
	}, arc.activeBackgroundWorkers.Done)
}

func (arc *armRemoteControlGamepad) eventProcessor() {
	var currentPose spatialmath.Pose
	var hasMovedOnce bool

	for {
		select {
		case <-arc.cancelCtx.Done():
			return
		case <-arc.events:
			arc.mu.RLock()
			targetPose := arc.targetPose
			initialized := arc.initialized
			arc.mu.RUnlock()

			if !initialized {
				continue
			}

			// Move if this is the first movement or if the target has changed
			shouldMove := !hasMovedOnce || !spatialmath.PoseAlmostEqual(currentPose, targetPose)

			if shouldMove {
				ctx, cancel := context.WithTimeout(arc.cancelCtx, 5*time.Second)
				if err := arc.arm.MoveToPosition(ctx, targetPose, nil); err != nil {
					arc.logger.Errorw("failed to move arm to target position", "error", err, "target", targetPose.Point())
				} else {
					arc.logger.Debugf("Moved arm to position: %v", targetPose.Point())
					currentPose = targetPose
					hasMovedOnce = true
				}
				cancel()
			}
		}
	}
}

func (arc *armRemoteControlGamepad) registerCallbacks(ctx context.Context) error {
	// Movement control callback
	remoteCtl := func(ctx context.Context, event input.Event) {
		if arc.cancelCtx.Err() != nil {
			return
		}
		arc.processMovementEvent(ctx, event)
	}

	// Connect/disconnect callback - stops the arm
	connectDisconnectCtl := func(ctx context.Context, event input.Event) {
		if arc.cancelCtx.Err() != nil {
			return
		}
		arc.logger.Infof("Input controller %s, stopping arm", event.Event)
		if err := arc.arm.Stop(ctx, nil); err != nil {
			arc.logger.Errorw("failed to stop arm on controller disconnect", "error", err)
		}
	}

	// Register movement controls
	movementControls := []input.Control{
		input.AbsoluteHat0X,
		input.AbsoluteHat0Y,
	}

	for _, control := range movementControls {
		if err := arc.inputController.RegisterControlCallback(
			ctx,
			control,
			[]input.EventType{input.PositionChangeAbs},
			remoteCtl,
			map[string]interface{}{},
		); err != nil {
			return err
		}
	}

	// Register button controls for Z-axis
	buttonControls := []input.Control{
		input.ButtonRT,
		input.ButtonLT,
	}

	for _, control := range buttonControls {
		if err := arc.inputController.RegisterControlCallback(
			ctx,
			control,
			[]input.EventType{input.ButtonPress, input.ButtonRelease},
			remoteCtl,
			map[string]interface{}{},
		); err != nil {
			return err
		}
	}

	// Register connect/disconnect handlers for all controls
	allControls := append(movementControls, buttonControls...)
	for _, control := range allControls {
		if err := arc.inputController.RegisterControlCallback(
			ctx,
			control,
			[]input.EventType{input.Connect, input.Disconnect},
			connectDisconnectCtl,
			map[string]interface{}{},
		); err != nil {
			return err
		}
	}

	return nil
}

func (arc *armRemoteControlGamepad) processMovementEvent(ctx context.Context, event input.Event) {
	arc.mu.Lock()
	defer arc.mu.Unlock()

	if !arc.initialized {
		return
	}

	// Get current target position
	point := arc.targetPose.Point()
	orientation := arc.targetPose.Orientation()

	// Apply movement based on control type
	switch event.Control {
	case input.AbsoluteHat0X:
		// Left/right movement - modify X coordinate
		if event.Value != 0 {
			deltaX := event.Value * arc.stepSize
			point.X += deltaX
			arc.logger.Debugf("Hat0X: %f -> Delta X: %f, New X: %f", event.Value, deltaX, point.X)
		}

	case input.AbsoluteHat0Y:
		// Forward/back movement - modify Y coordinate
		if event.Value != 0 {
			deltaY := -event.Value * arc.stepSize // Invert Y for intuitive forward/back
			point.Y += deltaY
			arc.logger.Debugf("Hat0Y: %f -> Delta Y: %f, New Y: %f", event.Value, deltaY, point.Y)
		}

	case input.ButtonRT:
		// Right trigger - move up (increase Z)
		if event.Event == input.ButtonPress {
			point.Z += arc.stepSize
			arc.logger.Debugf("RT pressed -> Z up: %f", point.Z)
		}

	case input.ButtonLT:
		// Left trigger - move down (decrease Z)
		if event.Event == input.ButtonPress {
			point.Z -= arc.stepSize
			arc.logger.Debugf("LT pressed -> Z down: %f", point.Z)
		}

	default:
		return // Ignore unknown controls
	}

	// Update target pose
	arc.targetPose = spatialmath.NewPose(point, orientation)

	// Signal the event processor
	select {
	case arc.events <- struct{}{}:
	default:
	}
}

func (arc *armRemoteControlGamepad) NewClientFromConn(ctx context.Context, conn rpc.ClientConn, remoteName string, name resource.Name, logger logging.Logger) (resource.Resource, error) {
	panic("not implemented")
}

func (arc *armRemoteControlGamepad) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	panic("not implemented")
}

func (arc *armRemoteControlGamepad) Close(context.Context) error {
	// Stop the arm first
	if arc.arm != nil {
		if err := arc.arm.Stop(context.Background(), nil); err != nil {
			arc.logger.Errorw("failed to stop arm during close", "error", err)
		}
	}

	// Cancel background operations
	arc.cancelFunc()

	// Wait for background workers to finish
	arc.activeBackgroundWorkers.Wait()

	return nil
}
