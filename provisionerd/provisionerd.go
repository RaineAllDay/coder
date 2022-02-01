package provisionerd

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"go.uber.org/atomic"

	"cdr.dev/slog"
	"github.com/coder/coder/provisionerd/proto"
	sdkproto "github.com/coder/coder/provisionersdk/proto"
	"github.com/coder/retry"
)

// Dialer represents the function to create a daemon client connection.
type Dialer func(ctx context.Context) (proto.DRPCProvisionerDaemonClient, error)

// Provisioners maps provisioner ID to implementation.
type Provisioners map[string]sdkproto.DRPCProvisionerClient

// Options provides customizations to the behavior of a provisioner daemon.
type Options struct {
	Logger slog.Logger

	PollInterval  time.Duration
	Provisioners  Provisioners
	WorkDirectory string
}

// New creates and starts a provisioner daemon.
func New(clientDialer Dialer, opts *Options) io.Closer {
	if opts.PollInterval == 0 {
		opts.PollInterval = 5 * time.Second
	}
	ctx, ctxCancel := context.WithCancel(context.Background())
	daemon := &provisionerDaemon{
		clientDialer: clientDialer,
		opts:         opts,

		closeContext: ctx,
		closeCancel:  ctxCancel,
		closed:       make(chan struct{}),
	}
	go daemon.connect(ctx)
	return daemon
}

type provisionerDaemon struct {
	opts *Options

	clientDialer Dialer
	connectMutex sync.Mutex
	client       proto.DRPCProvisionerDaemonClient
	updateStream proto.DRPCProvisionerDaemon_UpdateJobClient

	// Only use for ending a job.
	closeContext context.Context
	closeCancel  context.CancelFunc
	closed       chan struct{}
	closeMutex   sync.Mutex
	closeError   error

	// Lock on acquiring a job so two can't happen at once...?
	// If a single cancel can happen, but an acquire could happen?

	// Lock on acquire
	// Use atomic for checking if we are running a job
	// Use atomic for checking if we are canceling job
	// If we're running a job, wait for the done chan in
	// close.

	acquiredJob          *proto.AcquiredJob
	acquiredJobMutex     sync.Mutex
	acquiredJobCancel    context.CancelFunc
	acquiredJobCancelled atomic.Bool
	acquiredJobRunning   atomic.Bool
	acquiredJobDone      chan struct{}
}

// Connnect establishes a connection to coderd.
func (p *provisionerDaemon) connect(ctx context.Context) {
	p.connectMutex.Lock()
	defer p.connectMutex.Unlock()

	var err error
	// An exponential back-off occurs when the connection is failing to dial.
	// This is to prevent server spam in case of a coderd outage.
	for retrier := retry.New(50*time.Millisecond, 10*time.Second); retrier.Wait(ctx); {
		p.client, err = p.clientDialer(ctx)
		if err != nil {
			// Warn
			p.opts.Logger.Warn(context.Background(), "failed to dial", slog.Error(err))
			continue
		}
		p.updateStream, err = p.client.UpdateJob(ctx)
		if err != nil {
			p.opts.Logger.Warn(context.Background(), "create update job stream", slog.Error(err))
			continue
		}
		p.opts.Logger.Debug(context.Background(), "connected")
		break
	}

	go func() {
		if p.isClosed() {
			return
		}
		select {
		case <-p.closed:
			return
		case <-p.updateStream.Context().Done():
			// We use the update stream to detect when the connection
			// has been interrupted. This works well, because logs need
			// to buffer if a job is running in the background.
			p.opts.Logger.Debug(context.Background(), "update stream ended", slog.Error(p.updateStream.Context().Err()))
			p.connect(ctx)
		}
	}()

	go func() {
		if p.isClosed() {
			return
		}
		ticker := time.NewTicker(p.opts.PollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-p.closed:
				return
			case <-p.updateStream.Context().Done():
				return
			case <-ticker.C:
				p.acquireJob(ctx)
			}
		}
	}()
}

// Locks a job in the database, and runs it!
func (p *provisionerDaemon) acquireJob(ctx context.Context) {
	p.acquiredJobMutex.Lock()
	defer p.acquiredJobMutex.Unlock()
	if p.isRunningJob() {
		p.opts.Logger.Debug(context.Background(), "skipping acquire; job is already running")
		return
	}
	var err error
	p.acquiredJob, err = p.client.AcquireJob(ctx, &proto.Empty{})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		p.opts.Logger.Warn(context.Background(), "acquire job", slog.Error(err))
		return
	}
	if p.isClosed() {
		return
	}
	if p.acquiredJob.JobId == "" {
		p.opts.Logger.Debug(context.Background(), "no jobs available")
		return
	}
	ctx, p.acquiredJobCancel = context.WithCancel(ctx)
	p.acquiredJobCancelled.Store(false)
	p.acquiredJobRunning.Store(true)
	p.acquiredJobDone = make(chan struct{})

	p.opts.Logger.Info(context.Background(), "acquired job",
		slog.F("organization_name", p.acquiredJob.OrganizationName),
		slog.F("project_name", p.acquiredJob.ProjectName),
		slog.F("username", p.acquiredJob.UserName),
		slog.F("provisioner", p.acquiredJob.Provisioner),
	)

	go p.runJob(ctx)
}

func (p *provisionerDaemon) isRunningJob() bool {
	return p.acquiredJobRunning.Load()
}

func (p *provisionerDaemon) runJob(ctx context.Context) {
	go func() {
		select {
		case <-p.closed:
		case <-ctx.Done():
		}

		// Cleanup the work directory after execution.
		err := os.RemoveAll(p.opts.WorkDirectory)
		if err != nil {
			p.cancelActiveJob(fmt.Sprintf("remove all from %q directory: %s", p.opts.WorkDirectory, err))
			return
		}
		p.opts.Logger.Debug(ctx, "cleaned up work directory")
		p.acquiredJobMutex.Lock()
		defer p.acquiredJobMutex.Unlock()
		p.acquiredJobRunning.Store(false)
		close(p.acquiredJobDone)
	}()
	// It's safe to cast this ProvisionerType. This data is coming directly from coderd.
	provisioner, hasProvisioner := p.opts.Provisioners[p.acquiredJob.Provisioner]
	if !hasProvisioner {
		p.cancelActiveJob(fmt.Sprintf("provisioner %q not registered", p.acquiredJob.Provisioner))
		return
	}

	err := os.MkdirAll(p.opts.WorkDirectory, 0600)
	if err != nil {
		p.cancelActiveJob(fmt.Sprintf("create work directory %q: %s", p.opts.WorkDirectory, err))
		return
	}

	p.opts.Logger.Info(ctx, "unpacking project source archive", slog.F("size_bytes", len(p.acquiredJob.ProjectSourceArchive)))
	reader := tar.NewReader(bytes.NewBuffer(p.acquiredJob.ProjectSourceArchive))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			p.cancelActiveJob(fmt.Sprintf("read project source archive: %s", err))
			return
		}
		// #nosec
		path := filepath.Join(p.opts.WorkDirectory, header.Name)
		if !strings.HasPrefix(path, filepath.Clean(p.opts.WorkDirectory)) {
			p.cancelActiveJob("tar attempts to target relative upper directory")
			return
		}
		mode := header.FileInfo().Mode()
		if mode == 0 {
			mode = 0600
		}
		switch header.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(path, mode)
			if err != nil {
				p.cancelActiveJob(fmt.Sprintf("mkdir %q: %s", path, err))
				return
			}
			p.opts.Logger.Debug(context.Background(), "extracted directory", slog.F("path", path))
		case tar.TypeReg:
			file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, mode)
			if err != nil {
				p.cancelActiveJob(fmt.Sprintf("create file %q: %s", path, err))
				return
			}
			// Max file size of 10MB.
			size, err := io.CopyN(file, reader, (1<<20)*10)
			if errors.Is(err, io.EOF) {
				err = nil
			}
			if err != nil {
				p.cancelActiveJob(fmt.Sprintf("copy file %q: %s", path, err))
				return
			}
			err = file.Close()
			if err != nil {
				p.cancelActiveJob(fmt.Sprintf("close file %q: %s", path, err))
				return
			}
			p.opts.Logger.Debug(context.Background(), "extracted file",
				slog.F("size_bytes", size),
				slog.F("path", path),
				slog.F("mode", mode),
			)
		}
	}

	switch jobType := p.acquiredJob.Type.(type) {
	case *proto.AcquiredJob_ProjectImport_:
		p.opts.Logger.Debug(context.Background(), "acquired job is project import",
			slog.F("project_history_name", jobType.ProjectImport.ProjectHistoryName),
		)

		p.runProjectImport(ctx, provisioner, jobType)
	case *proto.AcquiredJob_WorkspaceProvision_:
		p.opts.Logger.Debug(context.Background(), "acquired job is workspace provision",
			slog.F("workspace_name", jobType.WorkspaceProvision.WorkspaceName),
			slog.F("state_length", len(jobType.WorkspaceProvision.State)),
			slog.F("parameters", jobType.WorkspaceProvision.ParameterValues),
		)

		p.runWorkspaceProvision(ctx, provisioner, jobType)
	default:
		p.cancelActiveJob(fmt.Sprintf("unknown job type %q; ensure your provisioner daemon is up-to-date", reflect.TypeOf(p.acquiredJob.Type).String()))
		return
	}

	p.acquiredJobCancel()
	p.opts.Logger.Info(context.Background(), "completed job")
}

func (p *provisionerDaemon) runProjectImport(ctx context.Context, provisioner sdkproto.DRPCProvisionerClient, job *proto.AcquiredJob_ProjectImport_) {
	stream, err := provisioner.Parse(ctx, &sdkproto.Parse_Request{
		Directory: p.opts.WorkDirectory,
	})
	if err != nil {
		p.cancelActiveJob(fmt.Sprintf("parse source: %s", err))
		return
	}
	defer stream.Close()
	for {
		msg, err := stream.Recv()
		if err != nil {
			p.cancelActiveJob(fmt.Sprintf("recv parse source: %s", err))
			return
		}
		switch msgType := msg.Type.(type) {
		case *sdkproto.Parse_Response_Log:
			p.opts.Logger.Debug(context.Background(), "parse job logged",
				slog.F("level", msgType.Log.Level),
				slog.F("output", msgType.Log.Output),
				slog.F("project_history_id", job.ProjectImport.ProjectHistoryId),
			)

			err = p.updateStream.Send(&proto.JobUpdate{
				JobId: p.acquiredJob.JobId,
				ProjectImportLogs: []*proto.Log{{
					Source:    proto.LogSource_PROVISIONER,
					Level:     msgType.Log.Level,
					CreatedAt: time.Now().UTC().UnixMilli(),
					Output:    msgType.Log.Output,
				}},
			})
			if err != nil {
				p.cancelActiveJob(fmt.Sprintf("update job: %s", err))
				return
			}
		case *sdkproto.Parse_Response_Complete:
			_, err = p.client.CompleteJob(ctx, &proto.CompletedJob{
				JobId: p.acquiredJob.JobId,
				Type: &proto.CompletedJob_ProjectImport_{
					ProjectImport: &proto.CompletedJob_ProjectImport{
						ParameterSchemas: msgType.Complete.ParameterSchemas,
					},
				},
			})
			if err != nil {
				p.cancelActiveJob(fmt.Sprintf("complete job: %s", err))
				return
			}
			// Return so we stop looping!
			return
		default:
			p.cancelActiveJob(fmt.Sprintf("invalid message type %q received from provisioner",
				reflect.TypeOf(msg.Type).String()))
			return
		}
	}
}

func (p *provisionerDaemon) runWorkspaceProvision(ctx context.Context, provisioner sdkproto.DRPCProvisionerClient, job *proto.AcquiredJob_WorkspaceProvision_) {
	stream, err := provisioner.Provision(ctx, &sdkproto.Provision_Request{
		Directory:       p.opts.WorkDirectory,
		ParameterValues: job.WorkspaceProvision.ParameterValues,
		State:           job.WorkspaceProvision.State,
	})
	if err != nil {
		p.cancelActiveJob(fmt.Sprintf("provision: %s", err))
		return
	}
	defer stream.Close()

	for {
		msg, err := stream.Recv()
		if err != nil {
			p.cancelActiveJob(fmt.Sprintf("recv workspace provision: %s", err))
			return
		}
		switch msgType := msg.Type.(type) {
		case *sdkproto.Provision_Response_Log:
			p.opts.Logger.Debug(context.Background(), "workspace provision job logged",
				slog.F("level", msgType.Log.Level),
				slog.F("output", msgType.Log.Output),
				slog.F("workspace_history_id", job.WorkspaceProvision.WorkspaceHistoryId),
			)

			err = p.updateStream.Send(&proto.JobUpdate{
				JobId: p.acquiredJob.JobId,
				WorkspaceProvisionLogs: []*proto.Log{{
					Source:    proto.LogSource_PROVISIONER,
					Level:     msgType.Log.Level,
					CreatedAt: time.Now().UTC().UnixMilli(),
					Output:    msgType.Log.Output,
				}},
			})
			if err != nil {
				p.cancelActiveJob(fmt.Sprintf("send job update: %s", err))
				return
			}
		case *sdkproto.Provision_Response_Complete:
			p.opts.Logger.Info(context.Background(), "provision successful; marking job as complete",
				slog.F("resource_count", len(msgType.Complete.Resources)),
				slog.F("resources", msgType.Complete.Resources),
				slog.F("state_length", len(msgType.Complete.State)),
			)

			// Complete job may need to be async if we disconnected...
			// When we reconnect we can flush any of these cached values.
			_, err = p.client.CompleteJob(ctx, &proto.CompletedJob{
				JobId: p.acquiredJob.JobId,
				Type: &proto.CompletedJob_WorkspaceProvision_{
					WorkspaceProvision: &proto.CompletedJob_WorkspaceProvision{
						State:     msgType.Complete.State,
						Resources: msgType.Complete.Resources,
					},
				},
			})
			if err != nil {
				p.cancelActiveJob(fmt.Sprintf("complete job: %s", err))
				return
			}
			// Return so we stop looping!
			return
		default:
			p.cancelActiveJob(fmt.Sprintf("invalid message type %q received from provisioner",
				reflect.TypeOf(msg.Type).String()))
			return
		}
	}
}

func (p *provisionerDaemon) cancelActiveJob(errMsg string) {
	if !p.isRunningJob() {
		p.opts.Logger.Warn(context.Background(), "skipping job cancel; none running", slog.F("error_message", errMsg))
		return
	}
	if p.acquiredJobCancelled.Load() {
		return
	}
	p.acquiredJobCancelled.Store(true)
	p.acquiredJobCancel()
	p.opts.Logger.Info(context.Background(), "canceling running job",
		slog.F("error_message", errMsg),
		slog.F("job_id", p.acquiredJob.JobId),
	)
	_, err := p.client.CancelJob(p.closeContext, &proto.CancelledJob{
		JobId: p.acquiredJob.JobId,
		Error: fmt.Sprintf("provisioner daemon: %s", errMsg),
	})
	if err != nil {
		p.opts.Logger.Warn(context.Background(), "failed to notify of cancel; job is no longer running", slog.Error(err))
		return
	}
	p.opts.Logger.Debug(context.Background(), "canceled running job")
}

// isClosed returns whether the API is closed or not.
func (p *provisionerDaemon) isClosed() bool {
	select {
	case <-p.closed:
		return true
	default:
		return false
	}
}

// Close ends the provisioner. It will mark any running jobs as canceled.
func (p *provisionerDaemon) Close() error {
	return p.closeWithError(nil)
}

// closeWithError closes the provisioner; subsequent reads/writes will return the error err.
func (p *provisionerDaemon) closeWithError(err error) error {
	p.closeMutex.Lock()
	defer p.closeMutex.Unlock()
	if p.isClosed() {
		return p.closeError
	}

	if p.isRunningJob() {
		errMsg := "provisioner daemon was shutdown gracefully"
		if err != nil {
			errMsg = err.Error()
		}
		if !p.acquiredJobCancelled.Load() {
			p.cancelActiveJob(errMsg)
		}
		<-p.acquiredJobDone
	}

	p.opts.Logger.Debug(context.Background(), "closing server with error", slog.Error(err))
	p.closeError = err
	close(p.closed)
	p.closeCancel()

	if p.updateStream != nil {
		_ = p.client.DRPCConn().Close()
		_ = p.updateStream.Close()
	}

	return err
}
