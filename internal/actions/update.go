package actions

import (
	"errors"
	"github.com/containrrr/watchtower/internal/util"
	"github.com/containrrr/watchtower/pkg/container"
	"github.com/containrrr/watchtower/pkg/lifecycle"
	"github.com/containrrr/watchtower/pkg/session"
	"github.com/containrrr/watchtower/pkg/sorter"
	"github.com/containrrr/watchtower/pkg/types"
	log "github.com/sirupsen/logrus"
)

// Update looks at the running Docker containers to see if any of the images
// used to start those containers have been updated. If a change is detected in
// any of the images, the associated containers are stopped and restarted with
// the new image.
func Update(client container.Client, params types.UpdateParams) (*session.Report, error) {
	log.Debug("Checking containers for updated images")
	progress := &session.Progress{}
	staleCount := 0

	if params.LifecycleHooks {
		lifecycle.ExecutePreChecks(client, params)
	}

	containers, err := client.ListContainers(params.Filter)
	if err != nil {
		return nil, err
	}

	staleCheckFailed := 0

	for i, targetContainer := range containers {
		stale, newestImage, err := client.IsContainerStale(targetContainer)
		if stale && !params.NoRestart && !params.MonitorOnly && !targetContainer.IsMonitorOnly() && !targetContainer.HasImageInfo() {
			err = errors.New("no available image info")
		}
		if err != nil {
			log.Infof("Unable to update container %q: %v. Proceeding to next.", containers[i].Name(), err)
			stale = false
			staleCheckFailed++
			progress.AddSkipped(targetContainer, err)
		} else {
			progress.AddScanned(targetContainer, newestImage)
		}
		containers[i].Stale = stale

		if stale {
			staleCount++
		}
	}

	containers, err = sorter.SortByDependencies(containers)
	if err != nil {
		return nil, err
	}

	checkDependencies(containers)

	containersToUpdate := []container.Container{}
	if !params.MonitorOnly {
		for i := len(containers) - 1; i >= 0; i-- {
			if !containers[i].IsMonitorOnly() {
				containersToUpdate = append(containersToUpdate, containers[i])
				progress.MarkForUpdate(containers[i].ID())
			}
		}
	}

	if params.RollingRestart {
		progress.UpdateFailed(performRollingRestart(containersToUpdate, client, params))
	} else {
		progress.UpdateFailed(stopContainersInReversedOrder(containersToUpdate, client, params))
		progress.UpdateFailed(restartContainersInSortedOrder(containersToUpdate, client, params))
	}

	if params.LifecycleHooks {
		lifecycle.ExecutePostChecks(client, params)
	}
	return progress.Report(), nil
}

func performRollingRestart(containers []container.Container, client container.Client, params types.UpdateParams) map[string]error {
	cleanupImageIDs := make(map[string]bool, len(containers))
	failed := make(map[string]error, len(containers))

	for i := len(containers) - 1; i >= 0; i-- {
		if containers[i].Stale {
			if err := stopStaleContainer(containers[i], client, params); err != nil {
				failed[containers[i].ID()] = err
			}
			if err := restartStaleContainer(containers[i], client, params); err != nil {
				failed[containers[i].ID()] = err
			}
			cleanupImageIDs[containers[i].ImageID()] = true
		}
	}

	if params.Cleanup {
		cleanupImages(client, cleanupImageIDs)
	}
	return failed
}

func stopContainersInReversedOrder(containers []container.Container, client container.Client, params types.UpdateParams) map[string]error {
	failed := make(map[string]error, len(containers))
	for i := len(containers) - 1; i >= 0; i-- {
		if err := stopStaleContainer(containers[i], client, params); err != nil {
			failed[containers[i].ID()] = err
		}
	}
	return failed
}

func stopStaleContainer(container container.Container, client container.Client, params types.UpdateParams) error {
	if container.IsWatchtower() {
		log.Debugf("This is the watchtower container %s", container.Name())
		return nil
	}

	if !container.Stale {
		return nil
	}
	if params.LifecycleHooks {
		if err := lifecycle.ExecutePreUpdateCommand(client, container); err != nil {
			log.Error(err)
			log.Info("Skipping container as the pre-update command failed")
			return err
		}
	}

	if err := client.StopContainer(container, params.Timeout); err != nil {
		log.Error(err)
		return err
	}
	return nil
}

func restartContainersInSortedOrder(containers []container.Container, client container.Client, params types.UpdateParams) map[string]error {
	cleanupImageIDs := make(map[string]bool, len(containers))
	failed := make(map[string]error, len(containers))

	for _, c := range containers {
		if !c.Stale {
			continue
		}
		if err := restartStaleContainer(c, client, params); err != nil {
			failed[c.ID()] = err
		}
		cleanupImageIDs[c.ImageID()] = true
	}

	if params.Cleanup {
		cleanupImages(client, cleanupImageIDs)
	}

	return failed
}

func cleanupImages(client container.Client, imageIDs map[string]bool) {
	for imageID := range imageIDs {
		if err := client.RemoveImageByID(imageID); err != nil {
			log.Error(err)
		}
	}
}

func restartStaleContainer(container container.Container, client container.Client, params types.UpdateParams) error {
	// Since we can't shutdown a watchtower container immediately, we need to
	// start the new one while the old one is still running. This prevents us
	// from re-using the same container name so we first rename the current
	// instance so that the new one can adopt the old name.
	if container.IsWatchtower() {
		if err := client.RenameContainer(container, util.RandName()); err != nil {
			log.Error(err)
			return nil
		}
	}

	if !params.NoRestart {
		if newContainerID, err := client.StartContainer(container); err != nil {
			log.Error(err)
			return err
		} else if container.Stale && params.LifecycleHooks {
			lifecycle.ExecutePostUpdateCommand(client, newContainerID)
		}
	}
	return nil
}

func checkDependencies(containers []container.Container) {

	for i, parent := range containers {
		if parent.ToRestart() {
			continue
		}

	LinkLoop:
		for _, linkName := range parent.Links() {
			for _, child := range containers {
				if child.Name() == linkName && child.ToRestart() {
					containers[i].Linked = true
					break LinkLoop
				}
			}
		}
	}
}
