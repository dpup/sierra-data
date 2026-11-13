package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/dpup/prefab/logging"

	"github.com/sierragridteam/sierra-data/internal/config"

	meshrelay "github.com/sierragridteam/sierra-meshrelay/relay"
	"github.com/sierragridteam/sierra-meshrelay/relay/dynamostore"
)

// The relay's store is one of two meshrelay.Storage implementations, chosen
// by meshRelay.store (PF__MESH_RELAY__STORE):
//
//   - sqlite (default): meshRelay.dbPath, one exclusive file lock. Local
//     development, self-hosted relays and the legacy EFS deployment.
//   - dynamodb: meshRelay.dynamodb.table in the task's region, one instance
//     lease (a conditional-write item with a heartbeat). The hosted relay in
//     the Grid account; no file system at all.
//
// Both wait up to meshRelay.lockTimeout for the previous relay to let go,
// which is what a rolling deploy with minimum healthy 0 relies on. Only the
// DynamoDB lease can be lost while open (a successor took it after this
// relay stopped heartbeating): that is fatal, like the SQLite lock vanishing
// would be, and goes to the controller through onFatal.

const (
	storeSQLite   = "sqlite"
	storeDynamoDB = "dynamodb"
)

// storeKind validates meshRelay.store and what it needs. Checked at
// preparation, so a standby with a bad store setting fails before it is
// offered as a replacement.
func storeKind(c config.MeshRelayConfig) (string, error) {
	kind := strings.ToLower(strings.TrimSpace(c.Store))
	switch kind {
	case "":
		return storeSQLite, nil
	case storeSQLite:
		return storeSQLite, nil
	case storeDynamoDB:
		if strings.TrimSpace(c.DynamoDB.Table) == "" {
			return "", fmt.Errorf("meshRelay.store is dynamodb but meshRelay.dynamodb.table " +
				"(PF__MESH_RELAY__DYNAMODB__TABLE) is empty")
		}
		if c.DynamoDB.Heartbeat > 0 && c.DynamoDB.StaleAfter > 0 && c.DynamoDB.StaleAfter < 2*c.DynamoDB.Heartbeat {
			return "", fmt.Errorf("meshRelay.dynamodb.staleAfter (%s) must be at least twice heartbeat (%s)",
				c.DynamoDB.StaleAfter, c.DynamoDB.Heartbeat)
		}
		return storeDynamoDB, nil
	default:
		return "", fmt.Errorf("meshRelay.store (PF__MESH_RELAY__STORE) must be %q or %q, not %q",
			storeSQLite, storeDynamoDB, c.Store)
	}
}

// storeLocation names the store for logs: the file, or the table.
func (a *relayApp) storeLocation() string {
	if a.storeKind == storeDynamoDB {
		return a.dynamo.Table
	}
	return a.dbPath
}

// openStore takes the store's single-writer lock and returns it. ctx carries
// the activation logger (the lock wait logs through it).
func (a *relayApp) openStore(ctx context.Context) (meshrelay.Storage, error) {
	if a.storeKind != storeDynamoDB {
		return meshrelay.OpenStore(ctx, a.dbPath, a.lockTimeout)
	}
	client, err := a.dynamoClient(ctx)
	if err != nil {
		return nil, err
	}
	return dynamostore.Open(ctx, dynamostore.Options{
		Client:      client,
		Table:       a.dynamo.Table,
		LockTimeout: a.lockTimeout,
		Heartbeat:   a.dynamo.Heartbeat,
		StaleAfter:  a.dynamo.StaleAfter,
		OnLost:      a.storeLost,
	})
}

// dynamoClient builds the DynamoDB client from the environment (AWS_REGION
// and the task role on ECS; a profile locally), with the optional endpoint
// override for DynamoDB Local.
func (a *relayApp) dynamoClient(ctx context.Context) (*dynamodb.Client, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	if cfg.Region == "" && a.dynamo.Endpoint == "" {
		return nil, fmt.Errorf("the DynamoDB store needs a region (AWS_REGION)")
	}
	return dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) {
		if a.dynamo.Endpoint != "" {
			o.BaseEndpoint = aws.String(a.dynamo.Endpoint)
			if o.Region == "" {
				o.Region = "us-west-2"
			}
		}
	}), nil
}

// storeLost is the DynamoDB store's OnLost: another relay holds the lease
// now and every call here returns ErrLeaseLost, so this process must stop
// serving. The controller's Fail (onFatal) exits it, as a stopped prefab
// server does; the successor is already serving.
func (a *relayApp) storeLost(err error) {
	logging.Errorw(a.ctx, "Relay store lease lost; stopping", "error", err)
	if a.onFatal != nil {
		a.onFatal(fmt.Errorf("relay store lease lost: %w", err))
	}
}
