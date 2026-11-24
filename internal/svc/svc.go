// Package svc runs a program as a native service: a Windows service, a
// systemd unit or a launchd daemon.
package svc

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/kardianos/service"
)

// Spec describes a service.
type Spec struct {
	Name        string
	DisplayName string
	Description string
	Args        []string // arguments the service manager starts the binary with
}

type program struct {
	run    func(context.Context) error
	cancel context.CancelFunc
	done   chan struct{}
}

func (p *program) Start(service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel, p.done = cancel, make(chan struct{})
	go func() {
		defer close(p.done)
		if err := p.run(ctx); err != nil {
			slog.Error("service stopped with an error", "err", err)
			os.Exit(1) // let the service manager restart it
		}
	}()
	return nil
}

func (p *program) Stop(service.Service) error {
	p.cancel()
	select {
	case <-p.done:
	case <-time.After(20 * time.Second):
	}
	return nil
}

func build(spec Spec, run func(context.Context) error) (service.Service, error) {
	return service.New(&program{run: run}, &service.Config{
		Name: spec.Name, DisplayName: spec.DisplayName, Description: spec.Description, Arguments: spec.Args,
		Option: service.KeyValue{"Restart": "always", "RunAtLoad": true, "KeepAlive": true, "OnFailure": "restart"},
	})
}

// Run runs the program: under the service manager when started by one,
// otherwise in the foreground until interrupted.
func Run(spec Spec, run func(context.Context) error) error {
	// Only Windows needs a service-control handshake. systemd and launchd
	// just start the process and signal it, as a terminal or Docker does.
	if runtime.GOOS != "windows" || service.Interactive() {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return run(ctx)
	}
	s, err := build(spec, run)
	if err != nil {
		return err
	}
	safeBoot(spec.Name, true) // for services installed by an older version
	return s.Run()
}

// Control installs, removes, starts, stops or queries the service.
func Control(spec Spec, action string) error {
	s, err := build(spec, func(context.Context) error { return nil })
	if err != nil {
		return err
	}
	if action == "status" {
		st, err := s.Status()
		if err != nil {
			return err
		}
		fmt.Println(map[service.Status]string{service.StatusRunning: "running", service.StatusStopped: "stopped"}[st])
		return nil
	}
	for _, a := range service.ControlAction {
		if a == action {
			if err := service.Control(s, action); err != nil {
				return err
			}
			switch action {
			case "install":
				if err := safeBoot(spec.Name, true); err != nil {
					fmt.Fprintln(os.Stderr, "warning: the service will not start in safe mode or Directory Services Restore Mode:", err)
				}
			case "uninstall":
				safeBoot(spec.Name, false)
			}
			return nil
		}
	}
	return fmt.Errorf("unknown service action %q (use install, uninstall, start, stop, restart or status)", action)
}
