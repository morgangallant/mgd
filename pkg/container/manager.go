package container

import (
	"context"
	"sync"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/oci"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

// Manager is used to manage containers running on this machine.
type Manager interface {
	// Create creates a container.
	Create(ctx context.Context, imageUri string) (*Container, error)

	// Lists all the containers running on this machine.
	List(ctx context.Context) ([]*Container, error)

	// Delete terminates a container, gracefully, and deletes it.
	Delete(ctx context.Context, id string) error

	// Shutdown shuts down all containers, and the container manager.
	Shutdown(ctx context.Context) error
}

var (
	_ Manager = (*ContainerdManager)(nil)
)

// ContainerdManager is a containerd implementation of the container manager, using containerd.
type ContainerdManager struct {
	client *containerd.Client
	logger *zap.Logger

	mu         sync.RWMutex
	containers map[string]*Container
}

// NewContainerdManager creates a new containerd manager.
func NewContainerdManager(logger *zap.Logger, address string) (*ContainerdManager, error) {
	client, err := containerd.New(address)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create containerd client")
	}
	return &ContainerdManager{
		client:     client,
		logger:     logger,
		containers: make(map[string]*Container),
	}, nil
}

func (cm *ContainerdManager) manageTask(
	ctx context.Context,
	obj *Container,
	task containerd.Task,
) error {
	status, err := task.Wait(ctx)
	if err != nil {
		return errors.Wrap(err, "waiting for task")
	}

	if err := task.Start(ctx); err != nil {
		return errors.Wrap(err, "starting task")
	}

	// todo: task.Kill (SIGTERM) => status.Result()

	select {
	case exit := <-status:
		return errors.Errorf("task exited with code %d", exit.ExitCode())
	case <-obj.shutdown:
		return nil
	}
}

// Create creates a container.
func (cm *ContainerdManager) Create(ctx context.Context, imageUri string) (*Container, error) {
	image, err := cm.client.Pull(ctx, imageUri, containerd.WithPullUnpack)
	if err != nil {
		return nil, errors.Wrap(err, "failed to pull image")
	}

	id := uuid.NewString()

	obj := &Container{
		ID:       id,
		status:   Starting,
		ImageURI: imageUri,
		shutdown: make(chan struct{}),
	}

	cm.mu.Lock()
	cm.containers[id] = obj
	cm.mu.Unlock()

	container, err := cm.client.NewContainer(
		ctx,
		id,
		containerd.WithImage(image),
		// TODO: Add mounts, etc. + cgroup stuff.
		containerd.WithNewSpec(oci.WithImageConfig(image)),
	)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create container")
	}

	task, err := container.NewTask(ctx, cio.NewCreator(cio.WithStdio))
	if err != nil {
		return nil, errors.Wrap(err, "failed to create task")
	}

	go func() {
		if err := cm.manageTask(context.Background(), obj, task); err != nil {
			cm.logger.Error("task manager failed", zap.String("id", id), zap.Error(err))
			// TODO: Do some cleanup here.
		}
	}()

	return obj, nil
}

// Lists all the containers running on this machine.
func (cm *ContainerdManager) List(ctx context.Context) ([]*Container, error) {
	ret := make([]*Container, 0, len(cm.containers))
	for _, c := range cm.containers {
		ret = append(ret, c)
	}
	return ret, nil
}

// Delete terminates a container, gracefully, and deletes it.
func (cm *ContainerdManager) Delete(ctx context.Context, id string) error {
	panic("not implemented") // TODO: Implement
}

// Shutdown shuts down all containers, and the container manager.
func (cm *ContainerdManager) Shutdown(ctx context.Context) error {
	return cm.client.Close()
}
