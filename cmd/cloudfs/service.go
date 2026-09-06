package main

import (
	"errors"

	"cloudfs/internal/service"
)

// cmdService is the CLI face of internal/service. The logic lives there so the
// running daemon can manage its own service over the control plane.
func cmdService(args []string) error {
	f := parseFlags(args)
	action := f.arg(0)
	if (action != "install" && action != "uninstall" && action != "status") || f.arg(1) != "" {
		return errors.New("service: expected install, uninstall or status")
	}
	rt, err := service.Real()
	if err != nil {
		return err
	}
	if action == "status" {
		return rt.Status()
	}
	cfg, configPath, err := loadConfig(f)
	if err != nil {
		return err
	}
	if action == "install" {
		return rt.Install(cfg, configPath)
	}
	return rt.Uninstall(cfg, configPath)
}
