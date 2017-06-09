// Copyright 2015 Sorint.lab
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied
// See the License for the specific language governing permissions and
// limitations under the License.

package integration

import (
	"fmt"
	"io/ioutil"
	"os"
	"testing"
	"time"

	"github.com/sorintlab/stolon/common"
	"github.com/sorintlab/stolon/pkg/cluster"

	"github.com/satori/go.uuid"
)

func TestInitStandbyCluster(t *testing.T) {
	t.Parallel()

	dir, err := ioutil.TempDir("", "stolon")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	defer os.RemoveAll(dir)

	// Setup a remote stolon cluster (with just one keeper and one sentinel)
	ptstore := setupStore(t, dir)
	defer ptstore.Stop()
	primaryStoreEndpoints := fmt.Sprintf("%s:%s", ptstore.listenAddress, ptstore.port)

	initialClusterSpec := &cluster.ClusterSpec{
		InitMode:           cluster.ClusterInitModeP(cluster.ClusterInitModeNew),
		SleepInterval:      &cluster.Duration{Duration: 2 * time.Second},
		FailInterval:       &cluster.Duration{Duration: 5 * time.Second},
		ConvergenceTimeout: &cluster.Duration{Duration: 30 * time.Second},
	}
	initialClusterSpecFile, err := writeClusterSpec(dir, initialClusterSpec)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	primaryClusterName := uuid.NewV4().String()
	pts, err := NewTestSentinel(t, dir, primaryClusterName, ptstore.storeBackend, primaryStoreEndpoints, fmt.Sprintf("--initial-cluster-spec=%s", initialClusterSpecFile))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if err := pts.Start(); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	defer pts.Stop()
	ptk, err := NewTestKeeper(t, dir, primaryClusterName, pgSUUsername, pgSUPassword, pgReplUsername, pgReplPassword, ptstore.storeBackend, primaryStoreEndpoints)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	if err := ptk.Start(); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	defer ptk.Stop()

	if err := ptk.WaitDBUp(60 * time.Second); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	t.Logf("primary database is up")

	if err := populate(t, ptk); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if err := write(t, ptk, 1, 1); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	// setup a standby cluster
	tstore := setupStore(t, dir)
	defer tstore.Stop()

	storeEndpoints := fmt.Sprintf("%s:%s", tstore.listenAddress, tstore.port)

	clusterName := uuid.NewV4().String()

	pgpass, err := ioutil.TempFile(dir, "pgpass")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	pgpass.WriteString(fmt.Sprintf("%s:%s:*:%s:%s\n", ptk.pgListenAddress, ptk.pgPort, ptk.pgReplUsername, ptk.pgReplPassword))
	pgpass.Close()

	initialClusterSpec = &cluster.ClusterSpec{
		InitMode:           cluster.ClusterInitModeP(cluster.ClusterInitModePITR),
		Role:               cluster.ClusterRoleP(cluster.ClusterRoleStandby),
		SleepInterval:      &cluster.Duration{Duration: 2 * time.Second},
		FailInterval:       &cluster.Duration{Duration: 5 * time.Second},
		ConvergenceTimeout: &cluster.Duration{Duration: 30 * time.Second},
		MaxStandbyLag:      cluster.Uint32P(50 * 1024), // limit lag to 50kiB
		PGParameters:       defaultPGParameters,
		PITRConfig: &cluster.PITRConfig{
			DataRestoreCommand: fmt.Sprintf("PGPASSFILE=%s pg_basebackup -D %%d -h %s -p %s -U %s", pgpass.Name(), ptk.pgListenAddress, ptk.pgPort, ptk.pgReplUsername),
		},
		StandbySettings: &cluster.StandbySettings{
			PrimaryConninfo: fmt.Sprintf("sslmode=disable host=%s port=%s user=%s password=%s", ptk.pgListenAddress, ptk.pgPort, ptk.pgReplUsername, ptk.pgReplPassword),
		},
	}
	initialClusterSpecFile, err = writeClusterSpec(dir, initialClusterSpec)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	ts, err := NewTestSentinel(t, dir, clusterName, tstore.storeBackend, storeEndpoints, fmt.Sprintf("--initial-cluster-spec=%s", initialClusterSpecFile))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if err := ts.Start(); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	defer ts.Stop()
	tk, err := NewTestKeeper(t, dir, clusterName, pgSUUsername, pgSUPassword, pgReplUsername, pgReplPassword, tstore.storeBackend, storeEndpoints)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	if err := tk.Start(); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	defer tk.Stop()

	if err := tk.WaitDBUp(60 * time.Second); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	t.Logf("standby cluster master database is up")

	if err := waitLines(t, tk, 1, 10*time.Second); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	// Check that the standby cluster master keeper is syncing
	if err := write(t, ptk, 2, 2); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if err := waitLines(t, tk, 2, 10*time.Second); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
}

func TestPromoteStandbyCluster(t *testing.T) {
	t.Parallel()

	dir, err := ioutil.TempDir("", "stolon")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	defer os.RemoveAll(dir)

	// Setup a remote stolon cluster (with just one keeper and one sentinel)
	ptstore := setupStore(t, dir)
	defer ptstore.Stop()
	primaryStoreEndpoints := fmt.Sprintf("%s:%s", ptstore.listenAddress, ptstore.port)

	initialClusterSpec := &cluster.ClusterSpec{
		InitMode:           cluster.ClusterInitModeP(cluster.ClusterInitModeNew),
		SleepInterval:      &cluster.Duration{Duration: 2 * time.Second},
		FailInterval:       &cluster.Duration{Duration: 5 * time.Second},
		ConvergenceTimeout: &cluster.Duration{Duration: 30 * time.Second},
	}
	initialClusterSpecFile, err := writeClusterSpec(dir, initialClusterSpec)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	primaryClusterName := uuid.NewV4().String()
	pts, err := NewTestSentinel(t, dir, primaryClusterName, ptstore.storeBackend, primaryStoreEndpoints, fmt.Sprintf("--initial-cluster-spec=%s", initialClusterSpecFile))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if err := pts.Start(); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	defer pts.Stop()
	ptk, err := NewTestKeeper(t, dir, primaryClusterName, pgSUUsername, pgSUPassword, pgReplUsername, pgReplPassword, ptstore.storeBackend, primaryStoreEndpoints)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	if err := ptk.Start(); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	defer ptk.Stop()

	if err := ptk.WaitDBUp(60 * time.Second); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	t.Logf("primary database is up")

	if err := populate(t, ptk); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if err := write(t, ptk, 1, 1); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	// setup a standby cluster
	tstore := setupStore(t, dir)
	defer tstore.Stop()

	storeEndpoints := fmt.Sprintf("%s:%s", tstore.listenAddress, tstore.port)

	clusterName := uuid.NewV4().String()

	pgpass, err := ioutil.TempFile(dir, "pgpass")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	pgpass.WriteString(fmt.Sprintf("%s:%s:*:%s:%s\n", ptk.pgListenAddress, ptk.pgPort, ptk.pgReplUsername, ptk.pgReplPassword))
	pgpass.Close()

	initialClusterSpec = &cluster.ClusterSpec{
		InitMode:           cluster.ClusterInitModeP(cluster.ClusterInitModePITR),
		Role:               cluster.ClusterRoleP(cluster.ClusterRoleStandby),
		SleepInterval:      &cluster.Duration{Duration: 2 * time.Second},
		FailInterval:       &cluster.Duration{Duration: 5 * time.Second},
		ConvergenceTimeout: &cluster.Duration{Duration: 30 * time.Second},
		MaxStandbyLag:      cluster.Uint32P(50 * 1024), // limit lag to 50kiB
		PGParameters:       defaultPGParameters,
		PITRConfig: &cluster.PITRConfig{
			DataRestoreCommand: fmt.Sprintf("PGPASSFILE=%s pg_basebackup -D %%d -h %s -p %s -U %s", pgpass.Name(), ptk.pgListenAddress, ptk.pgPort, ptk.pgReplUsername),
		},
		StandbySettings: &cluster.StandbySettings{
			PrimaryConninfo: fmt.Sprintf("sslmode=disable host=%s port=%s user=%s password=%s", ptk.pgListenAddress, ptk.pgPort, ptk.pgReplUsername, ptk.pgReplPassword),
		},
	}
	initialClusterSpecFile, err = writeClusterSpec(dir, initialClusterSpec)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	ts, err := NewTestSentinel(t, dir, clusterName, tstore.storeBackend, storeEndpoints, fmt.Sprintf("--initial-cluster-spec=%s", initialClusterSpecFile))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if err := ts.Start(); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	defer ts.Stop()
	tk, err := NewTestKeeper(t, dir, clusterName, pgSUUsername, pgSUPassword, pgReplUsername, pgReplPassword, tstore.storeBackend, storeEndpoints)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	if err := tk.Start(); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	defer tk.Stop()

	if err := tk.WaitDBUp(60 * time.Second); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	t.Logf("standby cluster master database is up")

	if err := waitLines(t, tk, 1, 10*time.Second); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	// Check that the standby cluster master keeper is syncing
	if err := write(t, ptk, 2, 2); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if err := waitLines(t, tk, 2, 10*time.Second); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	// promote the standby cluster to a primary cluster
	err = StolonCtl(clusterName, tstore.storeBackend, storeEndpoints, "promote", "-y")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	// check that the cluster master has been promoted to a primary
	if err := tk.WaitDBRole(common.RoleMaster, nil, 30*time.Second); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
}
