// Package docker integrates with the Docker Remote API https://docs.docker.com/reference/api/docker_remote_api_v1.20/
package docker

import (
	"fmt"
	"strings"
	"sync"

	"github.com/fsouza/go-dockerclient"
	"github.com/op/go-logging"

	"github.com/microscaling/microscaling/demand"
	"github.com/microscaling/microscaling/scheduler"
)

const labelMap string = "com.microscaling.microscaling-in-a-box"

var log = logging.MustGetLogger("mssscheduler")

type dockerContainer struct {
	state   string
	updated bool
}

// DockerScheduler stores information and state we need for communicating with Docker remote API
// We keep track of each container so that we have their identities to stop them when we need to
type DockerScheduler struct {
	client         *docker.Client
	pullImages     bool
	taskContainers map[string]map[string]*dockerContainer // tasks indexed by app name, containers indexed by ID
	sync.Mutex
}

// NewScheduler creates a new interface to the Docker remote API
func NewScheduler(pullImages bool, dockerHost string) *DockerScheduler {
	client, err := docker.NewClient(dockerHost)
	if err != nil {
		log.Errorf("Error starting Docker client: %v", err)
		return nil
	}

	return &DockerScheduler{
		client:         client,
		taskContainers: make(map[string]map[string]*dockerContainer),
		pullImages:     pullImages,
	}
}

// compile-time assert that we implement the right interface
var _ scheduler.Scheduler = (*DockerScheduler)(nil)

var scaling sync.WaitGroup

// InitScheduler gets the images for each task
func (c *DockerScheduler) InitScheduler(task *demand.Task) (err error) {
	log.Infof("Docker initializing task %s", task.Name)

	c.Lock()
	defer c.Unlock()

	c.taskContainers[task.Name] = make(map[string]*dockerContainer, 100)

	// We may need to pull the image for this container
	if c.pullImages {
		pullOpts := docker.PullImageOptions{
			Repository: task.Image,
		}

		authOpts := docker.AuthConfiguration{}

		log.Infof("Pulling image: %v", task.Image)
		err = c.client.PullImage(pullOpts, authOpts)
		if err != nil {
			log.Errorf("Failed to pull image %s: %v", task.Image, err)
		}
	}

	return err
}

// startTask creates the container and then starts it
func (c *DockerScheduler) startTask(task *demand.Task) {
	var labels = map[string]string{
		labelMap: task.Name,
	}

	var cmds = strings.Fields(task.Command)

	createOpts := docker.CreateContainerOptions{
		Config: &docker.Config{
			Image:        task.Image,
			Cmd:          cmds,
			AttachStdout: true,
			AttachStdin:  true,
			Labels:       labels,
			Env:          task.Env,
		},
		HostConfig: &docker.HostConfig{
			PublishAllPorts: task.PublishAllPorts,
			NetworkMode:     task.NetworkMode,
		},
	}

	go func() {
		scaling.Add(1)
		defer scaling.Done()

		log.Debugf("[start] task %s", task.Name)
		container, err := c.client.CreateContainer(createOpts)
		if err != nil {
			log.Errorf("Couldn't create container for task %s: %v", task.Name, err)
			return
		}

		var containerID = container.ID[:12]

		c.Lock()
		c.taskContainers[task.Name][containerID] = &dockerContainer{
			state: "created",
		}
		c.Unlock()
		log.Debugf("[created] task %s ID %s", task.Name, containerID)

		// Start it but passing nil for the HostConfig as this option was removed in Docker 1.12.
		err = c.client.StartContainer(containerID, nil)
		if err != nil {
			log.Errorf("Couldn't start container ID %s for task %s: %v", containerID, task.Name, err)
			return
		}

		log.Debugf("[starting] task %s ID %s", task.Name, containerID)

		c.Lock()
		c.taskContainers[task.Name][containerID].state = "starting"
		c.Unlock()
	}()
}

// stopTask kills the last container we know about of this type
func (c *DockerScheduler) stopTask(task *demand.Task) error {
	var err error

	// Kill a currently-running container of this type
	c.Lock()
	theseContainers := c.taskContainers[task.Name]
	var containerToKill string
	for id, v := range theseContainers {
		if v.state == "running" {
			containerToKill = id
			v.state = "stopping"
			break
		}
	}
	c.Unlock()

	if containerToKill == "" {
		return fmt.Errorf("[stop] No containers of type %s to kill", task.Name)
	}

	removeOpts := docker.RemoveContainerOptions{
		ID:            containerToKill,
		RemoveVolumes: true,
	}

	go func() {
		scaling.Add(1)
		defer scaling.Done()

		log.Debugf("[stopping] container for task %s with ID %s", task.Name, containerToKill)
		err = c.client.StopContainer(containerToKill, 1)
		if err != nil {
			log.Errorf("Couldn't stop container %s: %v", containerToKill, err)
			return
		}

		c.Lock()
		c.taskContainers[task.Name][containerToKill].state = "removing"
		c.Unlock()

		log.Debugf("[removing] container for task %s with ID %s", task.Name, containerToKill)
		err = c.client.RemoveContainer(removeOpts)
		if err != nil {
			log.Errorf("Couldn't remove container %s: %v", containerToKill, err)
			return
		}
	}()

	return nil
}

// StopStartTasks creates containers if there aren't enough of them, and stop them if there are too many
func (c *DockerScheduler) StopStartTasks(tasks *demand.Tasks) error {
	var tooMany []*demand.Task
	var tooFew []*demand.Task
	var diff int
	var err error

	tasks.Lock()
	defer tasks.Unlock()

	// TODO: Consider checking the number running before we start & stop
	// Don't do more scaling if this task is already changin
	for _, task := range tasks.Tasks {
		if task.Demand > task.Requested && task.Requested == task.Running {
			// There aren't enough of these containers yet
			tooFew = append(tooFew, task)
		}

		if task.Demand < task.Requested && task.Requested == task.Running {
			// There aren't enough of these containers yet
			tooMany = append(tooMany, task)
		}
	}

	// Scale down first to free up resources
	for _, task := range tooMany {
		diff = task.Requested - task.Demand
		log.Infof("Stop %d of task %s", diff, task.Name)
		for i := 0; i < diff; i++ {
			err = c.stopTask(task)
			if err != nil {
				log.Errorf("Couldn't stop %s: %v ", task.Name, err)
			}
			task.Requested--
		}
	}

	// Now we can scale up
	for _, task := range tooFew {
		diff = task.Demand - task.Requested
		log.Infof("Start %d of task %s", diff, task.Name)
		for i := 0; i < diff; i++ {
			c.startTask(task)
			task.Requested++
		}
	}

	// Don't return until all the scale tasks are complete
	scaling.Wait()
	return err
}

func statusToState(status string) string {
	if strings.Contains(status, "Up") {
		return "running"
	}
	if strings.Contains(status, "Removal") {
		return "removing"
	}
	if strings.Contains(status, "Exit") {
		return "exited"
	}
	if strings.Contains(status, "Dead") {
		return "dead"
	}
	log.Errorf("Unexpected docker status %s", status)
	return "unknown"
}

// CountAllTasks checks how many of each task are running
func (c *DockerScheduler) CountAllTasks(running *demand.Tasks) error {
	// Docker Remote API https://docs.docker.com/reference/api/docker_remote_api_v1.20/
	// get /containers/json
	var err error
	var containers []docker.APIContainers
	containers, err = c.client.ListContainers(docker.ListContainersOptions{})
	if err != nil {
		return fmt.Errorf("Failed to list containers: %v", err)
	}

	running.Lock()
	defer running.Unlock()
	c.Lock()
	defer c.Unlock()

	// Reset all the running counts to 0
	tasks := running.Tasks
	for _, t := range tasks {
		t.Running = 0

		for _, cc := range c.taskContainers[t.Name] {
			cc.updated = false
		}
	}

	var taskName string
	var present bool

	for i := range containers {
		labels := containers[i].Labels
		taskName, present = labels[labelMap]
		if present {
			// Only update tasks that are already in our task map - don't try to manage anything else
			// log.Debugf("Found a container with labels %v", labels)
			t, err := running.GetTask(taskName)
			if err != nil {
				log.Errorf("Received info about task %s that we're not managing", taskName)
			} else {
				newState := statusToState(containers[i].Status)
				id := containers[i].ID[:12]
				thisContainer, ok := c.taskContainers[taskName][id]
				if !ok {
					log.Infof("We have no previous record of container %s, state %s", id, newState)
					thisContainer = &dockerContainer{}
					c.taskContainers[taskName][id] = thisContainer
				}

				switch newState {
				case "running":
					t.Running++
					// We could be moving from starting to running, or it could be a container that's totally new to us
					if thisContainer.state == "starting" || thisContainer.state == "" {
						thisContainer.state = newState
					}
				case "removing":
					if thisContainer.state != "removing" {
						log.Errorf("Container %s is being removed, but we didn't terminate it", id)
					}
				case "exited":
					if thisContainer.state != "stopping" && thisContainer.state != "exited" {
						log.Errorf("Container %s is being removed, but we didn't terminate it", id)
					}
				case "dead":
					if thisContainer.state != "dead" {
						log.Errorf("Container %s is dead", id)
					}
					thisContainer.state = newState
				}

				thisContainer.updated = true
			}
		}
	}

	for _, task := range tasks {
		log.Debugf("  %s: internally running %d, requested %d", task.Name, task.Running, task.Requested)
		for id, cc := range c.taskContainers[task.Name] {
			log.Debugf("  %s - %s", id, cc.state)
			if !cc.updated {
				if cc.state == "removing" || cc.state == "exited" {
					log.Debugf("    Deleting %s", id)
					delete(c.taskContainers[task.Name], id)
				} else if cc.state != "created" && cc.state != "starting" && cc.state != "stopping" {
					log.Errorf("Bad state for container %s: %s", id, cc.state)
				}
			}
		}
	}

	return err
}

// Cleanup gives the scheduler an opportunity to stop anything that needs to be stopped
func (c *DockerScheduler) Cleanup() error {
	return nil
}
