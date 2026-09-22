package armremotecontrol

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/golang/geo/r3"
	"github.com/pkg/errors"
	commonpb "go.viam.com/api/common/v1"
	motionpb "go.viam.com/api/service/motion/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"go.viam.com/rdk/components/arm"
	"go.viam.com/rdk/components/input"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/motion"
	motionbuiltin "go.viam.com/rdk/services/motion/builtin"
	"go.viam.com/rdk/spatialmath"
)

// fakeArm satisfies arm.Arm. Embedding a nil arm.Arm promotes the rest of the
// interface's methods so this only has to override what the tests exercise;
// calling anything else would panic on the nil embedded interface, which is
// fine since these tests never do.
type fakeArm struct {
	arm.Arm

	mu        sync.Mutex
	endPos    spatialmath.Pose
	moveCalls []spatialmath.Pose
	stopCalls int
}

func (f *fakeArm) EndPosition(ctx context.Context, extra map[string]interface{}) (spatialmath.Pose, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.endPos, nil
}

func (f *fakeArm) MoveToPosition(ctx context.Context, pose spatialmath.Pose, extra map[string]interface{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.moveCalls = append(f.moveCalls, pose)
	return nil
}

func (f *fakeArm) Stop(ctx context.Context, extra map[string]interface{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalls++
	return nil
}

func (f *fakeArm) getStopCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopCalls
}

func (f *fakeArm) getMoveCalls() []spatialmath.Pose {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]spatialmath.Pose, len(f.moveCalls))
	copy(out, f.moveCalls)
	return out
}

// fakeMotionService satisfies motion.Service, recording every DoCommand call
// and delegating the response to a test-supplied handler.
type fakeMotionService struct {
	motion.Service

	mu      sync.Mutex
	calls   []map[string]interface{}
	handler func(cmd map[string]interface{}) (map[string]interface{}, error)
}

func (f *fakeMotionService) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	f.mu.Lock()
	f.calls = append(f.calls, cmd)
	f.mu.Unlock()

	if f.handler != nil {
		return f.handler(cmd)
	}
	return map[string]interface{}{}, nil
}

func (f *fakeMotionService) callsFor(key string) []map[string]interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]interface{}
	for _, c := range f.calls {
		if _, ok := c[key]; ok {
			out = append(out, c)
		}
	}
	return out
}

// healthyStatusHandler simulates a teleop pipeline that always plans
// successfully and increments plan_count on every teleop_move.
func healthyStatusHandler() func(cmd map[string]interface{}) (map[string]interface{}, error) {
	var planCount float64
	var mu sync.Mutex
	return func(cmd map[string]interface{}) (map[string]interface{}, error) {
		if _, ok := cmd[motionbuiltin.DoTeleopMove]; ok {
			mu.Lock()
			planCount++
			mu.Unlock()
		}
		if _, ok := cmd[motionbuiltin.DoTeleopStatus]; ok {
			mu.Lock()
			pc := planCount
			mu.Unlock()
			return map[string]interface{}{
				motionbuiltin.DoTeleopStatus: map[string]interface{}{
					"running":    true,
					"plan_count": pc,
				},
			}, nil
		}
		return map[string]interface{}{}, nil
	}
}

func newTestLogger(t *testing.T) logging.Logger {
	return logging.NewTestLogger(t)
}

// unaryPoseInFrame parses a teleop payload's PoseInFrame value using
// protojson, so a malformed command (wrong field name, wrong type, bad
// nesting) is caught rather than only string-matched.
func unaryPoseInFrame(t *testing.T, raw string) *commonpb.PoseInFrame {
	t.Helper()
	pif := &commonpb.PoseInFrame{}
	if err := protojson.Unmarshal([]byte(raw), pif); err != nil {
		t.Fatalf("payload did not parse as protojson PoseInFrame: %v\npayload: %s", err, raw)
	}
	return pif
}

// unaryMoveRequest parses teleop_start's payload using protojson into the
// real pb.MoveRequest, so a malformed command is caught rather than only
// string-matched.
func unaryMoveRequest(t *testing.T, raw string) *motionpb.MoveRequest {
	t.Helper()
	req := &motionpb.MoveRequest{}
	if err := protojson.Unmarshal([]byte(raw), req); err != nil {
		t.Fatalf("payload did not parse as protojson MoveRequest: %v\npayload: %s", err, raw)
	}
	return req
}

func TestDirectModeSelectedWhenMotionServiceUnset(t *testing.T) {
	fa := &fakeArm{endPos: spatialmath.NewZeroPose()}
	logger := newTestLogger(t)
	ctx := context.Background()
	var wg sync.WaitGroup

	mv, err := newMover(ctx, resource.Dependencies{}, &Config{ArmName: "arm-1"}, fa, logger, ctx, &wg)
	if err != nil {
		t.Fatalf("newMover: %v", err)
	}
	if _, ok := mv.(*directMover); !ok {
		t.Fatalf("expected *directMover, got %T", mv)
	}

	// Behaviour unchanged: read EndPosition, add the delta, MoveToPosition.
	fa.endPos = spatialmath.NewPose(r3.Vector{X: 1, Y: 2, Z: 3}, spatialmath.NewZeroOrientation())
	if err := mv.step(ctx, 10, -5, 0); err != nil {
		t.Fatalf("step: %v", err)
	}
	calls := fa.getMoveCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 MoveToPosition call, got %d", len(calls))
	}
	got := calls[0].Point()
	if got.X != 11 || got.Y != -3 || got.Z != 3 {
		t.Fatalf("expected point (11,-3,3), got (%v,%v,%v)", got.X, got.Y, got.Z)
	}

	if err := mv.stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if fa.getStopCalls() != 1 {
		t.Fatalf("expected arm.Stop to be called once, got %d", fa.getStopCalls())
	}
}

func TestTeleopModeIssuesStartOnceWithComponentNameAndReferenceFrame(t *testing.T) {
	fa := &fakeArm{}
	fms := &fakeMotionService{handler: healthyStatusHandler()}
	logger := newTestLogger(t)
	ctx := context.Background()
	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	defer wg.Wait()

	deps := resource.Dependencies{motion.Named("motion-1"): fms}
	conf := &Config{ArmName: "arm-1", MotionServiceName: "motion-1", ReferenceFrame: "gripper-1"}

	mv, err := newMover(ctx, deps, conf, fa, logger, cancelCtx, &wg)
	if err != nil {
		t.Fatalf("newMover: %v", err)
	}
	// Stop the background poller before the test asserts against call counts,
	// so it can't record extra teleop_status calls mid-assertion.
	cancel()

	if _, ok := mv.(*teleopMover); !ok {
		t.Fatalf("expected *teleopMover, got %T", mv)
	}

	startCalls := fms.callsFor(motionbuiltin.DoTeleopStart)
	if len(startCalls) != 1 {
		t.Fatalf("expected exactly 1 teleop_start call, got %d", len(startCalls))
	}

	payload, ok := startCalls[0][motionbuiltin.DoTeleopStart].(string)
	if !ok {
		t.Fatalf("teleop_start value is not a string: %#v", startCalls[0][motionbuiltin.DoTeleopStart])
	}

	// The full envelope, including component_name, now round-trips through
	// protojson into the real pb.MoveRequest (component_name is a plain
	// string field as of go.viam.com/api v0.1.579 / rdk v1.7.0).
	req := unaryMoveRequest(t, payload)
	if req.GetComponentName() != "gripper-1" {
		t.Fatalf("expected component_name gripper-1, got %q", req.GetComponentName())
	}
	if req.GetDestination().GetReferenceFrame() != "gripper-1" {
		t.Fatalf("expected destination.reference_frame gripper-1, got %q", req.GetDestination().GetReferenceFrame())
	}
}

// A 5-DoF arm cannot hold its tool orientation through a sideways move, so
// the teleop goal must be position-only or X/Y deltas yield zero IK solutions.
func TestTeleopStartRequestsPositionOnlyGoal(t *testing.T) {
	fa := &fakeArm{}
	fms := &fakeMotionService{handler: healthyStatusHandler()}
	logger := newTestLogger(t)
	ctx := context.Background()
	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	defer wg.Wait()

	deps := resource.Dependencies{motion.Named("motion-1"): fms}
	conf := &Config{ArmName: "arm-1", MotionServiceName: "motion-1", ReferenceFrame: "gripper-1"}

	if _, err := newMover(ctx, deps, conf, fa, logger, cancelCtx, &wg); err != nil {
		t.Fatalf("newMover: %v", err)
	}
	cancel()

	startCalls := fms.callsFor(motionbuiltin.DoTeleopStart)
	if len(startCalls) != 1 {
		t.Fatalf("expected exactly 1 teleop_start call, got %d", len(startCalls))
	}
	payload, ok := startCalls[0][motionbuiltin.DoTeleopStart].(string)
	if !ok {
		t.Fatalf("teleop_start value is not a string: %#v", startCalls[0][motionbuiltin.DoTeleopStart])
	}

	req := unaryMoveRequest(t, payload)
	if got := req.GetExtra().AsMap()["goal_metric_type"]; got != "position_only" {
		t.Fatalf("goal_metric_type = %v, want \"position_only\"", got)
	}
}

func TestStepEmitsTeleopMoveWithDeltaAndTopLevelComponentName(t *testing.T) {
	fa := &fakeArm{}
	fms := &fakeMotionService{handler: healthyStatusHandler()}
	logger := newTestLogger(t)
	ctx := context.Background()
	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	defer wg.Wait()

	deps := resource.Dependencies{motion.Named("motion-1"): fms}
	conf := &Config{ArmName: "arm-1", MotionServiceName: "motion-1", ReferenceFrame: "gripper-1"}

	mv, err := newMover(ctx, deps, conf, fa, logger, cancelCtx, &wg)
	if err != nil {
		t.Fatalf("newMover: %v", err)
	}
	cancel()

	if err := mv.step(ctx, 0, 0, 10); err != nil {
		t.Fatalf("step: %v", err)
	}

	moveCalls := fms.callsFor(motionbuiltin.DoTeleopMove)
	// The startup probe itself issues one zero-delta teleop_move; our explicit
	// step is the last one.
	if len(moveCalls) < 1 {
		t.Fatalf("expected at least 1 teleop_move call, got %d", len(moveCalls))
	}
	last := moveCalls[len(moveCalls)-1]

	componentName, ok := last["component_name"].(string)
	if !ok || componentName != "gripper-1" {
		t.Fatalf("expected top-level component_name gripper-1, got %#v", last["component_name"])
	}

	payload, ok := last[motionbuiltin.DoTeleopMove].(string)
	if !ok {
		t.Fatalf("teleop_move value is not a string: %#v", last[motionbuiltin.DoTeleopMove])
	}
	pif := unaryPoseInFrame(t, payload)
	if pif.GetReferenceFrame() != "gripper-1" {
		t.Fatalf("expected reference_frame gripper-1, got %q", pif.GetReferenceFrame())
	}
	if pif.GetPose().GetZ() != 10 {
		t.Fatalf("expected pose.z 10, got %v", pif.GetPose().GetZ())
	}
	if pif.GetPose().GetX() != 0 || pif.GetPose().GetY() != 0 {
		t.Fatalf("expected zero x/y delta, got (%v,%v)", pif.GetPose().GetX(), pif.GetPose().GetY())
	}
}

func TestStepDeltasAreRelativeNotAccumulating(t *testing.T) {
	fa := &fakeArm{}
	fms := &fakeMotionService{handler: healthyStatusHandler()}
	logger := newTestLogger(t)
	ctx := context.Background()
	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	defer wg.Wait()

	deps := resource.Dependencies{motion.Named("motion-1"): fms}
	conf := &Config{ArmName: "arm-1", MotionServiceName: "motion-1"}

	mv, err := newMover(ctx, deps, conf, fa, logger, cancelCtx, &wg)
	if err != nil {
		t.Fatalf("newMover: %v", err)
	}
	cancel()

	if err := mv.step(ctx, 5, 0, 0); err != nil {
		t.Fatalf("step 1: %v", err)
	}
	if err := mv.step(ctx, 5, 0, 0); err != nil {
		t.Fatalf("step 2: %v", err)
	}

	moveCalls := fms.callsFor(motionbuiltin.DoTeleopMove)
	if len(moveCalls) < 2 {
		t.Fatalf("expected at least 2 teleop_move calls, got %d", len(moveCalls))
	}
	lastTwo := moveCalls[len(moveCalls)-2:]
	pif1 := unaryPoseInFrame(t, lastTwo[0][motionbuiltin.DoTeleopMove].(string))
	pif2 := unaryPoseInFrame(t, lastTwo[1][motionbuiltin.DoTeleopMove].(string))

	if pif1.GetPose().GetX() != 5 || pif2.GetPose().GetX() != 5 {
		t.Fatalf("expected both steps to carry delta x=5 (not accumulating), got %v then %v",
			pif1.GetPose().GetX(), pif2.GetPose().GetX())
	}
}

func TestReferenceFrameDefaultsToArmName(t *testing.T) {
	fa := &fakeArm{}
	fms := &fakeMotionService{handler: healthyStatusHandler()}
	logger := newTestLogger(t)
	ctx := context.Background()
	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	defer wg.Wait()

	deps := resource.Dependencies{motion.Named("motion-1"): fms}
	conf := &Config{ArmName: "arm-1", MotionServiceName: "motion-1"} // no ReferenceFrame set

	mv, err := newMover(ctx, deps, conf, fa, logger, cancelCtx, &wg)
	if err != nil {
		t.Fatalf("newMover: %v", err)
	}
	cancel()

	tm, ok := mv.(*teleopMover)
	if !ok {
		t.Fatalf("expected *teleopMover, got %T", mv)
	}
	if tm.componentName != "arm-1" {
		t.Fatalf("expected reference frame to default to arm name arm-1, got %q", tm.componentName)
	}
}

func TestStartupFailsWhenProbeReportsError(t *testing.T) {
	fa := &fakeArm{}
	fms := &fakeMotionService{
		handler: func(cmd map[string]interface{}) (map[string]interface{}, error) {
			if _, ok := cmd[motionbuiltin.DoTeleopStatus]; ok {
				return map[string]interface{}{
					motionbuiltin.DoTeleopStatus: map[string]interface{}{
						"running":    true,
						"plan_count": float64(0),
						"error":      "reference frame gripper-9000 does not exist",
					},
				}, nil
			}
			return map[string]interface{}{}, nil
		},
	}
	logger := newTestLogger(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	defer wg.Wait()

	deps := resource.Dependencies{motion.Named("motion-1"): fms}
	conf := &Config{ArmName: "arm-1", MotionServiceName: "motion-1", ReferenceFrame: "gripper-9000"}

	_, err := newMover(ctx, deps, conf, fa, logger, ctx, &wg)
	if err == nil {
		t.Fatal("expected construction to fail when the teleop probe reports an error")
	}
}

func TestStatusErrorDuringOperationLogsAndDoesNotSwitchModes(t *testing.T) {
	fa := &fakeArm{}
	fms := &fakeMotionService{handler: healthyStatusHandler()}
	logger := newTestLogger(t)
	ctx := context.Background()
	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	defer wg.Wait()

	deps := resource.Dependencies{motion.Named("motion-1"): fms}
	conf := &Config{ArmName: "arm-1", MotionServiceName: "motion-1", ReferenceFrame: "gripper-1"}

	mv, err := newMover(ctx, deps, conf, fa, logger, cancelCtx, &wg)
	if err != nil {
		t.Fatalf("newMover: %v", err)
	}
	// Stop the background poller so the test drives checkStatus deterministically.
	cancel()

	tm, ok := mv.(*teleopMover)
	if !ok {
		t.Fatalf("expected *teleopMover, got %T", mv)
	}

	// Simulate the pipeline reporting an error on every poll.
	fms.mu.Lock()
	fms.handler = func(cmd map[string]interface{}) (map[string]interface{}, error) {
		if _, ok := cmd[motionbuiltin.DoTeleopStatus]; ok {
			return map[string]interface{}{
				motionbuiltin.DoTeleopStatus: map[string]interface{}{
					"running": true,
					"error":   "planner stalled",
				},
			}, nil
		}
		return map[string]interface{}{}, nil
	}
	fms.mu.Unlock()

	for i := 0; i < maxConsecutiveTeleopErrs; i++ {
		tm.checkStatus(ctx)
	}

	if fa.getStopCalls() == 0 {
		t.Fatal("expected arm.Stop to be called after repeated teleop failures")
	}

	// No fallback: the mover is still the teleop implementation, and applying
	// a step still goes through the motion service, never arm.MoveToPosition.
	if _, ok := mv.(*teleopMover); !ok {
		t.Fatalf("expected mover to remain *teleopMover after failures, got %T", mv)
	}
	if err := mv.step(ctx, 1, 0, 0); err != nil {
		t.Fatalf("step: %v", err)
	}
	if len(fa.getMoveCalls()) != 0 {
		t.Fatal("expected teleop mode to never call arm.MoveToPosition directly")
	}
}

func TestStopCallsBothTeleopStopAndArmStop(t *testing.T) {
	fa := &fakeArm{}
	fms := &fakeMotionService{handler: healthyStatusHandler()}
	logger := newTestLogger(t)
	ctx := context.Background()
	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	defer wg.Wait()

	deps := resource.Dependencies{motion.Named("motion-1"): fms}
	conf := &Config{ArmName: "arm-1", MotionServiceName: "motion-1", ReferenceFrame: "gripper-1"}

	mv, err := newMover(ctx, deps, conf, fa, logger, cancelCtx, &wg)
	if err != nil {
		t.Fatalf("newMover: %v", err)
	}
	cancel()

	if err := mv.stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if len(fms.callsFor(motionbuiltin.DoTeleopStop)) != 1 {
		t.Fatalf("expected exactly 1 teleop_stop call, got %d", len(fms.callsFor(motionbuiltin.DoTeleopStop)))
	}
	if fa.getStopCalls() != 1 {
		t.Fatalf("expected arm.Stop to be called once, got %d", fa.getStopCalls())
	}
}

// A misconfigured reference_frame is expected to surface as an error well
// within the probe timeout (see teleop.go: planTeleopMulti sets lastErr on
// every failed plan attempt). This covers the residual gap: if the pipeline
// never advances plan_count and never reports an error either, construction
// must still succeed (a slow first plan is plausible) but must not pass
// silently.
func TestProbeTimesOutWithoutAdvanceOrErrorSucceedsButWarns(t *testing.T) {
	fa := &fakeArm{}
	fms := &fakeMotionService{
		handler: func(cmd map[string]interface{}) (map[string]interface{}, error) {
			if _, ok := cmd[motionbuiltin.DoTeleopStatus]; ok {
				return map[string]interface{}{
					motionbuiltin.DoTeleopStatus: map[string]interface{}{"running": true, "plan_count": float64(0)},
				}, nil
			}
			return map[string]interface{}{}, nil
		},
	}
	logger := newTestLogger(t)
	ctx := context.Background()

	tm := &teleopMover{motionSvc: fms, arm: fa, componentName: "gripper-1", logger: logger}
	start := time.Now()
	err := tm.probe(ctx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("expected probe to succeed (logging a warning, not failing) on a plain timeout, got: %v", err)
	}
	if elapsed < teleopProbeTimeout {
		t.Fatalf("expected probe to wait out the full timeout (%s) before giving up, only waited %s", teleopProbeTimeout, elapsed)
	}
}

// Guard against a probe that never terminates: exercised for completeness
// only, not part of the spec's required list.
func TestProbeRespectsContextCancellation(t *testing.T) {
	fa := &fakeArm{}
	fms := &fakeMotionService{
		handler: func(cmd map[string]interface{}) (map[string]interface{}, error) {
			if _, ok := cmd[motionbuiltin.DoTeleopStatus]; ok {
				return map[string]interface{}{
					motionbuiltin.DoTeleopStatus: map[string]interface{}{"running": true, "plan_count": float64(0)},
				}, nil
			}
			return map[string]interface{}{}, nil
		},
	}
	logger := newTestLogger(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	tm := &teleopMover{motionSvc: fms, arm: fa, componentName: "gripper-1", logger: logger}
	err := tm.probe(ctx)
	if err == nil {
		t.Fatal("expected probe to return an error when its context is cancelled")
	}
}

func TestButtonPressedAcceptsHold(t *testing.T) {
	events := map[input.Control]input.Event{
		input.ButtonSouth: {Event: input.ButtonPress, Control: input.ButtonSouth},
		input.ButtonEast:  {Event: input.ButtonHold, Control: input.ButtonEast},
		input.ButtonWest:  {Event: input.ButtonRelease, Control: input.ButtonWest},
	}

	if !buttonPressed(events, input.ButtonSouth) {
		t.Fatalf("expected ButtonPress to count as pressed")
	}
	if !buttonPressed(events, input.ButtonEast) {
		t.Fatalf("expected ButtonHold to count as pressed")
	}
	if buttonPressed(events, input.ButtonWest) {
		t.Fatalf("expected ButtonRelease not to count as pressed")
	}
	if buttonPressed(events, input.ButtonNorth) {
		t.Fatalf("expected an absent control not to count as pressed")
	}
}

// fakeMover records what the tick logic asked for, standing in for either
// real mover.
type fakeMover struct {
	mu        sync.Mutex
	stepCalls [][3]float64
	stopCalls int
}

func (f *fakeMover) step(ctx context.Context, dx, dy, dz float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stepCalls = append(f.stepCalls, [3]float64{dx, dy, dz})
	return nil
}

func (f *fakeMover) stop(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalls++
	return nil
}

func (f *fakeMover) steps() [][3]float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][3]float64, len(f.stepCalls))
	copy(out, f.stepCalls)
	return out
}

func (f *fakeMover) stops() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopCalls
}

// newTestGamepad builds a service wired to fakes, bypassing NewGamepad so
// tests can drive tick directly without a running goroutine.
//
// maxContinuousMotion is deliberately left at its zero value, which disables
// the dead-operator timer: most tests have nothing to do with it, and a
// default-on timer measured against wall-clock time would make them flaky.
// Tests that exercise the timer opt in explicitly via arc.maxContinuousMotion.
func newTestGamepad(t *testing.T, mv mover) *armRemoteControlGamepad {
	t.Helper()
	return &armRemoteControlGamepad{
		mover:          mv,
		logger:         newTestLogger(t),
		stepSize:       10.0,
		initialized:    true,
		connected:      true,
		analogTriggers: true,
		requireEnable:  true,
		cancelCtx:      context.Background(),
	}
}

func TestTickAppliesHatAndButtonDeltas(t *testing.T) {
	mv := &fakeMover{}
	arc := newTestGamepad(t, mv)

	arc.tick(context.Background(), map[input.Control]input.Event{
		input.ButtonLT:      {Event: input.ButtonPress},
		input.AbsoluteHat0X: {Event: input.PositionChangeAbs, Value: 1.0},
		input.AbsoluteRZ:    {Event: input.PositionChangeAbs, Value: 1.0},
	}, time.Now())

	steps := mv.steps()
	if len(steps) != 1 {
		t.Fatalf("expected 1 step call, got %d", len(steps))
	}
	if steps[0] != [3]float64{10, 0, 10} {
		t.Fatalf("expected delta (10,0,10), got %v", steps[0])
	}
}

func TestZComesFromAnalogTriggers(t *testing.T) {
	mv := &fakeMover{}
	arc := newTestGamepad(t, mv)

	arc.tick(context.Background(), map[input.Control]input.Event{
		input.ButtonLT:   {Event: input.ButtonPress},
		input.AbsoluteRZ: {Event: input.PositionChangeAbs, Value: 1.0},
	}, time.Now())

	steps := mv.steps()
	if len(steps) != 1 || steps[0][2] != 10.0 {
		t.Fatalf("expected dz=10 from a fully pressed right trigger, got %v", steps)
	}
}

func TestOpposedTriggersCancel(t *testing.T) {
	mv := &fakeMover{}
	arc := newTestGamepad(t, mv)

	arc.tick(context.Background(), map[input.Control]input.Event{
		input.ButtonLT:   {Event: input.ButtonPress},
		input.AbsoluteRZ: {Event: input.PositionChangeAbs, Value: 1.0},
		input.AbsoluteZ:  {Event: input.PositionChangeAbs, Value: 1.0},
	}, time.Now())

	if len(mv.steps()) != 0 {
		t.Fatalf("expected both triggers pressed to cancel to no motion, got %v", mv.steps())
	}
}

func TestTriggersAreProportional(t *testing.T) {
	mv := &fakeMover{}
	arc := newTestGamepad(t, mv)

	arc.tick(context.Background(), map[input.Control]input.Event{
		input.ButtonLT:   {Event: input.ButtonPress},
		input.AbsoluteRZ: {Event: input.PositionChangeAbs, Value: 0.5},
	}, time.Now())

	if steps := mv.steps(); len(steps) != 1 || steps[0][2] != 5.0 {
		t.Fatalf("expected dz=5 at half deflection, got %v", steps)
	}
}

// fakeController satisfies input.Controller. Only Controls is exercised --
// tick takes its events as an argument, so nothing else is needed.
type fakeController struct {
	input.Controller

	controls []input.Control
	err      error
}

func (f *fakeController) Controls(ctx context.Context, extra map[string]interface{}) ([]input.Control, error) {
	return f.controls, f.err
}

func TestHasAnalogTriggersDetection(t *testing.T) {
	logger := newTestLogger(t)
	ctx := context.Background()

	full := &fakeController{controls: []input.Control{input.AbsoluteX, input.AbsoluteZ, input.AbsoluteRZ}}
	if !hasAnalogTriggers(ctx, full, logger) {
		t.Fatalf("expected a pad reporting both trigger axes to use the analog path")
	}

	// The Nintendo/8BitDo "Pro Controller" S-input shape: triggers are
	// digital buttons, no trigger axes at all.
	digital := &fakeController{controls: []input.Control{input.AbsoluteX, input.ButtonLT2, input.ButtonRT2}}
	if hasAnalogTriggers(ctx, digital, logger) {
		t.Fatalf("expected a pad without trigger axes to fall back to the digital triggers")
	}

	partial := &fakeController{controls: []input.Control{input.AbsoluteZ}}
	if hasAnalogTriggers(ctx, partial, logger) {
		t.Fatalf("expected a pad reporting only one trigger axis to fall back")
	}

	// A transient RPC failure is not evidence of an odd pad; assume analog.
	broken := &fakeController{err: errors.New("boom")}
	if !hasAnalogTriggers(ctx, broken, logger) {
		t.Fatalf("expected a Controls error to assume analog triggers")
	}
}

func TestZFallsBackToDigitalTriggers(t *testing.T) {
	mv := &fakeMover{}
	arc := newTestGamepad(t, mv)
	arc.analogTriggers = false // pad reports no AbsoluteZ/AbsoluteRZ

	arc.tick(context.Background(), map[input.Control]input.Event{
		input.ButtonLT:  {Event: input.ButtonPress},
		input.ButtonRT2: {Event: input.ButtonPress},
	}, time.Now())

	if steps := mv.steps(); len(steps) != 1 || steps[0][2] != 10.0 {
		t.Fatalf("expected dz=10 from the digital right trigger, got %v", steps)
	}
}

func TestDeadmanBlocksMotionWhenNotHeld(t *testing.T) {
	mv := &fakeMover{}
	arc := newTestGamepad(t, mv)

	arc.tick(context.Background(), map[input.Control]input.Event{
		input.AbsoluteHat0X: {Event: input.PositionChangeAbs, Value: 1.0},
	}, time.Now())

	if len(mv.steps()) != 0 {
		t.Fatalf("expected no motion without the deadman held, got %v", mv.steps())
	}
	// The production cold-start case: service up, operator hasn't touched
	// the pad yet. That must not call mover.stop either -- nothing was ever
	// moving, so there is nothing to stop.
	if mv.stops() != 0 {
		t.Fatalf("expected no stop call when motion was never in flight, got %d", mv.stops())
	}
}

func TestDeadmanAllowsMotionWhenHeld(t *testing.T) {
	mv := &fakeMover{}
	arc := newTestGamepad(t, mv)

	arc.tick(context.Background(), map[input.Control]input.Event{
		input.ButtonLT:      {Event: input.ButtonPress},
		input.AbsoluteHat0X: {Event: input.PositionChangeAbs, Value: 1.0},
	}, time.Now())

	if len(mv.steps()) != 1 {
		t.Fatalf("expected 1 step with the deadman held, got %v", mv.steps())
	}
}

func TestDeadmanReleaseStopsExactlyOnce(t *testing.T) {
	mv := &fakeMover{}
	arc := newTestGamepad(t, mv)

	held := map[input.Control]input.Event{
		input.ButtonLT:      {Event: input.ButtonPress},
		input.AbsoluteHat0X: {Event: input.PositionChangeAbs, Value: 1.0},
	}
	released := map[input.Control]input.Event{
		input.ButtonLT:      {Event: input.ButtonRelease},
		input.AbsoluteHat0X: {Event: input.PositionChangeAbs, Value: 1.0},
	}

	arc.tick(context.Background(), held, time.Now())
	arc.tick(context.Background(), released, time.Now())
	arc.tick(context.Background(), released, time.Now())
	arc.tick(context.Background(), released, time.Now())

	if mv.stops() != 1 {
		t.Fatalf("expected exactly 1 stop across three released ticks, got %d", mv.stops())
	}

	// Re-arm: the gate must not latch off. A re-press after a release has to
	// resume motion, not stay stopped forever.
	arc.tick(context.Background(), held, time.Now())

	if len(mv.steps()) != 2 {
		t.Fatalf("expected motion to resume after re-pressing the deadman, got %d steps", len(mv.steps()))
	}
}

// TestDeadmanReleaseStopsAfterAnIdleTick pins the human-realistic release
// sequence: centre the stick first, then let go of the deadman. That is how
// an operator actually stops -- not the single combined transition
// TestDeadmanReleaseStopsExactlyOnce drives. The intervening idle tick (stick
// centred, deadman still held) must not defeat the eventual stop.
func TestDeadmanReleaseStopsAfterAnIdleTick(t *testing.T) {
	mv := &fakeMover{}
	arc := newTestGamepad(t, mv)

	moving := map[input.Control]input.Event{
		input.ButtonLT:      {Event: input.ButtonPress},
		input.AbsoluteHat0X: {Event: input.PositionChangeAbs, Value: 1.0},
	}
	idle := map[input.Control]input.Event{
		input.ButtonLT: {Event: input.ButtonHold},
	}
	released := map[input.Control]input.Event{
		input.ButtonLT: {Event: input.ButtonRelease},
	}

	arc.tick(context.Background(), moving, time.Now())
	arc.tick(context.Background(), idle, time.Now())
	arc.tick(context.Background(), released, time.Now())

	if mv.stops() != 1 {
		t.Fatalf("expected the deadman release to stop the arm even after an idle tick cleared motionSince, got %d stops", mv.stops())
	}
}

func TestDeadmanCanBeDisabled(t *testing.T) {
	mv := &fakeMover{}
	arc := newTestGamepad(t, mv)
	arc.requireEnable = false

	arc.tick(context.Background(), map[input.Control]input.Event{
		input.AbsoluteHat0X: {Event: input.PositionChangeAbs, Value: 1.0},
	}, time.Now())

	if len(mv.steps()) != 1 {
		t.Fatalf("expected motion without the deadman when require_enable is false, got %v", mv.steps())
	}
}

func TestTickIsANoOpWhenNothingIsHeld(t *testing.T) {
	mv := &fakeMover{}
	arc := newTestGamepad(t, mv)

	arc.tick(context.Background(), map[input.Control]input.Event{}, time.Now())

	if len(mv.steps()) != 0 {
		t.Fatalf("expected no step calls, got %d", len(mv.steps()))
	}
}

func TestTickStopsOnceOnDisconnect(t *testing.T) {
	mv := &fakeMover{}
	arc := newTestGamepad(t, mv)

	gone := map[input.Control]input.Event{
		input.ButtonSouth: {Event: input.Disconnect},
	}
	arc.tick(context.Background(), gone, time.Now())
	arc.tick(context.Background(), gone, time.Now())

	if mv.stops() != 1 {
		t.Fatalf("expected exactly 1 stop call across two disconnected ticks, got %d", mv.stops())
	}

	// A normal tick means the controller is back, which re-arms the latch:
	// the next disconnect must stop the mover again, not be swallowed by the
	// earlier one.
	arc.tick(context.Background(), map[input.Control]input.Event{}, time.Now())
	arc.tick(context.Background(), gone, time.Now())

	if mv.stops() != 2 {
		t.Fatalf("expected the latch to re-arm after reconnecting, got %d stop calls", mv.stops())
	}
}

func TestOmittedSafetyFieldsDefaultSafe(t *testing.T) {
	conf := &Config{ArmName: "arm-1", InputControllerName: "gamepad-1"}

	if got := resolveRequireEnable(conf); !got {
		t.Fatalf("expected an omitted require_enable to default to true (deadman enabled), got false")
	}
	if got := resolveMaxContinuousMotion(conf); got != defaultMaxContinuousMotion {
		t.Fatalf("expected an omitted max_continuous_motion to default to %v, got %v", defaultMaxContinuousMotion, got)
	}
}

func TestExplicitSafetyFieldsAreHonoured(t *testing.T) {
	no := false
	zero := 0
	conf := &Config{RequireEnable: &no, MaxContinuousMotion: &zero}

	if resolveRequireEnable(conf) {
		t.Fatalf("expected an explicit require_enable:false to disable the deadman")
	}
	if got := resolveMaxContinuousMotion(conf); got != 0 {
		t.Fatalf("expected an explicit max_continuous_motion:0 to disable the timer, got %v", got)
	}
}

func TestValidateAddsGripperDependencyWhenSet(t *testing.T) {
	deps, _, err := (&Config{
		ArmName:             "arm-1",
		InputControllerName: "gamepad-1",
		Gripper:             "gripper-1",
	}).Validate("components.0")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	var found bool
	for _, d := range deps {
		if d == "gripper-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected gripper-1 in deps, got %v", deps)
	}
}

func TestValidateOmitsGripperDependencyWhenUnset(t *testing.T) {
	deps, _, err := (&Config{ArmName: "arm-1", InputControllerName: "gamepad-1"}).Validate("components.0")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("expected exactly the arm and controller deps, got %v", deps)
	}
}

func TestValidateRejectsNegativeMaxContinuousMotion(t *testing.T) {
	negative := -5
	_, _, err := (&Config{
		ArmName:             "arm-1",
		InputControllerName: "gamepad-1",
		MaxContinuousMotion: &negative,
	}).Validate("components.0")
	if err == nil {
		t.Fatalf("expected a negative max_continuous_motion to fail validation")
	}
}

func TestValidateAcceptsZeroMaxContinuousMotion(t *testing.T) {
	zero := 0
	_, _, err := (&Config{
		ArmName:             "arm-1",
		InputControllerName: "gamepad-1",
		MaxContinuousMotion: &zero,
	}).Validate("components.0")
	if err != nil {
		t.Fatalf("expected max_continuous_motion:0 (timer disabled) to be valid, got: %v", err)
	}
}

func TestEStopLatchesAndBlocksMotion(t *testing.T) {
	mv := &fakeMover{}
	arc := newTestGamepad(t, mv)

	moving := map[input.Control]input.Event{
		input.ButtonLT:      {Event: input.ButtonPress},
		input.AbsoluteHat0X: {Event: input.PositionChangeAbs, Value: 1.0},
	}

	arc.tick(context.Background(), moving, time.Now())
	if len(mv.steps()) != 1 {
		t.Fatalf("expected motion before the E-stop, got %v", mv.steps())
	}

	estop := map[input.Control]input.Event{
		input.ButtonMenu: {Event: input.ButtonPress},
	}
	// Hold the E-stop down for several ticks, as an operator's thumb would.
	// It must stop the mover on the latching transition only, not on every
	// tick the button stays held.
	for i := 0; i < 5; i++ {
		arc.tick(context.Background(), estop, time.Now())
	}
	if mv.stops() != 1 {
		t.Fatalf("expected the E-stop to stop the mover exactly once across 5 held ticks, got %d stops", mv.stops())
	}

	// Button released, but the latch holds.
	arc.tick(context.Background(), moving, time.Now())
	arc.tick(context.Background(), moving, time.Now())
	if len(mv.steps()) != 1 {
		t.Fatalf("expected no further motion while latched, got %v", mv.steps())
	}
}

func TestEStopClearsOnStart(t *testing.T) {
	mv := &fakeMover{}
	arc := newTestGamepad(t, mv)
	arc.estopped = true

	arc.tick(context.Background(), map[input.Control]input.Event{
		input.ButtonStart: {Event: input.ButtonPress},
	}, time.Now())

	moving := map[input.Control]input.Event{
		input.ButtonLT:      {Event: input.ButtonPress},
		input.AbsoluteHat0X: {Event: input.PositionChangeAbs, Value: 1.0},
	}
	arc.tick(context.Background(), moving, time.Now())

	if len(mv.steps()) != 1 {
		t.Fatalf("expected motion to resume after ButtonStart cleared the latch, got %v", mv.steps())
	}
}

func TestEStopButtonAlsoLatches(t *testing.T) {
	mv := &fakeMover{}
	arc := newTestGamepad(t, mv)

	arc.tick(context.Background(), map[input.Control]input.Event{
		input.ButtonEStop: {Event: input.ButtonPress},
	}, time.Now())

	if !arc.estopped {
		t.Fatalf("expected input.ButtonEStop to latch the E-stop")
	}
}

func TestDeadOperatorTimerStopsFrozenController(t *testing.T) {
	mv := &fakeMover{}
	arc := newTestGamepad(t, mv)
	arc.maxContinuousMotion = time.Second

	start := time.Now()
	frozen := map[input.Control]input.Event{
		input.ButtonLT:      {Event: input.ButtonPress, Time: start},
		input.AbsoluteHat0X: {Event: input.PositionChangeAbs, Value: 1.0, Time: start},
	}

	arc.tick(context.Background(), frozen, start)
	if len(mv.steps()) != 1 {
		t.Fatalf("expected motion at the start, got %v", mv.steps())
	}

	// The controller then goes silent for good: four more ticks pass with
	// the same frozen events (five in total). A regression that clears
	// motionSince inside the timer gate re-arms the delta path on every tick
	// after the gate first fires -- each stop immediately followed by
	// another step, a sawtooth -- which a run of only two ticks (as this
	// test used to be) cannot catch: the sawtooth's second step doesn't
	// land until the third tick, so steps==1/stops==1 holds either way at
	// two ticks.
	for i := 2; i <= 5; i++ {
		arc.tick(context.Background(), frozen, start.Add(time.Duration(i)*time.Second))
	}

	if len(mv.steps()) != 1 || mv.stops() != 1 {
		t.Fatalf("expected the timer to latch after firing once, not sawtooth: got %d steps, %d stops", len(mv.steps()), mv.stops())
	}

	// The spec says motion resumes only once lastEventAt advances. Nothing
	// above proves the gate ever un-latches -- it deliberately never clears
	// its own trip condition -- so pin that a tick with genuinely fresh
	// event timestamps lets motion through again.
	recovered := map[input.Control]input.Event{
		input.ButtonLT:      {Event: input.ButtonPress, Time: start.Add(10 * time.Second)},
		input.AbsoluteHat0X: {Event: input.PositionChangeAbs, Value: 1.0, Time: start.Add(10 * time.Second)},
	}
	arc.tick(context.Background(), recovered, start.Add(10*time.Second))

	if len(mv.steps()) != 2 {
		t.Fatalf("expected motion to resume once lastEventAt advances, got %d steps", len(mv.steps()))
	}
}

func TestDeadOperatorTimerResetsOnFreshEvents(t *testing.T) {
	mv := &fakeMover{}
	arc := newTestGamepad(t, mv)
	arc.maxContinuousMotion = time.Second

	start := time.Now()
	for i := 0; i < 5; i++ {
		at := start.Add(time.Duration(i) * 500 * time.Millisecond)
		arc.tick(context.Background(), map[input.Control]input.Event{
			input.ButtonLT:      {Event: input.ButtonPress, Time: at},
			input.AbsoluteHat0X: {Event: input.PositionChangeAbs, Value: 1.0, Time: at},
		}, at)
	}

	if len(mv.steps()) != 5 {
		t.Fatalf("expected all 5 ticks to move while events keep advancing, got %v", mv.steps())
	}
}

func TestIdleTicksClearTheRunTimer(t *testing.T) {
	mv := &fakeMover{}
	arc := newTestGamepad(t, mv)
	arc.maxContinuousMotion = time.Second

	start := time.Now()
	// Event timestamps advance with each tick. This matters: a real operator
	// centring a stick emits a fresh event, so lastEventAt advances and the
	// dead-operator timer never trips. A fixture that freezes Time at `start`
	// instead simulates a *dead controller*, which correctly DOES trip the
	// timer -- and would make this test fail for the right reason about the
	// wrong scenario. Do not "fix" such a failure by making haltMotion clear
	// motionSince; that re-conflates the run timer with the stop latch and
	// reintroduces the sawtooth documented in the spec.
	at := func(d time.Duration) time.Time { return start.Add(d) }
	moving := func(d time.Duration) map[input.Control]input.Event {
		return map[input.Control]input.Event{
			input.ButtonLT:      {Event: input.ButtonPress, Time: at(d)},
			input.AbsoluteHat0X: {Event: input.PositionChangeAbs, Value: 1.0, Time: at(d)},
		}
	}
	resting := func(d time.Duration) map[input.Control]input.Event {
		return map[input.Control]input.Event{
			input.ButtonLT: {Event: input.ButtonPress, Time: at(d)},
		}
	}

	arc.tick(context.Background(), moving(0), at(0))
	// Operator rests on the deadman for well over the timeout, but keeps
	// producing events -- the controller is alive, the hand is just still.
	arc.tick(context.Background(), resting(2*time.Second), at(2*time.Second))
	arc.tick(context.Background(), resting(4*time.Second), at(4*time.Second))
	// ...then moves again. Resting must not have tripped the timer.
	arc.tick(context.Background(), moving(5*time.Second), at(5*time.Second))

	if len(mv.steps()) != 2 {
		t.Fatalf("expected resting not to trip the timer; got %v", mv.steps())
	}
}

func TestDeadOperatorTimerCanBeDisabled(t *testing.T) {
	mv := &fakeMover{}
	arc := newTestGamepad(t, mv)
	arc.maxContinuousMotion = 0

	start := time.Now()
	frozen := map[input.Control]input.Event{
		input.ButtonLT:      {Event: input.ButtonPress, Time: start},
		input.AbsoluteHat0X: {Event: input.PositionChangeAbs, Value: 1.0, Time: start},
	}

	arc.tick(context.Background(), frozen, start)
	arc.tick(context.Background(), frozen, start.Add(time.Hour))

	if len(mv.steps()) != 2 {
		t.Fatalf("expected a disabled timer never to suppress motion, got %v", mv.steps())
	}
}
