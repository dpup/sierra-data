package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sierragridteam/sierra-data/internal/config"
	"github.com/sierragridteam/sierra-data/internal/handoff"

	meshrelay "github.com/sierragridteam/sierra-meshrelay/relay"
	"github.com/sierragridteam/sierra-meshrelay/relay/dynamostore"
)

func TestStoreKind(t *testing.T) {
	dyn := func(table string, hb, stale time.Duration) config.MeshRelayConfig {
		return config.MeshRelayConfig{Store: "dynamodb",
			DynamoDB: config.MeshRelayDynamoDBConfig{Table: table, Heartbeat: hb, StaleAfter: stale}}
	}
	for _, tc := range []struct {
		name string
		cfg  config.MeshRelayConfig
		want string
		err  string
	}{
		{"default is sqlite", config.MeshRelayConfig{}, storeSQLite, ""},
		{"sqlite", config.MeshRelayConfig{Store: "sqlite"}, storeSQLite, ""},
		{"case and space", config.MeshRelayConfig{Store: " DynamoDB "}, "", "dynamodb.table"},
		{"dynamodb", dyn("sierra-relay", 0, 0), storeDynamoDB, ""},
		{"dynamodb needs a table", dyn(" ", 0, 0), "", "PF__MESH_RELAY__DYNAMODB__TABLE"},
		{"stale must cover two heartbeats", dyn("t", 10*time.Second, 15*time.Second), "", "twice heartbeat"},
		{"stale ok", dyn("t", 10*time.Second, 45*time.Second), storeDynamoDB, ""},
		{"unknown", config.MeshRelayConfig{Store: "efs"}, "", `not "efs"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := storeKind(tc.cfg)
			if tc.err != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// A bad store setting stops preparation: a standby must not be offered as a
// replacement and then fail to activate.
func TestPrepareRelayRefusesABadStore(t *testing.T) {
	_, err := prepareRelay(testCtx(), &config.MeshRelayConfig{Store: "dynamodb"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PF__MESH_RELAY__DYNAMODB__TABLE")
}

// The lost lease reaches the controller as a fatal error, like a stopped
// server does.
func TestStoreLostIsFatal(t *testing.T) {
	var got error
	a := &relayApp{ctx: testCtx(), onFatal: func(err error) { got = err }}
	a.storeLost(dynamostore.ErrLeaseLost)
	require.Error(t, got)
	assert.True(t, errors.Is(got, dynamostore.ErrLeaseLost))
	a.onFatal = nil
	a.storeLost(dynamostore.ErrLeaseLost) // never panics without a controller
}

// dynamoEndpointEnv opts in to the DynamoDB tests: the same variable as
// sierra-meshrelay's own (`make dynamo-up` there starts DynamoDB Local on
// http://127.0.0.1:8000). Without it they skip, so `make test` stays
// offline.
const dynamoEndpointEnv = "MESHRELAY_TEST_DYNAMO_ENDPOINT"

func dynamoEndpoint(t *testing.T) string {
	t.Helper()
	ep := os.Getenv(dynamoEndpointEnv)
	if ep == "" {
		t.Skipf("set %s to a DynamoDB Local endpoint to run the DynamoDB relay tests", dynamoEndpointEnv)
	}
	return ep
}

// newDynamoTable creates a table on DynamoDB Local, deleted after the test.
func newDynamoTable(t *testing.T, ep string) string {
	t.Helper()
	db := dynamodb.New(dynamodb.Options{Region: "us-west-2", BaseEndpoint: aws.String(ep),
		Credentials: credentials.NewStaticCredentialsProvider("local", "local", "")})
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "sierra-relay-test-" + hex.EncodeToString(b)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	require.NoError(t, dynamostore.CreateTable(ctx, db, name))
	t.Cleanup(func() {
		_, _ = db.DeleteTable(context.Background(), &dynamodb.DeleteTableInput{TableName: aws.String(name)})
	})
	return name
}

// The REAL relayApp on the DynamoDB store under the handoff controller: the
// first activation finds the instance lease held by another relay and fails
// cleanly back to STANDBY; once that relay closes, activation takes the lease
// and serves; quiescence releases it so the next relay opens at once.
func TestRelayHandoffLifecycleOnDynamoDB(t *testing.T) {
	ep := dynamoEndpoint(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "local")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "local")
	t.Setenv("AWS_REGION", "us-west-2")
	table := newDynamoTable(t, ep)
	port := freePort(t)
	app, err := prepareRelay(testCtx(), &config.MeshRelayConfig{
		Store: "dynamodb", DynamoDB: config.MeshRelayDynamoDBConfig{Table: table, Endpoint: ep},
		Port: port, LockTimeout: 600 * time.Millisecond,
		Control: config.ControlConfig{DrainTimeout: 500 * time.Millisecond, WorkerStopTimeout: 5 * time.Second},
	})
	require.NoError(t, err)
	assert.Equal(t, storeDynamoDB, app.storeKind)
	assert.Equal(t, table, app.storeLocation())
	h := runController(t, app, true)

	client, err := app.dynamoClient(testCtx())
	require.NoError(t, err)
	owner, err := dynamostore.Open(testCtx(), dynamostore.Options{Client: client, Table: table, Owner: "other-relay",
		LockTimeout: time.Second})
	require.NoError(t, err)

	h.command("/activate", "it-activate-1")
	st := h.waitSettled()
	assert.Equal(t, handoff.StateStandby, st.State, "a held lease fails the activation: %+v", st.Operation)
	assert.False(t, st.DatabaseHeld)
	assert.Equal(t, "failed", st.Operation.Status)
	assert.Contains(t, st.Operation.Error, table)

	require.NoError(t, owner.Close())
	h.command("/activate", "it-activate-2")
	st = h.waitSettled()
	require.Equal(t, handoff.StateActive, st.State, "activation failed: %+v", st.Operation)
	assert.Equal(t, http.StatusOK, httpCode(t, port, meshrelay.PathPrefix+"/healthz"))
	assert.Equal(t, http.StatusOK, httpCode(t, port, meshrelay.PathPrefix+"/api/status"))

	h.command("/quiesce", "it-release")
	st = h.waitSettled()
	require.Equal(t, handoff.StateQuiesced, st.State, "quiescence failed: %+v", st.Operation)
	assert.Equal(t, http.StatusServiceUnavailable, httpCode(t, port, meshrelay.PathPrefix+"/healthz"))
	next, err := dynamostore.Open(testCtx(), dynamostore.Options{Client: client, Table: table, Owner: "next-relay",
		LockTimeout: 100 * time.Millisecond})
	require.NoError(t, err, "released: the next relay takes the lease at once")
	require.NoError(t, next.Close())
	assert.Equal(t, 0, h.terminate())
}
