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
	"google.golang.org/protobuf/types/known/structpb"

	"go.viam.com/rdk/components/arm"
	"go.viam.com/rdk/components/gripper"
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

	defaultMaxContinuousMotion = 30 * time.Second
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
	// Gripper, when set, binds the gripper controls to this component.
	// Unset leaves them as no-ops.
	Gripper string `json:"gripper,omitempty"`
	// RequireEnable is a *bool, not a bool, so that an omitted attribute is
	// distinguishable from an explicit false. A plain bool would default the
	// deadman to DISABLED whenever the attribute is absent, inverting the
	// safe default. nil means true.
	RequireEnable *bool `json:"require_enable,omitempty"`
	// MaxContinuousMotion is the dead-operator timeout in seconds. A pointer,
	// but for a different reason than RequireEnable: an explicit 0 disables
	// the timer, while an omitted field must mean the default, and a plain
	// int cannot tell those apart.
	MaxContinuousMotion *int `json:"max_continuous_motion,omitempty"`
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

	if cfg.Gripper != "" {
		deps = append(deps, cfg.Gripper)
	}

	if cfg.MaxContinuousMotion != nil && *cfg.MaxContinuousMotion < 0 {
		return nil, nil, resource.NewConfigValidationError(path, errors.New("max_continuous_motion must not be negative; use 0 to disable the timer"))
	}

	return deps, nil, nil
}

// resolveRequireEnable defaults an omitted require_enable to true. See the
// field comment: the omission case is the safety-critical one.
func resolveRequireEnable(conf *Config) bool {
	if conf.RequireEnable == nil {
		return true
	}
	return *conf.RequireEnable
}

// resolveMaxContinuousMotion defaults an omitted max_continuous_motion to
// defaultMaxContinuousMotion, while honouring an explicit 0 as "disabled".
// The duration is stored rather than the raw seconds so tests can use
// sub-second timeouts without widening the config surface.
func resolveMaxContinuousMotion(conf *Config) time.Duration {
	if conf.MaxContinuousMotion == nil {
		return defaultMaxContinuousMotion
	}
	return time.Duration(*conf.MaxContinuousMotion) * time.Second
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
	// Plan for position only, leaving orientation unconstrained.
	//
	// A PoseInFrame always carries an orientation, so an identity-rotation
	// delta still asks the planner to hold the tool's current orientation
	// exactly. On an arm with fewer than six degrees of freedom that is
	// generally unsatisfiable while translating: the RoArm-M3 has no wrist
	// yaw, so its base joint sets both the tool's azimuth and the working
	// plane, and any sideways move changes the orientation it was told to
	// hold. The planner then returns zero IK solutions for X and Y while Z
	// still succeeds, because motion along the approach axis stays in plane.
	//
	// The arm driver already passes this on its own MoveToPosition path,
	// which is why direct mode works where teleop mode did not.
	extra, err := structpb.NewStruct(map[string]interface{}{
		"goal_metric_type": "position_only",
	})
	if err != nil {
		return errors.Wrap(err, "failed to build teleop_start extra")
	}
	req := &motionpb.MoveRequest{
		ComponentName: m.componentName,
		Destination:   deltaPoseInFrame(m.componentName, 0, 0, 0),
		Extra:         extra,
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

	// gripper binds the gripper controls, nil when Config.Gripper is unset
	// (left nil until Task 8 wires it up; nothing calls Open/Grab yet).
	gripper gripper.Gripper

	cancelCtx  context.Context
	cancelFunc func()

	// State management
	mu          sync.RWMutex
	initialized bool

	// estopped is written by tick, not an RPC handler, but is the natural
	// thing for a future DoCommand to expose, so -- unlike connected and
	// motionSince below -- it lives under mu rather than being loop-owned.
	estopped bool

	// stepSize, requireEnable, and maxContinuousMotion are resolved once from
	// config in NewGamepad and never written again, so tick reads them
	// lock-free even though they sit in this mu-guarded block. Do not add a
	// field here that is written after construction without also giving it
	// mu protection.
	stepSize            float64
	requireEnable       bool
	maxContinuousMotion time.Duration

	// analogTriggers records whether this pad reports AbsoluteZ/AbsoluteRZ.
	// Several mappings in gamepad_mappings_linux.go do not, exposing their
	// triggers only as the digital ButtonLT2/ButtonRT2. Resolved once at
	// construction rather than probed per tick.
	//
	// This does not react to a bare USB swap for a different pad model with
	// no reconfigure (resource.AlwaysRebuild covers a reconfigure, not that).
	// Both stale directions fail toward no-Z-motion, never runaway: cached
	// true against a digital-trigger pad means the analog axes are simply
	// absent from events, so axisValue returns 0; cached false against an
	// analog pad means the digital trigger buttons it checks are never
	// pressed. Either way Z stays quiet rather than moving on its own.
	analogTriggers bool

	// Button state tracking for continuous movement
	movementTicker *time.Ticker
	movementStop   chan struct{}

	// Loop-owned state. Written only by tick, which runs solely on the
	// movement goroutine, so it needs no lock.
	connected bool

	// motionSince is the start of the current unbroken run of commanded
	// motion, zero when idle. Loop-owned; see the dead-operator timer.
	motionSince time.Time

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
		Named:               name.AsNamed(),
		arm:                 arm1,
		inputController:     controller,
		logger:              logger,
		cfg:                 conf,
		cancelCtx:           cancelCtx,
		cancelFunc:          cancelFunc,
		stepSize:            stepSize,
		requireEnable:       resolveRequireEnable(conf),
		maxContinuousMotion: resolveMaxContinuousMotion(conf),
		analogTriggers:      hasAnalogTriggers(ctx, controller, logger),
		movementStop:        make(chan struct{}),
		connected:           true,
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

// buttonPressed reports whether a button is currently down.
// ButtonHold counts: gamepad_linux.go maps evdev autorepeat (value 2) to
// ButtonHold, so on a pad that autorepeats, a genuinely held button would
// otherwise read as released.
func buttonPressed(events map[input.Control]input.Event, c input.Control) bool {
	e, ok := events[c]
	return ok && (e.Event == input.ButtonPress || e.Event == input.ButtonHold)
}

// hasAnalogTriggers reports whether the controller exposes both analog
// trigger axes. A controller that errors is assumed to have them: the
// analog path is the common case, and the digital fallback is for known-odd
// pads, not for transient RPC failures.
func hasAnalogTriggers(ctx context.Context, c input.Controller, logger logging.Logger) bool {
	controls, err := c.Controls(ctx, nil)
	if err != nil {
		logger.Warnw("could not read controller controls; assuming analog triggers", "error", err)
		return true
	}

	var z, rz bool
	for _, ctrl := range controls {
		switch ctrl {
		case input.AbsoluteZ:
			z = true
		case input.AbsoluteRZ:
			rz = true
		}
	}
	if !z || !rz {
		logger.Info("controller reports no analog triggers; using ButtonLT2/ButtonRT2 for Z")
	}
	return z && rz
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

// continuousMovementProcessor is the movement loop: it drives a 10Hz ticker,
// reads the controller's current state each tick, and delegates to tick.
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

	for {
		select {
		case <-arc.cancelCtx.Done():
			return
		case <-arc.movementStop:
			return
		case <-ticker.C:
			events, err := arc.inputController.Events(arc.cancelCtx, nil)
			if err != nil {
				// No command is sent this tick, which is the safe outcome:
				// a controller we cannot read is one we do not obey.
				arc.logger.Debugw("unable to read input controller events", "error", err)
				continue
			}
			arc.tick(arc.cancelCtx, events, time.Now())
		}
	}
}

// tick resolves one poll of controller state into the actions it implies. It
// runs a sequence of gates (controller-gone, deadman, ...) that can each
// short-circuit the tick before any delta is computed, then -- if nothing
// gated -- computes this tick's delta from whichever buttons and hat axes
// are held and passes it to mover.step. It never accumulates a target across
// ticks -- each call is a one-shot relative delta -- which is what lets
// releasing a control stop the arm.
//
// It is separated from the loop above so that tests can drive it
// synchronously with canned events and a controlled clock, rather than
// through a real ticker.
func (arc *armRemoteControlGamepad) tick(ctx context.Context, events map[input.Control]input.Event, now time.Time) {
	if controllerGone(events) {
		if arc.connected {
			arc.connected = false
			arc.logger.Info("input controller disconnected, stopping arm")
			// Force it: motionSince zero does not mean the arm is
			// stationary (an idle tick clears it without stopping
			// anything, and in teleop mode the pipeline stays live
			// independently of whether we're issuing moves), and a
			// disconnect is a fault, not a normal idle -- it must stop
			// the arm regardless of what our own bookkeeping believes.
			arc.forceHalt(ctx, now)
		}
		return
	}
	arc.connected = true

	// E-stop outranks every other gate, including the deadman (see
	// docs/SPEC-teleop-safety-gripper.md "Gating order"). Latch on
	// ButtonMenu or ButtonEStop; log, halt, and stop the gripper only on the
	// latching transition, not on every tick it stays held.
	if buttonPressed(events, input.ButtonMenu) || buttonPressed(events, input.ButtonEStop) {
		arc.mu.Lock()
		alreadyLatched := arc.estopped
		arc.estopped = true
		arc.mu.Unlock()

		if !alreadyLatched {
			arc.logger.Error("E-STOP engaged; press Start to clear")
			// Force it: an E-stop must stop the arm regardless of what
			// motionSince claims about current commanded motion.
			arc.forceHalt(ctx, now)
			arc.stopGripper(ctx)
		}
		return
	}

	arc.mu.RLock()
	estopped := arc.estopped
	arc.mu.RUnlock()
	if estopped {
		// Latched. ButtonStart is the only input read until it clears; no
		// other input is read while latched.
		if buttonPressed(events, input.ButtonStart) {
			arc.mu.Lock()
			arc.estopped = false
			arc.mu.Unlock()
			arc.logger.Info("E-stop cleared")
		}
		return
	}

	arc.mu.RLock()
	initialized := arc.initialized
	arc.mu.RUnlock()
	if !initialized {
		return
	}

	// Gates run most authoritative first (see
	// docs/SPEC-teleop-safety-gripper.md "Gating order"): controller gone,
	// then the E-stop gates above, then this deadman gate. Task 7's
	// dead-operator timer is subordinate to the deadman and belongs
	// immediately BELOW it. Insert new gates by authority, not convenience.
	if arc.requireEnable && !buttonPressed(events, input.ButtonLT) {
		arc.haltMotion(ctx)
		return
	}

	dx := axisValue(events, input.AbsoluteHat0X) * arc.stepSize
	dy := axisValue(events, input.AbsoluteHat0Y) * arc.stepSize
	dz := arc.zAxis(events) * arc.stepSize

	if dx == 0 && dy == 0 && dz == 0 {
		// An idle tick ends the run. See the spec: the timer exists to catch
		// a controller that went away, not an operator who paused.
		arc.motionSince = time.Time{}
		return
	}

	if arc.motionSince.IsZero() {
		arc.motionSince = now
	}

	stepCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := arc.mover.step(stepCtx, dx, dy, dz)
	cancel()
	if err != nil {
		arc.logger.Errorw("failed to apply movement step", "error", err, "dx", dx, "dy", dy, "dz", dz)
	} else {
		arc.logger.Debugf("Applied movement step: dx=%f dy=%f dz=%f", dx, dy, dz)
	}
}

// haltMotion stops the mover if a run of commanded motion is in flight, and
// clears motionSince. Calling it while already idle is a no-op: that is what
// keeps a released deadman from calling mover.stop on every subsequent tick,
// which matters because in teleop mode stop tears down and re-establishes the
// whole motion-service pipeline. In short, haltMotion is "stop if we were
// moving" -- see forceHalt below for "stop regardless", which fault
// conditions need instead.
//
// This is meant to be the one chokepoint for "stop, and remember we
// stopped": every loop-side stop -- the deadman gate, the E-stop gate,
// Task 7's dead-operator timer, and the controller-disconnect branch --
// routes through here (the latter two via forceHalt) rather than calling
// arc.mover.stop directly. Close's teardown call is the one exception, since
// by then there is no motionSince left to keep consistent.
//
// mover.stop, not arm.Stop directly, is what every one of those call sites
// gets by routing through here: the teleop mover must also tear down its
// pipeline, and teleop_stop alone does not halt the arm.
//
// haltMotion is movement-goroutine-only: it writes motionSince, which is
// unguarded loop-owned state (see the field comment), with no mutex. Do not
// call it from DoCommand or any other goroutine. The spec anticipates
// exposing the E-stop over DoCommand; wiring that straight into haltMotion
// would look like the obvious move -- this is the designated stop path --
// but it would race motionSince against the movement loop and could call
// mover.stop concurrently with a step already in flight. An RPC-triggered
// stop needs to set a mu-guarded (or atomic) flag for tick itself to poll
// and act on, not call this helper directly.
func (arc *armRemoteControlGamepad) haltMotion(ctx context.Context) {
	if arc.motionSince.IsZero() {
		return
	}
	arc.motionSince = time.Time{}
	if err := arc.mover.stop(ctx); err != nil {
		arc.logger.Errorw("failed to stop motion", "error", err)
	}
}

// forceHalt halts unconditionally: it sets motionSince to now, so
// haltMotion's guard always sees a run in flight and always calls
// mover.stop, regardless of what motionSince previously claimed.
//
// This is for fault conditions only -- currently the E-stop gate and the
// controller-disconnect branch. Both need it for the same reason:
// motionSince == 0 means "this service isn't currently commanding motion",
// which is weaker than "the arm is stationary". An idle tick clears
// motionSince without stopping anything, and in teleop mode the pipeline
// established by teleop_start stays live independently of whether we're
// sending teleop_move calls. A fault must not read either of those as
// "nothing to stop".
func (arc *armRemoteControlGamepad) forceHalt(ctx context.Context, now time.Time) {
	arc.motionSince = now
	arc.haltMotion(ctx)
}

// stopGripper halts the gripper if one is configured. Unlike the grab/open
// path (Task 8) it does not wait on gripperBusy: interrupting an in-flight
// gripper operation is the entire point of an E-stop.
func (arc *armRemoteControlGamepad) stopGripper(ctx context.Context) {
	if arc.gripper == nil {
		return
	}
	if err := arc.gripper.Stop(ctx, nil); err != nil {
		arc.logger.Errorw("failed to stop gripper", "error", err)
	}
}

// zAxis returns the Z command in the range -1..1, from the analog triggers
// where the pad has them and the digital triggers where it does not.
// gamepad_linux.go scales AbsoluteZ/AbsoluteRZ to 0..1, so the difference
// spans -1..1 with both-pressed cancelling to zero.
func (arc *armRemoteControlGamepad) zAxis(events map[input.Control]input.Event) float64 {
	if arc.analogTriggers {
		return axisValue(events, input.AbsoluteRZ) - axisValue(events, input.AbsoluteZ)
	}

	var z float64
	if buttonPressed(events, input.ButtonRT2) {
		z++
	}
	if buttonPressed(events, input.ButtonLT2) {
		z--
	}
	return z
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
