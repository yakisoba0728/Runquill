package main

import (
	"errors"
	"os"
)

type runnerConfiguration struct {
	runtime    runnerRuntime
	kind       string
	hostCount  int
	hostLimits map[string]int
	pools      workerPools
	images     *runnerImages
}

func configureRunnerRuntime(image string) (runnerConfiguration, error) {
	// Kubernetes leaves pulls to the kubelet, but an invalid policy still stops startup.
	if _, err := loadImagePull(os.Getenv); err != nil {
		return runnerConfiguration{}, err
	}
	images, err := loadRunnerImages(os.Getenv, image)
	if err != nil {
		return runnerConfiguration{}, err
	}
	switch env("RUNQUILL_BACKEND", "docker") {
	case "docker":
		fleet, err := newDockerFleet(image, env("RUNQUILL_DOCKER_HOSTS", "local"))
		if err != nil {
			return runnerConfiguration{}, err
		}
		limits, err := fleet.limits(os.Getenv("RUNQUILL_DOCKER_HOST_LIMITS"))
		if err != nil {
			return runnerConfiguration{}, err
		}
		pools, err := fleet.configurePools(os.Getenv("RUNQUILL_WORKER_POOLS"))
		if err != nil {
			return runnerConfiguration{}, err
		}
		fleet.recordPrivilegeRequests(pools)
		return runnerConfiguration{runtime: fleet, kind: "docker", hostCount: len(fleet.hosts), hostLimits: limits, pools: pools, images: images}, nil
	case "kubernetes":
		if env("RUNQUILL_DOCKER_HOSTS", "local") != "local" || os.Getenv("RUNQUILL_DOCKER_HOST_LIMITS") != "" || os.Getenv("RUNQUILL_WORKER_POOLS") != "" {
			return runnerConfiguration{}, errors.New("unset Docker host and worker pool settings for Kubernetes mode")
		}
		if env("RUNQUILL_CONTAINER_ENGINE", "docker") != "docker" {
			return runnerConfiguration{}, errors.New("unset RUNQUILL_CONTAINER_ENGINE for Kubernetes mode; it selects the CLI for Docker hosts only")
		}
		kube, err := newKubeRunners(image)
		if err != nil {
			return runnerConfiguration{}, err
		}
		return runnerConfiguration{runtime: kube, kind: "kubernetes", hostCount: 1, images: images}, nil
	default:
		return runnerConfiguration{}, errors.New("RUNQUILL_BACKEND must be docker or kubernetes")
	}
}
