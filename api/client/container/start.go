package container

import (
	"fmt"
	"io"
	"net/http/httputil"
	"strings"

	"golang.org/x/net/context"

	"github.com/docker/docker/api/client"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/cli"
	"github.com/docker/docker/pkg/promise"
	"github.com/docker/docker/pkg/signal"
	"github.com/spf13/cobra"
)

type startOptions struct {
	attach     bool
	openStdin  bool
	detachKeys string

	containers []string
}

// NewStartCommand creates a new cobra.Command for `docker start`
func NewStartCommand(dockerCli *client.DockerCli) *cobra.Command {
	var opts startOptions

	cmd := &cobra.Command{
		Use:   "start [OPTIONS] CONTAINER [CONTAINER...]",
		Short: "Start one or more stopped containers",
		Args:  cli.RequiresMinArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.containers = args
			return runStart(dockerCli, &opts)
		},
	}

	flags := cmd.Flags()
	flags.BoolVarP(&opts.attach, "attach", "a", false, "Attach STDOUT/STDERR and forward signals")
	flags.BoolVarP(&opts.openStdin, "interactive", "i", false, "Attach container's STDIN")
	flags.StringVar(&opts.detachKeys, "detach-keys", "", "Override the key sequence for detaching a container")
	return cmd
}

func runStart(dockerCli *client.DockerCli, opts *startOptions) error {
	ctx, cancelFun := context.WithCancel(context.Background())

	if opts.attach || opts.openStdin {
		// We're going to attach to a container.
		// 1. Ensure we only have one container.
		if len(opts.containers) > 1 {
			return fmt.Errorf("You cannot start and attach multiple containers at once.")
		}

		// 2. Attach to the container.
		container := opts.containers[0]
		c, err := dockerCli.Client().ContainerInspect(ctx, container)
		if err != nil {
			return err
		}

		// We always use c.ID instead of container to maintain consistency during `docker start`
		if !c.Config.Tty {
			sigc := ForwardAllSignals(ctx, dockerCli, c.ID)
			defer signal.StopCatch(sigc)
		}

		if opts.detachKeys != "" {
			dockerCli.ConfigFile().DetachKeys = opts.detachKeys
		}

		options := types.ContainerAttachOptions{
			Stream:     true,
			Stdin:      opts.openStdin && c.Config.OpenStdin,
			Stdout:     true,
			Stderr:     true,
			DetachKeys: dockerCli.ConfigFile().DetachKeys,
		}

		var in io.ReadCloser

		if options.Stdin {
			in = dockerCli.In()
		}

		resp, errAttach := dockerCli.Client().ContainerAttach(ctx, c.ID, options)
		if errAttach != nil && errAttach != httputil.ErrPersistEOF {
			// ContainerAttach return an ErrPersistEOF (connection closed)
			// means server met an error and already put it in Hijacked connection,
			// we would keep the error and read the detailed error message from hijacked connection
			return errAttach
		}
		defer resp.Close()
		cErr := promise.Go(func() error {
			errHijack := holdHijackedConnection(ctx, dockerCli, c.Config.Tty, in, dockerCli.Out(), dockerCli.Err(), resp)
			if errHijack == nil {
				return errAttach
			}
			return errHijack
		})

		// 3. We should open a channel for receiving status code of the container
		// no matter it's detached, removed on daemon side(--rm) or exit normally.
		statusChan, statusErr := waitExitOrRemoved(dockerCli, context.Background(), c.ID, c.HostConfig.AutoRemove)

		// 4. Start the container.
		if err := dockerCli.Client().ContainerStart(ctx, c.ID, types.ContainerStartOptions{}); err != nil {
			cancelFun()
			<-cErr
			if c.HostConfig.AutoRemove && statusErr == nil {
				// wait container to be removed
				<-statusChan
			}
			return err
		}

		// 5. Wait for attachment to break.
		if c.Config.Tty && dockerCli.Out().IsTerminal() {
			if err := MonitorTtySize(ctx, dockerCli, c.ID, false); err != nil {
				fmt.Fprintf(dockerCli.Err(), "Error monitoring TTY size: %s\n", err)
			}
		}
		if attchErr := <-cErr; attchErr != nil {
			return attchErr
		}

		if statusErr != nil {
			return fmt.Errorf("can't get container's exit code: %v", statusErr)
		}

		if status := <-statusChan; status != 0 {
			return cli.StatusError{StatusCode: status}
		}
	} else {
		// We're not going to attach to anything.
		// Start as many containers as we want.
		return startContainersWithoutAttachments(dockerCli, ctx, opts.containers)
	}

	return nil
}

func startContainersWithoutAttachments(dockerCli *client.DockerCli, ctx context.Context, containers []string) error {
	var failedContainers []string
	for _, container := range containers {
		if err := dockerCli.Client().ContainerStart(ctx, container, types.ContainerStartOptions{}); err != nil {
			fmt.Fprintf(dockerCli.Err(), "%s\n", err)
			failedContainers = append(failedContainers, container)
		} else {
			fmt.Fprintf(dockerCli.Out(), "%s\n", container)
		}
	}

	if len(failedContainers) > 0 {
		return fmt.Errorf("Error: failed to start containers: %v", strings.Join(failedContainers, ", "))
	}
	return nil
}
