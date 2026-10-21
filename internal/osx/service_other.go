//go:build !windows

package osx

import "context"

type scm struct{}

func (scm) Query(string) (ServiceState, error)               { return ServiceState{}, ErrNoServiceManager }
func (scm) Create(ServiceConfig) error                       { return ErrNoServiceManager }
func (scm) Update(ServiceConfig) error                       { return ErrNoServiceManager }
func (scm) SetStart(string, ServiceStart, bool) error        { return ErrNoServiceManager }
func (scm) Delete(string) error                              { return ErrNoServiceManager }
func runService(string, func(ctx context.Context) int) error { return ErrNotService }
