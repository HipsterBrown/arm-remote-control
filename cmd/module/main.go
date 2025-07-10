package main

import (
	"armremotecontrol"
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/generic"
)

func main() {
	// ModularMain can take multiple APIModel arguments, if your module implements multiple models.
	module.ModularMain(resource.APIModel{API: generic.API, Model: armremotecontrol.Gamepad})
}
