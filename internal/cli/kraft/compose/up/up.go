// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2024, Unikraft GmbH and The KraftKit Authors.
// Licensed under the BSD-3-Clause License (the "License").
// You may not use this file except in compliance with the License.

package up

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/MakeNowJust/heredoc"
	"github.com/compose-spec/compose-go/types"
	"github.com/spf13/cobra"

	"kraftkit.sh/cmdfactory"
	"kraftkit.sh/compose"
	"kraftkit.sh/internal/cli/kraft/build"
	"kraftkit.sh/internal/cli/kraft/logs"
	"kraftkit.sh/internal/cli/kraft/net/create"
	"kraftkit.sh/internal/cli/kraft/pkg"
	"kraftkit.sh/internal/cli/kraft/pkg/pull"
	"kraftkit.sh/internal/cli/kraft/remove"
	"kraftkit.sh/internal/cli/kraft/run"
	"kraftkit.sh/log"
	"kraftkit.sh/machine/network"
	"kraftkit.sh/packmanager"
	"kraftkit.sh/unikraft"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	composeapi "kraftkit.sh/api/compose/v1"
	machineapi "kraftkit.sh/api/machine/v1alpha1"
	networkapi "kraftkit.sh/api/network/v1alpha1"
	mplatform "kraftkit.sh/machine/platform"
)

type UpOptions struct {
	composefile string
}

func NewCmd() *cobra.Command {
	cmd, err := cmdfactory.New(&UpOptions{}, cobra.Command{
		Short:   "Run a compose project",
		Use:     "up [FLAGS]",
		Args:    cobra.NoArgs,
		Aliases: []string{},
		Long:    "Run a compose project.",
		Example: heredoc.Doc(`
			# Run a compose project
			$ kraft compose up
		`),
		Annotations: map[string]string{
			cmdfactory.AnnotationHelpGroup: "compose",
		},
	})
	if err != nil {
		panic(err)
	}

	return cmd
}

func (opts *UpOptions) Pre(cmd *cobra.Command, _ []string) error {
	ctx, err := packmanager.WithDefaultUmbrellaManagerInContext(cmd.Context())
	if err != nil {
		return err
	}

	cmd.SetContext(ctx)

	if cmd.Flag("file").Changed {
		opts.composefile = cmd.Flag("file").Value.String()
	}

	log.G(cmd.Context()).WithField("composefile", opts.composefile).Debug("using")
	return nil
}

func (opts *UpOptions) Run(ctx context.Context, args []string) error {
	workdir, err := os.Getwd()
	if err != nil {
		return err
	}

	project, err := compose.NewProjectFromComposeFile(ctx, workdir, opts.composefile)
	if err != nil {
		return err
	}

	if err := project.Validate(ctx); err != nil {
		return err
	}

	if err := project.AssignIPs(ctx); err != nil {
		return err
	}

	composeController, err := compose.NewComposeProjectV1(ctx)
	if err != nil {
		return err
	}

	embeddedProject, err := composeController.Get(ctx, &composeapi.Compose{
		ObjectMeta: metav1.ObjectMeta{
			Name: project.Name,
		},
	})
	if err != nil {
		return err
	}

	projectNetworks := []metav1.ObjectMeta{}
	if embeddedProject != nil {
		projectNetworks = embeddedProject.Status.Networks
	}

	networkController, err := network.NewNetworkV1alpha1ServiceIterator(ctx)
	if err != nil {
		return err
	}

	networks, err := networkController.List(ctx, &networkapi.NetworkList{})
	if err != nil {
		return err
	}

	// We need to first create the networks with a provided subnet
	// and then the ones which we will assign IPs to
	subnetNetworks := []string{}
	emptyNetworks := []string{}
	for name, network := range project.Networks {
		if network.Ipam.Config == nil || len(network.Ipam.Config) == 0 {
			emptyNetworks = append(emptyNetworks, name)
		} else {
			subnetNetworks = append(subnetNetworks, name)
		}
	}

	orderedNetworks := append(subnetNetworks, emptyNetworks...)

	for _, networkName := range orderedNetworks {
		network := project.Networks[networkName]
		alreadyRunning := false
		for _, n := range networks.Items {
			if n.Name == network.Name {
				alreadyRunning = true
				break
			}
		}
		if alreadyRunning {
			continue
		}

		driver := "bridge"
		if network.Driver != "" {
			driver = network.Driver
		}

		subnet := ""
		if len(network.Ipam.Config) > 0 {
			subnet = network.Ipam.Config[0].Subnet
		}
		createOptions := create.CreateOptions{
			Driver:  driver,
			Network: subnet,
		}

		log.G(ctx).Infof("creating network %s...", network.Name)
		if err := createOptions.Run(ctx, []string{network.Name}); err != nil {
			return err
		}

		if network, err := networkController.Get(ctx, &networkapi.Network{
			ObjectMeta: metav1.ObjectMeta{
				Name: network.Name,
			},
		}); err == nil && network.Status.State == networkapi.NetworkStateUp {
			projectNetworks = append(projectNetworks, network.ObjectMeta)
		}

	}

	projectMachines := []metav1.ObjectMeta{}
	if embeddedProject != nil {
		projectMachines = embeddedProject.Status.Machines
	}

	// Check that none of the services are already running
	machineController, err := mplatform.NewMachineV1alpha1ServiceIterator(ctx)
	if err != nil {
		return err
	}

	machines, err := machineController.List(ctx, &machineapi.MachineList{})
	if err != nil {
		return err
	}

	for _, service := range project.Services {
		alreadyRunning := false
		for _, machine := range machines.Items {
			if service.Name == machine.Name {
				if machine.Status.State == machineapi.MachineStateRunning {
					alreadyRunning = true
				} else {
					rmOpts := remove.RemoveOptions{
						Platform: machine.Spec.Platform,
					}

					if err := rmOpts.Run(ctx, []string{service.Name}); err != nil {
						return err
					}
				}
				break
			}
		}
		if alreadyRunning {
			continue
		}
		if service.Image == "" {
			if err := buildService(ctx, service); err != nil {
				return err
			}
		} else {
			if err := ensureServiceIsPackaged(ctx, service); err != nil {
				return err
			}
		}

		if err := runService(ctx, project, service); err != nil {
			log.G(ctx).WithError(err).Errorf("failed to run service %s", service.Name)
		}

		if machine, err := machineController.Get(ctx, &machineapi.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: service.Name,
			},
		}); err == nil && machine.Status.State == machineapi.MachineStateRunning {
			projectMachines = append(projectMachines, machine.ObjectMeta)
		}
	}

	if _, err := composeController.Update(ctx, &composeapi.Compose{
		ObjectMeta: metav1.ObjectMeta{
			Name: project.Name,
		},
		Spec: composeapi.ComposeSpec{
			Composefile: project.ComposeFiles[0],
			Workdir:     project.WorkingDir,
		},
		Status: composeapi.ComposeStatus{
			Machines: projectMachines,
			Networks: projectNetworks,
		},
	}); err != nil {
		return err
	}

	var wg sync.WaitGroup

	longestName := 0
	for _, service := range project.Services {
		if len(service.Name) > longestName {
			longestName = len(service.Name)
		}
	}
	for i := range project.Services {
		wg.Add(1)
		go func(service types.ServiceConfig) {
			defer wg.Done()

			if err := logService(ctx, service, longestName); err != nil {
				log.G(ctx).WithError(err).Errorf("failed to log service %s", service.Name)
			}
		}(project.Services[i])
	}

	wg.Wait()

	return nil
}

func platArchFromService(service types.ServiceConfig) (string, string, error) {
	// The service platform should be in the form <platform>/<arch>

	parts := strings.SplitN(service.Platform, "/", 2)

	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid platform: %s for service %s", service.Platform, service.Name)
	}

	return parts[0], parts[1], nil
}

func ensureServiceIsPackaged(ctx context.Context, service types.ServiceConfig) error {
	plat, arch, err := platArchFromService(service)
	if err != nil {
		return err
	}

	parts := strings.SplitN(service.Image, ":", 2)
	imageName := parts[0]
	imageVersion := "latest"
	if len(parts) == 2 {
		imageVersion = parts[1]
	}

	service.Image = imageName + ":" + imageVersion

	log.G(ctx).Debugf("searching for service %s locally...", service.Name)
	// Check whether the image is already in the local catalog
	packages, err := packmanager.G(ctx).Catalog(ctx,
		packmanager.WithArchitecture(arch),
		packmanager.WithName(imageName),
		packmanager.WithPlatform(plat),
		packmanager.WithTypes(unikraft.ComponentTypeApp),
		packmanager.WithVersion(imageVersion))
	if err != nil {
		return err
	}

	// If we have it locally, we are done
	if len(packages) != 0 {
		log.G(ctx).Debugf("found service %s locally", service.Name)
		return nil
	}

	log.G(ctx).Debugf("searching for service %s remotely...", service.Name)
	// Check whether the image is in the remote catalog
	packages, err = packmanager.G(ctx).Catalog(ctx,
		packmanager.WithArchitecture(arch),
		packmanager.WithName(imageName),
		packmanager.WithPlatform(plat),
		packmanager.WithTypes(unikraft.ComponentTypeApp),
		packmanager.WithRemote(true),
		packmanager.WithVersion(imageVersion))
	if err != nil {
		return err
	}

	// If we have it remotely, we are done
	if len(packages) != 0 {
		log.G(ctx).Infof("found service %s remotely, pulling...", service.Name)
		// We need to pull it locally
		pullOptions := pull.PullOptions{Platform: plat, Architecture: arch}
		return pullOptions.Run(ctx, []string{service.Image})
	}

	// Otherwise, we need to build and package it
	if err := buildService(ctx, service); err != nil {
		return err
	}

	return pkgService(ctx, service)
}

func buildService(ctx context.Context, service types.ServiceConfig) error {
	if service.Build == nil {
		return fmt.Errorf("service %s has no build context", service.Name)
	}

	plat, arch, err := platArchFromService(service)
	if err != nil {
		return err
	}

	log.G(ctx).Infof("building service %s...", service.Name)

	buildOptions := build.BuildOptions{Platform: plat, Architecture: arch}

	return buildOptions.Run(ctx, []string{service.Build.Context})
}

func pkgService(ctx context.Context, service types.ServiceConfig) error {
	plat, arch, err := platArchFromService(service)
	if err != nil {
		return err
	}

	log.G(ctx).Infof("packaging service %s...", service.Name)

	pkgOptions := pkg.PkgOptions{
		Architecture: arch,
		Name:         service.Image,
		Format:       "oci",
		Platform:     plat,
		Strategy:     packmanager.StrategyOverwrite,
	}

	return pkgOptions.Run(ctx, []string{service.Build.Context})
}

func runService(ctx context.Context, project *compose.Project, service types.ServiceConfig) error {
	// The service should be packaged at this point
	plat, arch, err := platArchFromService(service)
	if err != nil {
		return err
	}

	log.G(ctx).Infof("running service %s...", service.Name)

	networks := []string{}
	for name, network := range service.Networks {
		networkArg := fmt.Sprintf("%s:%s", project.Networks[name].Name, network.Ipv4Address)
		networks = append(networks, networkArg)
	}

	runOptions := run.RunOptions{
		Architecture: arch,
		Detach:       true,
		Name:         service.Name,
		Networks:     networks,
		Platform:     plat,
	}

	if service.Image != "" {
		return runOptions.Run(ctx, []string{service.Image})
	}

	return runOptions.Run(ctx, []string{service.Build.Context})
}

func logService(ctx context.Context, service types.ServiceConfig, prefixLength int) error {
	prefix := service.Name + strings.Repeat(" ", prefixLength-len(service.Name))

	plat, _, err := platArchFromService(service)
	if err != nil {
		return err
	}

	logOptions := logs.LogOptions{
		Follow:   true,
		Platform: plat,
		Prefix:   prefix,
	}

	return logOptions.Run(ctx, []string{service.Name})
}