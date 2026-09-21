package armremotecontrol

import (
	"context"
	"sync"
	"time"

	"github.com/pkg/errors"
	commonpb "go.viam.com/api/common/v1"
	motionpb "go.viam.com/api/service/motion/v1"
	"go.viam.com/utils"
	"google.golang.org/protobuf/encoding/protojson"

	"go.viam.com/rdk/components/arm"
	"go.viam.com/rdk/components/input"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/generic"
	"go.viam.com/rdk/services/motion"
	motionbuiltin "go.viam.com/rdk/services/motion/builtin"
	"go.viam.com/rdk/spatialmath"
	"go.viam.com/utils/rpc"
)

var (
	Gamepad          = resource.NewModel("hipsterbrown", "arm-remote-control", "gamepad")
	errUnimplemented = errors.New("unimplemented")
)

const (
	defaultStepSize = 10.0 // mm for all axis movements

	teleopStatusPollInterval = time.Second
	teleopProbeTimeout       = 2 * time.Second
	teleopProbePollInterval  = 100 * time.Millisecond
	// ponytail: fixed threshold for "repeatedly" failing; tune (or make
	// configurable) if this proves too eager or too slow to trip in practice.
	maxConsecutiveTeleopErrs = 3
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
	// MotionServiceName, when set, routes movement through the named motion
	// service's teleop pipeline instead of driving the arm directly. Unset
	// selects the direct arm path (the default).
	MotionServiceName string `json:"motion_service,omitempty"`
	// ReferenceFrame is the frame deltas are expressed in when
	// MotionServiceName is set. Defaults to ArmName. Ignored in direct mode.
	ReferenceFrame string `json:"reference_frame,omitempty"`
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

	if cfg.MotionServiceName != "" {
		deps = append(deps, motion.Named(cfg.MotionServiceName).String())
	}

	return deps, nil, nil
}

// mover applies teleop deltas to the arm. It is the only seam between the
// direct arm.MoveToPosition path and the motion service teleop pipeline; do
// not add other abstractions around it.
type mover interface {
	// step applies a delta. Units are millimetres.
	step(ctx context.Context, dx, dy, dz float64) error
	// stop halts motion. Always also calls arm.Stop where relevant.
	stop(ctx context.Context) error
}

// directMover applies deltas straight to the arm: read arm.EndPosition, add
// the delta, arm.MoveToPosition. Because it always reads the live measured
// pose, it cannot get ahead of the hardware even if calls back up.
type directMover struct {
	arm arm.Arm
}

func (m *directMover) step(ctx context.Context, dx, dy, dz float64) error {
	currentPose, err := m.arm.EndPosition(ctx, nil)
	if err != nil {
		return errors.Wrap(err, "failed to get current arm position")
	}

	point := currentPose.Point()
	point.X += dx
	point.Y += dy
	point.Z += dz

	newPose := spatialmath.NewPose(point, currentPose.Orientation())
	return m.arm.MoveToPosition(ctx, newPose, nil)
}

func (m *directMover) stop(ctx context.Context) error {
	return m.arm.Stop(ctx, nil)
}

// teleopMover drives the arm through the motion service's teleop DoCommand
// pipeline (teleop_start/teleop_move/teleop_stop/teleop_status). Deltas are
// sent as relative poses; the pipeline re-resolves them against its live
// planning head on every replan, so this mover never accumulates a local
// target, matching directMover's "read live, add delta" property.
type teleopMover struct {
	motionSvc     motion.Service
	arm           arm.Arm
	componentName string // also the reference frame deltas are expressed in
	logger        logging.Logger

	mu              sync.Mutex
	consecutiveErrs int
}

// newTeleopMover starts the teleop pipeline for componentName, probes it with
// a zero-delta move to confirm the reference frame actually resolves, and (on
// success) starts a background goroutine that polls teleop_status roughly
// once a second for the lifetime of cancelCtx.
//
// teleop_move always reports success even when the planner is failing, so
// without this probe a bad reference_frame produces a healthy-looking service
// driving a dead arm. Failing construction here turns that into a startup
// error instead.
func newTeleopMover(
	ctx context.Context,
	motionSvc motion.Service,
	armDep arm.Arm,
	componentName string,
	logger logging.Logger,
	cancelCtx context.Context,
	wg *sync.WaitGroup,
) (*teleopMover, error) {
	tm := &teleopMover{
		motionSvc:     motionSvc,
		arm:           armDep,
		componentName: componentName,
		logger:        logger,
	}

	if err := tm.start(ctx); err != nil {
		return nil, errors.Wrap(err, "failed to start teleop pipeline")
	}

	if err := tm.probe(ctx); err != nil {
		return nil, err
	}

	wg.Add(1)
	utils.ManagedGo(func() { tm.pollStatus(cancelCtx) }, wg.Done)

	return tm, nil
}

// teleopMarshalOpts renders protojson with original (snake_case) proto field
// names and includes zero-valued fields, matching the wire examples in
// docs/SPEC-motion-teleop.md.
var teleopMarshalOpts = protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}

// deltaPoseInFrame builds the PoseInFrame proto used as both teleop_start's
// destination and teleop_move's payload. The orientation is always the
// identity orientation vector (o_z=1, theta=0): this mover only ever
// expresses translation, never rotation.
func deltaPoseInFrame(frame string, dx, dy, dz float64) *commonpb.PoseInFrame {
	return &commonpb.PoseInFrame{
		ReferenceFrame: frame,
		Pose: &commonpb.Pose{
			X:  dx,
			Y:  dy,
			Z:  dz,
			OZ: 1,
		},
	}
}

// start (re)registers the teleop pipeline for this component. Per teleop.go,
// teleop_start's value is a JSON string of a protojson MoveRequest, and
// component_name lives *inside* that string -- unlike teleop_move, where it
// is a separate top-level DoCommand key. This asymmetry is real, verified
// against go.viam.com/rdk@v1.7.0/services/motion/builtin/teleop.go, not a
// typo carried over from the spec.
func (m *teleopMover) start(ctx context.Context) error {
	req := &motionpb.MoveRequest{
		ComponentName: m.componentName,
		Destination:   deltaPoseInFrame(m.componentName, 0, 0, 0),
	}
	payload, err := teleopMarshalOpts.Marshal(req)
	if err != nil {
		return errors.Wrap(err, "failed to build teleop_start payload")
	}

	_, err = m.motionSvc.DoCommand(ctx, map[string]interface{}{
		motionbuiltin.DoTeleopStart: string(payload),
	})
	return err
}

// step sends a relative delta as a single teleop_move call. It never
// accumulates a target locally: each call carries only this tick's delta, and
// the pipeline resolves it against its own live planning head.
func (m *teleopMover) step(ctx context.Context, dx, dy, dz float64) error {
	poseBytes, err := teleopMarshalOpts.Marshal(deltaPoseInFrame(m.componentName, dx, dy, dz))
	if err != nil {
		return errors.Wrap(err, "failed to build teleop_move payload")
	}

	_, err = m.motionSvc.DoCommand(ctx, map[string]interface{}{
		motionbuiltin.DoTeleopMove: string(poseBytes),
		"component_name":           m.componentName,
	})
	return err
}

// stop tears down the teleop pipeline and stops the arm. teleop_stop does not
// stop the arm by itself, so the two are always paired here.
func (m *teleopMover) stop(ctx context.Context) error {
	if _, err := m.motionSvc.DoCommand(ctx, map[string]interface{}{motionbuiltin.DoTeleopStop: true}); err != nil {
		m.logger.Warnw("failed to stop teleop pipeline", "error", err)
	}
	return m.arm.Stop(ctx, nil)
}

// probe sends one zero-delta teleop_move and polls teleop_status until
// plan_count advances past its pre-probe baseline or about two seconds
// elapse, then fails if the pipeline reports an error. This is the startup
// trust boundary: a reference_frame that does not exist in the frame system
// fails inside the planner goroutine, is only ever surfaced through
// teleop_status, and would otherwise leave a healthy-looking service driving
// a dead arm.
func (m *teleopMover) probe(ctx context.Context) error {
	baseline, _, err := m.pollOnce(ctx)
	if err != nil {
		return errors.Wrap(err, "teleop startup probe: failed to read baseline status")
	}

	if err := m.step(ctx, 0, 0, 0); err != nil {
		return errors.Wrap(err, "teleop startup probe: failed to send probe move")
	}

	deadline := time.Now().Add(teleopProbeTimeout)
	for time.Now().Before(deadline) {
		planCount, errStr, err := m.pollOnce(ctx)
		if err != nil {
			return errors.Wrap(err, "teleop startup probe: failed to poll status")
		}
		if errStr != "" {
			return errors.Errorf("teleop startup probe: pipeline reported an error: %s", errStr)
		}
		if planCount > baseline {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(teleopProbePollInterval):
		}
	}

	// About two seconds elapsed with no plan_count advance and no reported
	// error. Per spec, only an explicit error fails construction -- a slow
	// first plan is plausible -- but this is also exactly the shape a
	// misconfigured reference_frame could take if it never surfaces through
	// teleop_status in time, so it must not pass without a trace.
	m.logger.Warnf(
		"teleop startup probe: no plan confirmed and no error reported after %s for reference_frame %q; continuing, but the arm may not actually respond to movement",
		teleopProbeTimeout, m.componentName,
	)
	return nil
}

// pollOnce issues a single teleop_status request and extracts the fields the
// mover cares about.
func (m *teleopMover) pollOnce(ctx context.Context) (planCount float64, errStr string, err error) {
	resp, err := m.motionSvc.DoCommand(ctx, map[string]interface{}{motionbuiltin.DoTeleopStatus: true})
	if err != nil {
		return 0, "", err
	}

	status, ok := resp[motionbuiltin.DoTeleopStatus].(map[string]interface{})
	if !ok {
		return 0, "", errors.Errorf("unexpected %s response shape: %#v", motionbuiltin.DoTeleopStatus, resp[motionbuiltin.DoTeleopStatus])
	}

	planCount = toFloat64(status["plan_count"])
	if e, ok := status["error"].(string); ok {
		errStr = e
	}
	return planCount, errStr, nil
}

func toFloat64(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	default:
		return 0
	}
}

// pollStatus runs for the lifetime of ctx, polling teleop_status roughly once
// a second. teleop_move always reports success even when the planner is
// stalled or failing, so this is the only channel through which ongoing plan
// failures surface.
func (m *teleopMover) pollStatus(ctx context.Context) {
	ticker := time.NewTicker(teleopStatusPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkStatus(ctx)
		}
	}
}

func (m *teleopMover) checkStatus(ctx context.Context) {
	resp, err := m.motionSvc.DoCommand(ctx, map[string]interface{}{motionbuiltin.DoTeleopStatus: true})
	if err != nil {
		m.logger.Errorw("failed to poll teleop status", "error", err)
		return
	}

	status, ok := resp[motionbuiltin.DoTeleopStatus].(map[string]interface{})
	if !ok {
		m.logger.Errorw("unexpected teleop_status response shape", "response", resp[motionbuiltin.DoTeleopStatus])
		return
	}

	if errStr, ok := status["error"].(string); ok && errStr != "" {
		m.logger.Errorw("teleop pipeline reported an error", "error", errStr)
		m.recordFailure(ctx)
	} else {
		m.recordSuccess()
	}

	if running, ok := status["running"].(bool); ok && !running {
		// A motion service Reconfigure tears the pipeline down silently.
		m.logger.Warn("teleop pipeline is not running; attempting to re-establish it")
		if err := m.start(ctx); err != nil {
			m.logger.Errorw("failed to re-establish teleop pipeline", "error", err)
		}
	}
}

// recordFailure never falls back to the direct path: changing what "forward"
// means mid-session, while an operator's hand is on the controls, is more
// dangerous than stopping. It only ever stops the arm.
func (m *teleopMover) recordFailure(ctx context.Context) {
	m.mu.Lock()
	m.consecutiveErrs++
	n := m.consecutiveErrs
	m.mu.Unlock()

	if n >= maxConsecutiveTeleopErrs {
		m.logger.Error("teleop pipeline failed repeatedly; stopping arm (not falling back to direct control)")
		if err := m.arm.Stop(ctx, nil); err != nil {
			m.logger.Errorw("failed to stop arm after repeated teleop failures", "error", err)
		}
	}
}

func (m *teleopMover) recordSuccess() {
	m.mu.Lock()
	m.consecutiveErrs = 0
	m.mu.Unlock()
}

// newMover selects and constructs the mover for conf: the direct arm path
// when MotionServiceName is unset, or the motion service teleop pipeline
// otherwise. There is no runtime switching between the two after this call.
func newMover(
	ctx context.Context,
	deps resource.Dependencies,
	conf *Config,
	armDep arm.Arm,
	logger logging.Logger,
	cancelCtx context.Context,
	wg *sync.WaitGroup,
) (mover, error) {
	if conf.MotionServiceName == "" {
		return &directMover{arm: armDep}, nil
	}

	motionSvc, err := motion.FromDependencies(deps, conf.MotionServiceName)
	if err != nil {
		return nil, err
	}

	referenceFrame := conf.ReferenceFrame
	if referenceFrame == "" {
		referenceFrame = conf.ArmName
	}

	return newTeleopMover(ctx, motionSvc, armDep, referenceFrame, logger, cancelCtx, wg)
}

type armRemoteControlGamepad struct {
	resource.AlwaysRebuild
	resource.Named

	arm             arm.Arm
	inputController input.Controller
	mover           mover
	logger          logging.Logger
	cfg             *Config

	cancelCtx  context.Context
	cancelFunc func()

	// State management
	mu          sync.RWMutex
	initialized bool
	stepSize    float64

	// Button state tracking for continuous movement
	movementTicker *time.Ticker
	movementStop   chan struct{}

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
		Named:           name.AsNamed(),
		arm:             arm1,
		inputController: controller,
		logger:          logger,
		cfg:             conf,
		cancelCtx:       cancelCtx,
		cancelFunc:      cancelFunc,
		stepSize:        stepSize,
		movementStop:    make(chan struct{}),
	}

	mv, err := newMover(ctx, deps, conf, arm1, logger, cancelCtx, &arc.activeBackgroundWorkers)
	if err != nil {
		cancelFunc()
		return nil, errors.Wrap(err, "failed to start mover")
	}
	arc.mover = mv

	// Initialize current position
	if err := arc.initializePosition(ctx); err != nil {
		cancelFunc()
		return nil, errors.Wrap(err, "failed to initialize arm position")
	}

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
	arc.initialized = true
	arc.mu.Unlock()

	arc.logger.Infof("Initialized arm remote control at position: %v", currentPose.Point())
	return nil
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

// continuousMovementProcessor is the movement loop. At 10Hz it reads the
// controller's current state, computes this tick's delta from whichever
// buttons and hat axes are held, and passes it to mover.step. It never
// accumulates a target across ticks -- each call is a one-shot relative delta
// -- which is what lets releasing a control stop the arm.
//
// Input is read by polling Events() rather than by registering callbacks.
// RegisterControlCallback does not reach a modular input controller:
// viam-server's StreamEvents handler stalls registering against the module's
// own client and never reaches its forwarding loop, so the registration
// returns nil and the callbacks then silently never fire. Events() is unary
// and works across the module boundary, and this loop only ever needed the
// latest value per control.
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
					// mover.stop, not arm.Stop directly: the teleop mover must
					// also tear down its pipeline, and teleop_stop alone does
					// not halt the arm.
					if err := arc.mover.stop(arc.cancelCtx); err != nil {
						arc.logger.Errorw("failed to stop on controller disconnect", "error", err)
					}
				}
				continue
			}
			connected = true

			arc.mu.RLock()
			initialized := arc.initialized
			arc.mu.RUnlock()
			if !initialized {
				continue
			}

			rtPressed := buttonPressed(events, input.ButtonRT)
			ltPressed := buttonPressed(events, input.ButtonLT)
			hat0X := axisValue(events, input.AbsoluteHat0X)
			hat0Y := axisValue(events, input.AbsoluteHat0Y)

			var dx, dy, dz float64

			// Z-axis movement from buttons
			if rtPressed {
				dz += arc.stepSize
			}
			if ltPressed {
				dz -= arc.stepSize
			}

			// X-axis movement from hat
			if hat0X != 0.0 {
				dx = hat0X * arc.stepSize
			}

			// Y-axis movement from hat
			if hat0Y != 0.0 {
				dy = hat0Y * arc.stepSize
			}

			if dx == 0 && dy == 0 && dz == 0 {
				continue
			}

			stepCtx, cancel := context.WithTimeout(arc.cancelCtx, 5*time.Second)
			err = arc.mover.step(stepCtx, dx, dy, dz)
			cancel()
			if err != nil {
				arc.logger.Errorw("failed to apply movement step", "error", err, "dx", dx, "dy", dy, "dz", dz)
			} else {
				arc.logger.Debugf("Applied movement step: dx=%f dy=%f dz=%f", dx, dy, dz)
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
	// Stop motion first
	if arc.mover != nil {
		if err := arc.mover.stop(context.Background()); err != nil {
			arc.logger.Errorw("failed to stop during close", "error", err)
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
