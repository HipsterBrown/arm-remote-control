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
	resource.Named

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
	arm1, err := arm.FromProvider(deps, conf.ArmName)
	if err != nil {
		return nil, err
	}

	controller, err := input.FromProvider(deps, conf.InputControllerName)
	if err != nil {
		return nil, err
	}

	cancelCtx, cancelFunc := context.WithCancel(context.Background())

	stepSize := conf.StepSize
	if stepSize <= 0 {
		stepSize = defaultStepSize
	}

	arc := &armRemoteControlGamepad{
		Named:           name.AsNamed(),
		arm:             arm1,
		inputController: controller,
		logger:          logger,
		cfg:             conf,
		cancelCtx:       cancelCtx,
		cancelFunc:      cancelFunc,
		stepSize:        stepSize,
		events:          make(chan struct{}, 1),
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

	return arc, nil
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

// axisValue returns the latest absolute position for an axis control. A
// control whose most recent event is Connect/Disconnect rather than a
// position reports 0.
func axisValue(events map[input.Control]input.Event, c input.Control) float64 {
	e, ok := events[c]
	if !ok || e.Event != input.PositionChangeAbs {
		return 0
	}
	return e.Value
}

// buttonPressed reports whether a button's most recent event was a press.
func buttonPressed(events map[input.Control]input.Event, c input.Control) bool {
	e, ok := events[c]
	return ok && e.Event == input.ButtonPress
}

// controllerGone reports whether the controller's latest events say it went
// away, which replaces the Disconnect callback this service used to register.
func controllerGone(events map[input.Control]input.Event) bool {
	for _, e := range events {
		if e.Event == input.Disconnect {
			return true
		}
	}
	return false
}

// continuousMovementProcessor drives the arm from the input controller's
// current state at 10Hz.
//
// It polls Events() instead of registering callbacks. RegisterControlCallback
// does not reach a modular input controller: viam-server's StreamEvents
// handler blocks while registering against the module's own client and never
// reaches its forwarding loop, so the registration returns nil and the
// callbacks then silently never fire. Events() is unary and works across the
// module boundary, and this loop only ever needed the latest value per
// control anyway.
func (arc *armRemoteControlGamepad) continuousMovementProcessor() {
	ticker := time.NewTicker(100 * time.Millisecond) // 10Hz movement updates
	defer ticker.Stop()

	connected := true
	for {
		select {
		case <-arc.cancelCtx.Done():
			return
		case <-arc.movementStop:
			return
		case <-ticker.C:
			events, err := arc.inputController.Events(arc.cancelCtx, nil)
			if err != nil {
				arc.logger.Debugw("unable to read input controller events", "error", err)
				continue
			}

			if controllerGone(events) {
				if connected {
					connected = false
					arc.logger.Info("input controller disconnected, stopping arm")
					if err := arc.arm.Stop(arc.cancelCtx, nil); err != nil {
						arc.logger.Errorw("failed to stop arm on controller disconnect", "error", err)
					}
				}
				continue
			}
			connected = true

			arc.mu.RLock()
			initialized := arc.initialized
			stepSize := arc.stepSize
			arc.mu.RUnlock()
			if !initialized {
				continue
			}

			hat0X := axisValue(events, input.AbsoluteHat0X)
			hat0Y := axisValue(events, input.AbsoluteHat0Y)
			rtPressed := buttonPressed(events, input.ButtonRT)
			ltPressed := buttonPressed(events, input.ButtonLT)

			if !rtPressed && !ltPressed && hat0X == 0 && hat0Y == 0 {
				continue
			}

			// Read the arm without holding the lock. This is a serial round
			// trip taking tens of milliseconds, and the previous version held
			// the write lock across it and returned early on error without
			// unlocking, which wedged this loop, the event processor and
			// Close permanently on the first failed read.
			currentPose, err := arc.arm.EndPosition(arc.cancelCtx, nil)
			if err != nil {
				arc.logger.Debugw("unable to get current end position", "error", err)
				continue
			}

			point := currentPose.Point()
			if rtPressed {
				point.Z += stepSize
			}
			if ltPressed {
				point.Z -= stepSize
			}
			point.X += hat0X * stepSize
			point.Y += hat0Y * stepSize
			arc.logger.Debugf("moving to X=%.1f Y=%.1f Z=%.1f (hat %.0f,%.0f rt=%v lt=%v)",
				point.X, point.Y, point.Z, hat0X, hat0Y, rtPressed, ltPressed)

			arc.mu.Lock()
			arc.targetPose = spatialmath.NewPose(point, currentPose.Orientation())
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
