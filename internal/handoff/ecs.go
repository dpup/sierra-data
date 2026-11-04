package handoff

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/dpup/prefab/logging"
)

// ECSIdentity is the ECS task and container this process runs in, as /status
// reports it. A deployment coordinator uses it to tell an ECS container
// restart (same task, same IP and port, new process) from a new task, and to
// prove which task answered.
//
// It is read from the ECS task metadata endpoint (v4) by this process after
// it started, and BoundToProcess says whether it is known to describe THIS
// incarnation: the container's ECS start time falls just before the
// process's own. Until then (metadata lags a restart) the controller keeps
// re-reading it. Without ECS (local runs) Available is false with a reason.
type ECSIdentity struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`

	TaskARN          string `json:"taskArn,omitempty"`
	Cluster          string `json:"cluster,omitempty"`
	Family           string `json:"family,omitempty"`
	Revision         string `json:"revision,omitempty"`
	AvailabilityZone string `json:"availabilityZone,omitempty"`

	ContainerName      string     `json:"containerName,omitempty"`
	ContainerARN       string     `json:"containerArn,omitempty"`
	DockerID           string     `json:"dockerId,omitempty"`
	ContainerStartedAt *time.Time `json:"containerStartedAt,omitempty"`
	// RestartCount is how many times ECS has restarted this container under
	// its restart policy. Null when ECS does not report it (no restart policy,
	// or a platform that omits it): unknown, never zero.
	RestartCount *int `json:"restartCount"`

	BoundToProcess bool       `json:"boundToProcess"`
	FetchedAt      *time.Time `json:"fetchedAt,omitempty"`
}

const (
	// ecsMetadataEnv is set by the ECS agent in every container.
	ecsMetadataEnv = "ECS_CONTAINER_METADATA_URI_V4"
	// ecsBindWindow is how long before the process start the container may
	// have started and still count as this incarnation (image entrypoint,
	// Go runtime start, config load).
	ecsBindWindow = 2 * time.Minute
	// ecsBindSkew tolerates clock skew between the agent and the container.
	ecsBindSkew = 5 * time.Second
)

// ecsContainerDoc and ecsTaskDoc are the fields read from the v4 endpoint's
// ${URI} and ${URI}/task responses.
type ecsContainerDoc struct {
	DockerID     string     `json:"DockerId"`
	Name         string     `json:"Name"`
	ContainerARN string     `json:"ContainerARN"`
	StartedAt    *time.Time `json:"StartedAt"`
	RestartCount *int       `json:"RestartCount"`
}

type ecsTaskDoc struct {
	TaskARN          string `json:"TaskARN"`
	Cluster          string `json:"Cluster"`
	Family           string `json:"Family"`
	Revision         string `json:"Revision"`
	AvailabilityZone string `json:"AvailabilityZone"`
}

// fetchECSIdentity reads the metadata endpoint once.
func fetchECSIdentity(ctx context.Context, uri string, processStart, now time.Time) ECSIdentity {
	if uri == "" {
		return ECSIdentity{Reason: ecsMetadataEnv + " is not set (not running on ECS)"}
	}
	var c ecsContainerDoc
	if err := getJSON(ctx, uri, &c); err != nil {
		return ECSIdentity{Reason: "container metadata: " + err.Error()}
	}
	var t ecsTaskDoc
	if err := getJSON(ctx, uri+"/task", &t); err != nil {
		return ECSIdentity{Reason: "task metadata: " + err.Error()}
	}
	id := ECSIdentity{
		Available: true, TaskARN: t.TaskARN, Cluster: t.Cluster, Family: t.Family,
		Revision: t.Revision, AvailabilityZone: t.AvailabilityZone,
		ContainerName: c.Name, ContainerARN: c.ContainerARN, DockerID: c.DockerID,
		ContainerStartedAt: c.StartedAt, RestartCount: c.RestartCount, FetchedAt: &now,
	}
	switch {
	case c.StartedAt == nil:
		id.Reason = "ECS reports no container start time, so the metadata cannot be tied to this process"
	case c.StartedAt.Before(processStart.Add(-ecsBindWindow)):
		id.Reason = fmt.Sprintf("ECS reports the container started at %s, well before this process (%s): "+
			"the metadata may describe a previous incarnation",
			c.StartedAt.UTC().Format(time.RFC3339), processStart.UTC().Format(time.RFC3339))
	case c.StartedAt.After(processStart.Add(ecsBindSkew)):
		id.Reason = fmt.Sprintf("ECS reports the container started at %s, after this process (%s)",
			c.StartedAt.UTC().Format(time.RFC3339), processStart.UTC().Format(time.RFC3339))
	default:
		id.BoundToProcess = true
	}
	return id
}

func getJSON(ctx context.Context, url string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

// watchECSIdentity reads the metadata until it is bound to this process (or
// is definitively unavailable), publishing each reading to /status. ECS can
// lag a container restart, so an unbound reading is retried for a while.
func (c *Controller) watchECSIdentity(ctx context.Context) {
	uri := c.opts.ECSMetadataURI
	if uri == "" {
		uri = os.Getenv(ecsMetadataEnv)
	}
	deadline := c.now().Add(c.ecsRetryFor)
	for {
		id := fetchECSIdentity(ctx, uri, c.startedAt, c.now())
		c.mu.Lock()
		c.ecs = id
		c.mu.Unlock()
		if uri == "" || id.BoundToProcess || !c.now().Before(deadline) {
			if uri != "" {
				logging.Infow(c.ctx, "ECS identity", "available", id.Available, "bound", id.BoundToProcess,
					"taskArn", id.TaskARN, "container", id.ContainerName, "restartCount", id.RestartCount,
					"reason", id.Reason)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.ecsRetryEvery):
		}
	}
}
