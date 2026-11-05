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

// ECSIdentity is ECS's description of the task and container this process
// runs in, as /status reports it: raw metadata, refreshed for the life of the
// process, with nothing inferred from it.
//
// It does NOT identify the process. On Fargate an in-place container restart
// (the restart policy) keeps containerStartedAt, dockerId and containerArn
// unchanged; only restartCount advances (observed on a Fargate restart test,
// 2026-11-04). An earlier version claimed a
// "boundToProcess" from container start time and stopped reading; that
// accepted a predecessor's metadata. The process generation is instanceId,
// which the expectedInstanceId guard enforces. What this adds is which task
// and container answered, and ECS's restart count at FetchedAt: meaningful
// only compared with a predecessor the coordinator recorded, and never on its
// own grounds for activation.
type ECSIdentity struct {
	// Available is true once a reading has succeeded. Reason says why not.
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`

	TaskARN          string `json:"taskArn,omitempty"`
	Cluster          string `json:"cluster,omitempty"`
	Family           string `json:"family,omitempty"`
	Revision         string `json:"revision,omitempty"`
	AvailabilityZone string `json:"availabilityZone,omitempty"`

	ContainerName string `json:"containerName,omitempty"`
	ContainerARN  string `json:"containerArn,omitempty"`
	DockerID      string `json:"dockerId,omitempty"`
	// ContainerStartedAt is ECS's container start time. It does NOT advance
	// on an in-place restart, so it says nothing about this process.
	ContainerStartedAt *time.Time `json:"containerStartedAt,omitempty"`
	// RestartCount is how many times ECS has restarted this container under
	// its restart policy, as of FetchedAt. Null when ECS does not report it
	// (no restart policy): unknown, never zero.
	RestartCount *int `json:"restartCount"`

	// FetchedAt is when the values above were read. LastAttemptAt and
	// LastError describe the most recent refresh; on an error the values are
	// the last good reading, as old as FetchedAt.
	FetchedAt     *time.Time `json:"fetchedAt,omitempty"`
	LastAttemptAt *time.Time `json:"lastAttemptAt,omitempty"`
	LastError     string     `json:"lastError,omitempty"`
}

// ecsMetadataEnv is set by the ECS agent in every container.
const ecsMetadataEnv = "ECS_CONTAINER_METADATA_URI_V4"

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

// readECSIdentity reads the metadata endpoint once.
func readECSIdentity(ctx context.Context, uri string, now time.Time) (ECSIdentity, error) {
	var c ecsContainerDoc
	if err := getJSON(ctx, uri, &c); err != nil {
		return ECSIdentity{}, fmt.Errorf("container metadata: %w", err)
	}
	var t ecsTaskDoc
	if err := getJSON(ctx, uri+"/task", &t); err != nil {
		return ECSIdentity{}, fmt.Errorf("task metadata: %w", err)
	}
	return ECSIdentity{
		Available: true, TaskARN: t.TaskARN, Cluster: t.Cluster, Family: t.Family,
		Revision: t.Revision, AvailabilityZone: t.AvailabilityZone,
		ContainerName: c.Name, ContainerARN: c.ContainerARN, DockerID: c.DockerID,
		ContainerStartedAt: c.StartedAt, RestartCount: c.RestartCount,
		FetchedAt: &now, LastAttemptAt: &now,
	}, nil
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

// watchECSIdentity keeps /status.ecs current for the life of the process. It
// never stops on a "good enough" reading: ECS can lag a container restart,
// and the restart count is the one field that moves.
func (c *Controller) watchECSIdentity(ctx context.Context) {
	uri := c.opts.ECSMetadataURI
	if uri == "" {
		uri = os.Getenv(ecsMetadataEnv)
	}
	if uri == "" {
		c.mu.Lock()
		c.ecs = ECSIdentity{Reason: ecsMetadataEnv + " is not set (not running on ECS)"}
		c.mu.Unlock()
		return
	}
	for {
		now := c.now()
		id, err := readECSIdentity(ctx, uri, now)
		c.mu.Lock()
		prev := c.ecs
		switch {
		case err == nil:
			c.ecs = id
		case prev.Available:
			// Keep the last good reading; say it is stale and why.
			prev.LastAttemptAt, prev.LastError = &now, err.Error()
			c.ecs = prev
		default:
			c.ecs = ECSIdentity{Reason: err.Error(), LastAttemptAt: &now, LastError: err.Error()}
		}
		c.mu.Unlock()
		logECSChange(c.ctx, prev, id, err)

		select {
		case <-ctx.Done():
			return
		case <-time.After(c.ecsRefreshEvery):
		}
	}
}

// logECSChange logs the first reading, any change of identity or restart
// count, and the transitions into and out of failing. Not every refresh.
func logECSChange(ctx context.Context, prev, cur ECSIdentity, err error) {
	switch {
	case err != nil && prev.LastError == "":
		logging.Warnw(ctx, "ECS metadata read failed", "error", err, "haveLastGood", prev.Available)
	case err != nil:
		// still failing; already logged
	case !prev.Available || prev.TaskARN != cur.TaskARN || prev.ContainerARN != cur.ContainerARN ||
		prev.DockerID != cur.DockerID || !sameCount(prev.RestartCount, cur.RestartCount):
		logging.Infow(ctx, "ECS identity", "taskArn", cur.TaskARN, "container", cur.ContainerName,
			"dockerId", cur.DockerID, "restartCount", cur.RestartCount)
	case prev.LastError != "":
		logging.Infow(ctx, "ECS metadata read recovered", "taskArn", cur.TaskARN)
	}
}

func sameCount(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
