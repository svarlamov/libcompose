package docker

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/docker/engine-api/client"
	"github.com/docker/go-connections/nat"
	"github.com/hyperhq/libcompose/config"
	"github.com/hyperhq/libcompose/labels"
	"github.com/hyperhq/libcompose/project"
	"github.com/hyperhq/libcompose/project/options"
	"github.com/hyperhq/libcompose/utils"
	"golang.org/x/net/context"
)

// Service is a project.Service implementations.
type Service struct {
	name          string
	serviceConfig *config.ServiceConfig
	context       *Context
}

// NewService creates a service
func NewService(name string, serviceConfig *config.ServiceConfig, context *Context) *Service {
	return &Service{
		name:          name,
		serviceConfig: serviceConfig,
		context:       context,
	}
}

// Name returns the service name.
func (s *Service) Name() string {
	return s.name
}

// Config returns the configuration of the service (config.ServiceConfig).
func (s *Service) Config() *config.ServiceConfig {
	return s.serviceConfig
}

// DependentServices returns the dependent services (as an array of ServiceRelationship) of the service.
func (s *Service) DependentServices() []project.ServiceRelationship {
	return project.DefaultDependentServices(s.context.Project, s)
}

// Create implements Service.Create. It ensures the image exists or build it
// if it can and then create a container.
func (s *Service) Create(options options.Create) error {
	containers, err := s.collectContainers()
	if err != nil {
		return err
	}

	imageName, err := s.ensureImageExists(options.NoBuild)
	if err != nil {
		return err
	}

	if len(containers) != 0 {
		return s.eachContainer(func(c *Container) error {
			return s.recreateIfNeeded(imageName, c, options.NoRecreate, options.ForceRecreate)
		})
	}

	_, err = s.createOne(imageName)
	return err
}

func (s *Service) collectContainers() ([]*Container, error) {
	client := s.context.ClientFactory.Create(s)
	containers, err := GetContainersByFilter(client, labels.SERVICE.Eq(s.name), labels.PROJECT.Eq(s.context.Project.Name))
	if err != nil {
		return nil, err
	}

	result := []*Container{}

	for _, container := range containers {
		containerNumber, err := strconv.Atoi(container.Labels[labels.NUMBER.Str()])
		if err != nil {
			return nil, err
		}
		// Compose add "/" before name, so Name[1] will store actaul name.
		name := strings.SplitAfter(container.Names[0], "/")
		result = append(result, NewContainer(client, name[1], containerNumber, s))
	}

	return result, nil
}

func (s *Service) createOne(imageName string) (*Container, error) {
	containers, err := s.constructContainers(imageName, 1)
	if err != nil {
		return nil, err
	}

	return containers[0], err
}

func (s *Service) ensureImageExists(noBuild bool) (string, error) {
	err := s.imageExists()

	if err == nil {
		return s.imageName(), nil
	}

	if err != nil && !client.IsErrImageNotFound(err) {
		return "", err
	}

	/*
		if s.Config().Build.Context != "" {
			if noBuild {
				return "", fmt.Errorf("Service %q needs to be built, but no-build was specified", s.name)
			}
			return s.imageName(), s.build(options.Build{})
		}
	*/

	return s.imageName(), s.Pull()
}

func (s *Service) imageExists() error {
	client := s.context.ClientFactory.Create(s)

	_, _, err := client.ImageInspectWithRaw(context.Background(), s.imageName(), false)
	return err
}

func (s *Service) imageName() string {
	if s.Config().Image != "" {
		return s.Config().Image
	}
	return fmt.Sprintf("%s_%s", s.context.ProjectName, s.Name())
}

// Build implements Service.Build. If an imageName is specified or if the context has
// no build to work with it will do nothing. Otherwise it will try to build
// the image and returns an error if any.
func (s *Service) Build(buildOptions options.Build) error {
	if s.Config().Image != "" {
		return nil
	}
	return s.build(buildOptions)
}

func (s *Service) build(buildOptions options.Build) error {
	return nil
}

func (s *Service) constructContainers(imageName string, count int) ([]*Container, error) {
	result, err := s.collectContainers()
	if err != nil {
		return nil, err
	}

	client := s.context.ClientFactory.Create(s)

	var namer Namer

	if s.serviceConfig.ContainerName != "" {
		if count > 1 {
			logrus.Warnf(`The "%s" service is using the custom container name "%s". Docker requires each container to have a unique name. Remove the custom name to scale the service.`, s.name, s.serviceConfig.ContainerName)
		}
		namer = NewSingleNamer(s.serviceConfig.ContainerName)
	} else {
		namer, err = NewNamer(client, s.context.Project.Name, s.name, false)
		if err != nil {
			return nil, err
		}
	}

	for i := len(result); i < count; i++ {
		containerName, containerNumber := namer.Next()

		c := NewContainer(client, containerName, containerNumber, s)

		dockerContainer, err := c.Create(imageName)
		if err != nil {
			return nil, err
		}

		logrus.Debugf("Created container %s: %v", dockerContainer.ID, dockerContainer.Name)

		result = append(result, NewContainer(client, containerName, containerNumber, s))
	}

	return result, nil
}

// Up implements Service.Up. It builds the image if needed, creates a container
// and start it.
func (s *Service) Up(options options.Up) error {
	containers, err := s.collectContainers()
	if err != nil {
		return err
	}

	var imageName = s.imageName()
	if len(containers) == 0 || !options.NoRecreate {
		imageName, err = s.ensureImageExists(options.NoBuild)
		if err != nil {
			return err
		}
	}

	return s.up(imageName, true, options)
}

// Run implements Service.Run. It runs a one of command within the service container.
func (s *Service) Run(ctx context.Context, commandParts []string) (int, error) {
	imageName, err := s.ensureImageExists(false)
	if err != nil {
		return -1, err
	}

	client := s.context.ClientFactory.Create(s)

	namer, err := NewNamer(client, s.context.Project.Name, s.name, true)
	if err != nil {
		return -1, err
	}

	containerName, containerNumber := namer.Next()

	c := NewOneOffContainer(client, containerName, containerNumber, s)

	return c.Run(ctx, imageName, &config.ServiceConfig{Command: commandParts, Tty: true, StdinOpen: true})
}

// Info implements Service.Info. It returns an project.InfoSet with the containers
// related to this service (can be multiple if using the scale command).
func (s *Service) Info(qFlag bool) (project.InfoSet, error) {
	result := project.InfoSet{}
	containers, err := s.collectContainers()
	if err != nil {
		return nil, err
	}

	for _, c := range containers {
		info, err := c.Info(qFlag)
		if err != nil {
			return nil, err
		}
		result = append(result, info)
	}

	return result, nil
}

// Start implements Service.Start. It tries to start a container without creating it.
func (s *Service) Start() error {
	return s.up("", false, options.Up{})
}

func (s *Service) up(imageName string, create bool, options options.Up) error {
	containers, err := s.collectContainers()
	if err != nil {
		return err
	}

	logrus.Debugf("Found %d existing containers for service %s", len(containers), s.name)

	if len(containers) == 0 && create {
		c, err := s.createOne(imageName)
		if err != nil {
			return err
		}
		containers = []*Container{c}
	}

	return s.eachContainer(func(c *Container) error {
		if create {
			if err := s.recreateIfNeeded(imageName, c, options.NoRecreate, options.ForceRecreate); err != nil {
				return err
			}
		}

		return c.Up(imageName)
	})
}

func (s *Service) recreateIfNeeded(imageName string, c *Container, noRecreate, forceRecreate bool) error {
	if noRecreate {
		return nil
	}
	outOfSync, err := c.OutOfSync(imageName)
	if err != nil {
		return err
	}

	logrus.WithFields(logrus.Fields{
		"outOfSync":     outOfSync,
		"ForceRecreate": forceRecreate,
		"NoRecreate":    noRecreate}).Debug("Going to decide if recreate is needed")

	if forceRecreate || outOfSync {
		logrus.Infof("Recreating %s", s.name)
		if _, err := c.Recreate(imageName); err != nil {
			return err
		}
	}

	return nil
}

func (s *Service) eachContainer(action func(*Container) error) error {
	containers, err := s.collectContainers()
	if err != nil {
		return err
	}

	tasks := utils.InParallel{}
	for _, container := range containers {
		task := func(container *Container) func() error {
			return func() error {
				return action(container)
			}
		}(container)

		tasks.Add(task)
	}

	return tasks.Wait()
}

// Stop implements Service.Stop. It stops any containers related to the service.
func (s *Service) Stop(timeout int) error {
	return s.eachContainer(func(c *Container) error {
		return c.Stop(timeout)
	})
}

// Restart implements Service.Restart. It restarts any containers related to the service.
func (s *Service) Restart(timeout int) error {
	return s.eachContainer(func(c *Container) error {
		return c.Restart(timeout)
	})
}

// Kill implements Service.Kill. It kills any containers related to the service.
func (s *Service) Kill(signal string) error {
	return s.eachContainer(func(c *Container) error {
		return c.Kill(signal)
	})
}

// Delete implements Service.Delete. It removes any containers related to the service.
func (s *Service) Delete(options options.Delete) error {
	return s.eachContainer(func(c *Container) error {
		return c.Delete(options.RemoveVolume)
	})
}

// Log implements Service.Log. It returns the docker logs for each container related to the service.
func (s *Service) Log(follow bool) error {
	return s.eachContainer(func(c *Container) error {
		return c.Log(follow)
	})
}

// Scale implements Service.Scale. It creates or removes containers to have the specified number
// of related container to the service to run.
func (s *Service) Scale(scale int, timeout int) error {
	if s.specificiesHostPort() {
		logrus.Warnf("The \"%s\" service specifies a port on the host. If multiple containers for this service are created on a single host, the port will clash.", s.Name())
	}

	foundCount := 0
	err := s.eachContainer(func(c *Container) error {
		foundCount++
		if foundCount > scale {
			err := c.Stop(timeout)
			if err != nil {
				return err
			}
			// FIXME(vdemeester) remove volume in scale by default ?
			return c.Delete(false)
		}
		return nil
	})

	if err != nil {
		return err
	}

	if foundCount != scale {
		imageName, err := s.ensureImageExists(false)
		if err != nil {
			return err
		}

		if _, err = s.constructContainers(imageName, scale); err != nil {
			return err
		}
	}

	return s.up("", false, options.Up{})
}

// Pull implements Service.Pull. It pulls the image of the service and skip the service that
// would need to be built.
func (s *Service) Pull() error {
	if s.Config().Image == "" {
		return nil
	}

	return pullImage(s.context.ClientFactory.Create(s), s, s.Config().Image)
}

// Pause implements Service.Pause. It puts into pause the container(s) related
// to the service.
func (s *Service) Pause() error {
	return s.eachContainer(func(c *Container) error {
		return c.Pause()
	})
}

// Unpause implements Service.Pause. It brings back from pause the container(s)
// related to the service.
func (s *Service) Unpause() error {
	return s.eachContainer(func(c *Container) error {
		return c.Unpause()
	})
}

// RemoveImage implements Service.RemoveImage. It removes images used for the service
// depending on the specified type.
func (s *Service) RemoveImage(imageType options.ImageType) error {
	switch imageType {
	case "local":
		if s.Config().Image != "" {
			return nil
		}
		return removeImage(s.context.ClientFactory.Create(s), s.imageName())
	case "all":
		return removeImage(s.context.ClientFactory.Create(s), s.imageName())
	default:
		// Don't do a thing, should be validated up-front
		return nil
	}
}

// Containers implements Service.Containers. It returns the list of containers
// that are related to the service.
func (s *Service) Containers() ([]project.Container, error) {
	result := []project.Container{}
	containers, err := s.collectContainers()
	if err != nil {
		return nil, err
	}

	for _, c := range containers {
		result = append(result, c)
	}

	return result, nil
}

func (s *Service) specificiesHostPort() bool {
	_, bindings, err := nat.ParsePortSpecs(s.Config().Ports)

	if err != nil {
		fmt.Println(err)
	}

	for _, portBindings := range bindings {
		for _, portBinding := range portBindings {
			if portBinding.HostPort != "" {
				return true
			}
		}
	}

	return false
}
