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

	// Button state tracking for continuous movement
	buttonStates   map[input.Control]bool
	hatValues      map[input.Control]float64 // Track hat control values
	movementTicker *time.Ticker
	movementStop   chan struct{}

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
		buttonStates:    make(map[input.Control]bool),
		hatValues:       make(map[input.Control]float64),
		movementStop:    make(chan struct{}),
	}

	// Initialize current position
	if err := arc.initializePosition(ctx); err != nil {
		return nil, errors.Wrap(err, "failed to initialize arm position")
	}

	// Start event processor
	arc.startEventProcessor()

	// Start continuous movement processor
	arc.startContinuousMovement()

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

func (arc *armRemoteControlGamepad) startContinuousMovement() {
	arc.activeBackgroundWorkers.Add(1)
	utils.ManagedGo(func() {
		arc.continuousMovementProcessor()
	}, arc.activeBackgroundWorkers.Done)
}

func (arc *armRemoteControlGamepad) continuousMovementProcessor() {
	ticker := time.NewTicker(100 * time.Millisecond) // 10Hz movement updates
	defer ticker.Stop()

	for {
		select {
		case <-arc.cancelCtx.Done():
			return
		case <-arc.movementStop:
			return
		case <-ticker.C:
			arc.mu.RLock()
			rtPressed := arc.buttonStates[input.ButtonRT]
			ltPressed := arc.buttonStates[input.ButtonLT]
			hat0X := arc.hatValues[input.AbsoluteHat0X]
			hat0Y := arc.hatValues[input.AbsoluteHat0Y]
			initialized := arc.initialized
			arc.mu.RUnlock()

			if !initialized {
				continue
			}

			// Check if any movement is needed
			hasMovement := rtPressed || ltPressed || hat0X != 0.0 || hat0Y != 0.0
			if !hasMovement {
				continue
			}

			// Apply continuous movement
			arc.mu.Lock()
			currentPose, err := arc.arm.EndPosition(arc.cancelCtx, nil)
			if err != nil {
				arc.logger.Debug("Unable to get current end position")
				continue
			}
			point := currentPose.Point()
			orientation := currentPose.Orientation()

			// Z-axis movement from buttons
			if rtPressed {
				point.Z += arc.stepSize
				arc.logger.Debugf("RT held -> Z up: %f", point.Z)
			}
			if ltPressed {
				point.Z -= arc.stepSize
				arc.logger.Debugf("LT held -> Z down: %f", point.Z)
			}

			// X-axis movement from hat
			if hat0X != 0.0 {
				deltaX := hat0X * arc.stepSize
				point.X += deltaX
				arc.logger.Debugf("Hat0X held: %f -> Delta X: %f, New X: %f", hat0X, deltaX, point.X)
			}

			// Y-axis movement from hat
			if hat0Y != 0.0 {
				deltaY := hat0Y * arc.stepSize
				point.Y += deltaY
				arc.logger.Debugf("Hat0Y held: %f -> Delta Y: %f, New Y: %f", hat0Y, deltaY, point.Y)
			}

			arc.targetPose = spatialmath.NewPose(point, orientation)
			arc.mu.Unlock()

			// Signal the event processor
			select {
			case arc.events <- struct{}{}:
			default:
			}
		}
	}
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

func (arc *armRemoteControlGamepad) processMovementEvent(_ context.Context, event input.Event) {
	arc.mu.Lock()
	defer arc.mu.Unlock()

	if !arc.initialized {
		return
	}

	// Apply movement based on control type
	switch event.Control {
	case input.AbsoluteHat0X:
		// Track hat value for continuous movement
		arc.hatValues[input.AbsoluteHat0X] = event.Value
		if event.Value == 0.0 {
			arc.logger.Debugf("Hat0X released - stopping X movement")
		} else {
			arc.logger.Debugf("Hat0X: %f - starting continuous X movement", event.Value)
		}

	case input.AbsoluteHat0Y:
		// Track hat value for continuous movement
		arc.hatValues[input.AbsoluteHat0Y] = event.Value
		if event.Value == 0.0 {
			arc.logger.Debugf("Hat0Y released - stopping Y movement")
		} else {
			arc.logger.Debugf("Hat0Y: %f - starting continuous Y movement", event.Value)
		}

	case input.ButtonRT:
		// Track button state for continuous movement
		switch event.Event {
		case input.ButtonPress:
			arc.buttonStates[input.ButtonRT] = true
			arc.logger.Debugf("RT pressed - starting continuous Z up movement")
		case input.ButtonRelease:
			arc.buttonStates[input.ButtonRT] = false
			arc.logger.Debugf("RT released - stopping Z up movement")
		}

	case input.ButtonLT:
		// Track button state for continuous movement
		switch event.Event {
		case input.ButtonPress:
			arc.buttonStates[input.ButtonLT] = true
			arc.logger.Debugf("LT pressed - starting continuous Z down movement")
		case input.ButtonRelease:
			arc.buttonStates[input.ButtonLT] = false
			arc.logger.Debugf("LT released - stopping Z down movement")
		}

	default:
		return // Ignore unknown controls
	}

	// All movements are now handled by the continuous processor
	// No need to signal events here as the continuous processor runs at 10Hz
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

	// Stop continuous movement processor
	close(arc.movementStop)

	// Cancel background operations
	arc.cancelFunc()

	// Wait for background workers to finish
	arc.activeBackgroundWorkers.Wait()

	return nil
}
