//go:build !linux

package gatewayidentity

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"

	"github.com/srcfl/ftw/go/internal/nova"
)

func defaultBindingFileOps() bindingFileOps { return bindingFileOps{} }

func LoadOrCreateUnboundNovaIdentity(keyPath string) (*nova.Identity, error) {
	paths, err := PathsForKey(keyPath)
	if err != nil {
		return nil, err
	}
	identity, err := nova.LoadOrCreateIdentityGuarded(
		paths.Key,
		[]string{filepath.Base(paths.Marker), filepath.Base(paths.Sidecar)},
		func(*os.File) error { return nil },
	)
	if errors.Is(err, nova.ErrIdentityCreationBlocked) {
		return nil, ErrBindingIncomplete
	}
	return identity, err
}

func openBindingStorage(BindingPaths, bindingFileOps) (bindingStorage, error) {
	return nil, ErrUnsupportedBinding
}

type unsupportedRouteAuthority struct{}

func newRouteAuthority() routeAuthority { return unsupportedRouteAuthority{} }

func (unsupportedRouteAuthority) LookupIP(context.Context, string) ([]net.IP, error) {
	return nil, ErrUnsupportedBinding
}

func (unsupportedRouteAuthority) ResolvedRoute(context.Context, net.IP) (routeResult, error) {
	return routeResult{}, ErrUnsupportedBinding
}

func (unsupportedRouteAuthority) PhysicalInterface(int) (physicalInterface, error) {
	return physicalInterface{}, errors.New("physical interface lookup is unsupported")
}

func linuxResolvedRoute(context.Context, net.IP) (routeResult, error) {
	return routeResult{}, ErrUnsupportedBinding
}

func makeBindingFIFO(string, uint32) error { return ErrUnsupportedBinding }
