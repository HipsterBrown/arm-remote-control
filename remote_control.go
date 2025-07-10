package armremotecontrol

import (
	"context"
	"github.com/pkg/errors"
	"go.viam.com/rdk/components/arm"
	"go.viam.com/rdk/components/input"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/generic"
	"go.viam.com/utils/rpc"
)

var (
	Gamepad          = resource.NewModel("hipsterbrown", "arm-remote-control", "gamepad")
	errUnimplemented = errors.New("unimplemented")
)

func init() {
	resource.RegisterService(generic.API, Gamepad,
		resource.Registration[resource.Resource, *Config]{
			Constructor: newArmRemoteControlGamepad,
		},
	)
}

type Config struct {
	ArmName             string `json:"arm"`
	InputControllerName string `json:"input_controller"`
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

	arc := &armRemoteControlGamepad{
		name:            name,
		arm:             arm1,
		inputController: controller,
		logger:          logger,
		cfg:             conf,
		cancelCtx:       cancelCtx,
		cancelFunc:      cancelFunc,
	}

	if err := arc.registerCallbacks(ctx); err != nil {
		return nil, errors.Errorf("error with starting remote control service: %q", err)
	}
	return arc, nil
}

func (arc *armRemoteControlGamepad) Name() resource.Name {
	return arc.name
}

func (arc *armRemoteControlGamepad) registerCallbacks(ctx context.Context) error {
	remoteCtl := func(ctx context.Context, event input.Event) {
		if arc.cancelCtx.Err() != nil {
			return
		}
		switch event.Control {
		case input.AbsoluteHat0X:
			arc.logger.Infof("Got value to change Hat0X: %f", event.Value)
		case input.AbsoluteHat0Y:
			arc.logger.Infof("Got value to change Hat0Y: %f", event.Value)
		default:
		}
	}

	for _, control := range []input.Control{input.AbsoluteHat0X, input.AbsoluteHat0Y} {
		if err := arc.inputController.RegisterControlCallback(ctx, control, []input.EventType{input.PositionChangeAbs}, remoteCtl, map[string]interface{}{}); err != nil {
			return err
		}
	}
	return nil
}

func (arc *armRemoteControlGamepad) NewClientFromConn(ctx context.Context, conn rpc.ClientConn, remoteName string, name resource.Name, logger logging.Logger) (resource.Resource, error) {
	panic("not implemented")
}

func (arc *armRemoteControlGamepad) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	panic("not implemented")
}

func (arc *armRemoteControlGamepad) Close(context.Context) error {
	// Put close code here
	arc.cancelFunc()
	return nil
}
