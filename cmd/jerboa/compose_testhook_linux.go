//go:build linux

package main

import (
	"context"
	"fmt"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/compose"
	"github.com/AitorConS/jerboa/internal/volume"
)

// composeUpWithCtx mirrors the `compose up` RunE logic with an injectable
// context and client. It exists only for the in-process daemon tests, which
// run on Linux, so it lives behind a linux build constraint.
func composeUpWithCtx(ctx context.Context, client *api.Client, f compose.File, storePath string) (compose.State, error) {
	order, err := compose.TopologicalSort(f.Services)
	if err != nil {
		return compose.State{}, err
	}

	volPath := volumeStorePath(storePath)
	volStore, err := volume.NewStore(volPath)
	if err != nil {
		return compose.State{}, fmt.Errorf("open volume store: %w", err)
	}

	var createdVolumes []string
	for volName, volCfg := range f.Volumes {
		if _, getErr := volStore.Get(volName); getErr != nil {
			sizeBytes, parseErr := volume.ParseSize(volCfg.DefaultSize())
			if parseErr != nil {
				return compose.State{}, fmt.Errorf("volume %q: %w", volName, parseErr)
			}
			if _, createErr := volStore.Create(volName, sizeBytes); createErr != nil {
				return compose.State{}, fmt.Errorf("create volume %q: %w", volName, createErr)
			}
			createdVolumes = append(createdVolumes, volName)
		}
	}

	state := compose.State{
		Services:        make(map[string]string, len(f.Services)),
		ServiceNetworks: make(map[string]string, len(f.Services)),
		ServiceIPs:      make(map[string]string, len(f.Services)),
		CreatedVolumes:  createdVolumes,
	}
	for _, name := range order {
		svc := f.Services[name]
		mem := svc.Memory
		if mem == "" {
			mem = "256M"
		}
		params, err := buildServiceRunParams(svc, mem, storePath)
		if err != nil {
			return compose.State{}, fmt.Errorf("service %q: %w", name, err)
		}
		params.Name = name
		info, err := client.Run(ctx, params)
		if err != nil {
			return compose.State{}, fmt.Errorf("service %q: %w", name, err)
		}
		state.Services[name] = info.ID
	}
	return state, nil
}
