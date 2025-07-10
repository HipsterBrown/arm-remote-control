package main

import (
	"context"
	"armremotecontrol"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/generic"
)

func main() {
	err := realMain()
	if err != nil {
		panic(err)
	}
}

func realMain() error {
	ctx := context.Background()
	logger := logging.NewLogger("cli")

	deps := resource.Dependencies{}
	// can load these from a remote machine if you need

	cfg := armremotecontrol.Config{}

	thing, err := armremotecontrol.NewGamepad(ctx, deps, generic.Named("foo"), &cfg, logger)
	if err != nil {
		return err
	}
	defer thing.Close(ctx)

	return nil
}
