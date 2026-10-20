package ctlapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"holocron/internal/cli"
	"holocron/internal/logx"
	"holocron/internal/status"
)

// Action names of the routes (component ctlapi).
const (
	ActState    = "ctlapi.state"
	ActStop     = "ctlapi.stop"
	ActReload   = "ctlapi.reload"
	ActRecord   = "ctlapi.camera.record"
	ActLive     = "ctlapi.live"
	ActLevelGet = "ctlapi.log.level.get"
	ActLevelSet = "ctlapi.log.level.set"
)

// errBadRequest is a refusal (logged WARN) answered 400.
var errBadRequest = fmt.Errorf("%w (bad request)", cli.ErrRefused)

type serverKey struct{}

func serverFrom(ctx context.Context) (*Server, error) {
	s, _ := ctx.Value(serverKey{}).(*Server)
	if s == nil {
		return nil, errors.New("ctlapi: action run outside a request")
	}
	return s, nil
}

// decode reads the JSON body strictly (unknown fields refused); an empty body is an empty object.
func decode(in cli.Input, into any) error {
	body, _ := in.Args["body"].([]byte)
	if len(strings.TrimSpace(string(body))) == 0 {
		body = []byte("{}")
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("%w: the body is not the expected JSON object: %v", errBadRequest, err)
	}
	if dec.More() {
		return fmt.Errorf("%w: one JSON object expected", errBadRequest)
	}
	return nil
}

func init() {
	reg := func(name string, mutating bool, run func(ctx context.Context, s *Server, in cli.Input) (cli.Output, error)) {
		cli.Register(cli.Action{Name: name, Component: "ctlapi", Mutating: mutating, Run: func(ctx context.Context, in cli.Input) (cli.Output, error) {
			s, err := serverFrom(ctx)
			if err != nil {
				return cli.Output{}, err
			}
			return run(ctx, s, in)
		}})
	}
	reg(ActState, false, func(ctx context.Context, s *Server, in cli.Input) (cli.Output, error) {
		doc := s.o.Backend.Snapshot()
		if doc == nil {
			return cli.Output{}, ErrNotRunning
		}
		return cli.Output{Data: StateFrom(doc)}, nil
	})
	reg(ActStop, true, func(ctx context.Context, s *Server, in cli.Input) (cli.Output, error) {
		var req stopRequest
		if err := decode(in, &req); err != nil {
			return cli.Output{}, err
		}
		switch req.Kind {
		case "":
			req.Kind = status.StopKindRequested
		case status.StopKindRequested, status.StopKindInterrupted:
		default:
			return cli.Output{}, fmt.Errorf("%w: kind must be requested or interrupted, got %q", errBadRequest, req.Kind)
		}
		rep := s.o.Backend.Stop(req.Kind)
		if rep.Survivors == nil {
			rep.Survivors = []int{}
		}
		return cli.Output{Target: req.Kind, Data: rep}, nil
	})
	reg(ActReload, true, func(ctx context.Context, s *Server, in cli.Input) (cli.Output, error) {
		if err := decode(in, &struct{}{}); err != nil {
			return cli.Output{}, err
		}
		return cli.Output{}, s.o.Backend.Reload(ctx)
	})
	reg(ActRecord, true, func(ctx context.Context, s *Server, in cli.Input) (cli.Output, error) {
		var req enabledRequest
		if err := decode(in, &req); err != nil {
			return cli.Output{}, err
		}
		if req.Enabled == nil {
			return cli.Output{}, fmt.Errorf("%w: enabled (true or false) is required", errBadRequest)
		}
		ch, err := s.o.Backend.SetRecord(ctx, in.Target, *req.Enabled)
		return cli.Output{Data: ch}, err
	})
	reg(ActLive, true, func(ctx context.Context, s *Server, in cli.Input) (cli.Output, error) {
		var req enabledRequest
		if err := decode(in, &req); err != nil {
			return cli.Output{}, err
		}
		if req.Enabled == nil {
			return cli.Output{}, fmt.Errorf("%w: enabled (true or false) is required", errBadRequest)
		}
		ch, err := s.o.Backend.SetLive(ctx, *req.Enabled)
		return cli.Output{Target: "live", Data: ch}, err
	})
	reg(ActLevelGet, false, func(ctx context.Context, s *Server, in cli.Input) (cli.Output, error) {
		if s.o.Levels == nil {
			return cli.Output{}, ErrNotRunning
		}
		return cli.Output{Data: LevelsFrom(s.o.Levels.Snapshot())}, nil
	})
	reg(ActLevelSet, true, func(ctx context.Context, s *Server, in cli.Input) (cli.Output, error) {
		var req levelRequest
		if err := decode(in, &req); err != nil {
			return cli.Output{}, err
		}
		if s.o.Levels == nil {
			return cli.Output{}, ErrNotRunning
		}
		target, err := ApplyLevel(s.o.Levels, req.Component, req.Level)
		if err != nil {
			return cli.Output{}, fmt.Errorf("%w: %v", errBadRequest, err)
		}
		return cli.Output{Target: target, Data: LevelsFrom(s.o.Levels.Snapshot())}, nil
	})
}

// ApplyLevel sets one component's level ("all" or "" = the default) on a LevelController.
func ApplyLevel(lc *logx.LevelController, component, level string) (target string, err error) {
	l, err := logx.ParseLevel(level)
	if err != nil {
		return "", err
	}
	component = strings.ToLower(strings.TrimSpace(component))
	if component == "" || component == "all" {
		lc.SetDefault(l)
		for c := range lc.Snapshot().Components {
			lc.Clear(c)
		}
		return "all=" + logx.LevelName(l), nil
	}
	if strings.ContainsAny(component, " =,") {
		return "", fmt.Errorf("component %q: no spaces, '=' or ','", component)
	}
	lc.SetComponent(component, l)
	return component + "=" + logx.LevelName(l), nil
}

// LevelsFrom renders a level snapshot.
func LevelsFrom(s logx.LevelSnapshot) Levels {
	out := Levels{Default: logx.LevelName(s.Default), Components: map[string]string{}}
	for c, l := range s.Components {
		out.Components[c] = logx.LevelName(l)
	}
	return out
}
